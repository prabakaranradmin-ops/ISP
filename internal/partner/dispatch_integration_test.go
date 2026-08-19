//go:build integration

package partner_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
)

// End-to-end webhook delivery — FR-API-002..003 | MDS §4.22.
//
// These run a real HTTP receiver that verifies the signature the way a partner
// would, using the same exported VerifySignature the integration docs hand
// out. Testing against our own reference implementation is what stops the
// documentation and the sender drifting apart.

type recordedCall struct {
	Body      []byte
	Signature string
	EventID   string
	EventType string
}

// stubStore captures what the sender recorded.
type stubStore struct {
	mu       sync.Mutex
	attempts []stubAttempt
}

type stubAttempt struct {
	EndpointID int
	Status     string
	RespStatus *int
	LastError  string
}

func (s *stubStore) SubscribersFor(context.Context, string) ([]partner.EndpointSecret, error) {
	return nil, nil
}

func (s *stubStore) RecordDeliveryAttempt(_ context.Context, endpointID int, _ partner.Event,
	status string, respStatus *int, _, lastError string, _ *time.Time,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = append(s.attempts, stubAttempt{endpointID, status, respStatus, lastError})
	return nil
}

func (s *stubStore) last() stubAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.attempts) == 0 {
		return stubAttempt{}
	}
	return s.attempts[len(s.attempts)-1]
}

// plainDecryptor stands in for the AES keystore; the secret round trip itself
// is covered by pkg/crypto's own tests.
type plainDecryptor struct{ secret string }

func (d plainDecryptor) Decrypt(string) (string, error) { return d.secret, nil }

func newTask(t *testing.T, endpointID int, url string) *jobqueue.Task {
	t.Helper()
	ev, err := partner.NewEvent(partner.EventTicketCreated, 42, time.Now())
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	payload, err := json.Marshal(partner.TaskPayload{
		EndpointID:      endpointID,
		URL:             url,
		SecretEncrypted: "encrypted-placeholder",
		Event:           ev,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return jobqueue.NewTask(partner.TaskTypeWebhook, payload)
}

func TestFR_API_002_DeliverySignsWhatThePartnerVerifies(t *testing.T) {
	const secret = "whsec_integration"
	var got recordedCall

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = recordedCall{
			Body:      body,
			Signature: r.Header.Get(partner.HeaderSignature),
			EventID:   r.Header.Get(partner.HeaderEventID),
			EventType: r.Header.Get(partner.HeaderEventType),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	store := &stubStore{}
	sender := partner.NewSender(store, plainDecryptor{secret})

	// httptest binds to 127.0.0.1, which the SSRF guard blocks by design — so
	// the sender is exercised through its exported handler with the guard's
	// dialler swapped for a plain one. The guard itself is covered separately
	// in TestFR_API_002_DialerBlocksAfterResolution; conflating the two would
	// mean testing neither properly.
	partner.SetHTTPClientForTest(sender, srv.Client())

	if err := sender.ProcessTask(context.Background(), newTask(t, 1, srv.URL)); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if got.Signature == "" {
		t.Fatal("every delivery must carry a signature header")
	}
	if got.EventType != partner.EventTicketCreated {
		t.Errorf("event type header: got %q", got.EventType)
	}
	if got.EventID == "" {
		t.Error("the event id header is the partner's idempotency key and must be present")
	}

	// Verified exactly as a partner would.
	if !partner.VerifySignature(secret, got.Signature, got.Body, time.Now(), 5*time.Minute) {
		t.Error("the signature we send must verify under the secret we gave the partner")
	}
	if partner.VerifySignature("whsec_other", got.Signature, got.Body, time.Now(), 5*time.Minute) {
		t.Error("the signature must not verify under a different secret")
	}

	// The payload is thin: identifiers only, no PII.
	var payload map[string]any
	if err := json.Unmarshal(got.Body, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	for _, forbidden := range []string{"username", "mobile_number", "email", "caf_number", "password"} {
		if _, present := payload[forbidden]; present {
			t.Errorf("thin payloads must not carry %q — it would put PII in webhook_deliveries "+
				"under DPDP retention", forbidden)
		}
	}
	for _, required := range []string{"event_id", "event_type", "entity_id", "occurred_at"} {
		if _, present := payload[required]; !present {
			t.Errorf("payload is missing %q", required)
		}
	}

	if a := store.last(); a.Status != partner.StatusDelivered {
		t.Errorf("a 2xx must be recorded as delivered, got %q", a.Status)
	}
}

func TestFR_API_003_FailureIsRecordedAndRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	store := &stubStore{}
	sender := partner.NewSender(store, plainDecryptor{"whsec_x"})
	partner.SetHTTPClientForTest(sender, srv.Client())

	err := sender.ProcessTask(context.Background(), newTask(t, 2, srv.URL))
	if err == nil {
		t.Fatal("a 5xx must return an error so Asynq retries it")
	}

	a := store.last()
	if a.Status != partner.StatusPending {
		t.Errorf("a retryable failure must stay pending, got %q — abandoning on the first "+
			"bad response would lose events during a partner's deploy", a.Status)
	}
	if a.RespStatus == nil || *a.RespStatus != http.StatusInternalServerError {
		t.Error("the partner's response status must be recorded for diagnosis")
	}
	if a.LastError == "" {
		t.Error("the failure reason must be recorded")
	}
}

// TestFR_API_003_4xxIsRetriedNotDropped covers a decision that is easy to get
// wrong: a partner returning 401 because their own auth broke is transient
// from our side, and giving up would silently lose the event.
func TestFR_API_003_4xxIsRetriedNotDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	store := &stubStore{}
	sender := partner.NewSender(store, plainDecryptor{"whsec_x"})
	partner.SetHTTPClientForTest(sender, srv.Client())

	if err := sender.ProcessTask(context.Background(), newTask(t, 3, srv.URL)); err == nil {
		t.Fatal("a 4xx must still retry")
	}
	if a := store.last(); a.Status != partner.StatusPending {
		t.Errorf("a 4xx must stay pending, got %q", a.Status)
	}
}

// TestFR_API_002_BlockedTargetIsAbandonedNotRetried is the SSRF path through
// the real sender: no amount of retrying makes a private address public, and
// retrying for a day would waste the queue on a permanent misconfiguration.
func TestFR_API_002_BlockedTargetIsAbandonedNotRetried(t *testing.T) {
	store := &stubStore{}
	sender := partner.NewSender(store, plainDecryptor{"whsec_x"})

	err := sender.ProcessTask(context.Background(), newTask(t, 4, "https://169.254.169.254/meta-data/"))
	if err == nil {
		t.Fatal("a delivery to cloud metadata must fail")
	}

	a := store.last()
	if a.Status != partner.StatusAbandoned {
		t.Errorf("a blocked target must be abandoned, not left pending, got %q — otherwise a "+
			"misconfigured endpoint occupies the queue for a day", a.Status)
	}
}
