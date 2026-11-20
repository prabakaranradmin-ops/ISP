//go:build integration

// Payment-gateway order and callback log — FR-PAY-001.
//
// The requirement's phrase is "whether or not it credits a wallet", so
// the property under test is coverage: every exit path of the webhook
// writes a row, including the ones that deliberately do nothing. A path
// that silently records nothing is the failure here, and it is invisible
// from the outside — the response is the same either way.
package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

type recordingGatewayLog struct {
	mu     sync.Mutex
	events []billing.GatewayEvent
	err    error
}

func (r *recordingGatewayLog) RecordGatewayEvent(_ context.Context, e billing.GatewayEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return r.err
}

func (r *recordingGatewayLog) ListGatewayEvents(context.Context, billing.GatewayLogFilter) ([]billing.GatewayLogEntry, error) {
	return nil, nil
}

func (r *recordingGatewayLog) SummariseGatewayEvents(context.Context, billing.GatewayLogFilter) ([]billing.GatewayStatusCount, error) {
	return nil, nil
}

func (r *recordingGatewayLog) all() []billing.GatewayEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]billing.GatewayEvent(nil), r.events...)
}

const testWebhookSecret = "whsec_test_secret_value"

// signedWebhook posts a body with a valid Razorpay signature.
func signedWebhook(t *testing.T, mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	mac.Write([]byte(body))
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest("POST", "/webhooks/razorpay", strings.NewReader(body))
	req.Header.Set("X-Razorpay-Signature", sig)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func gatewayHarness(t *testing.T, logStore api.GatewayLogStore, secret string) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{},
		Wallet:                billing.NewWalletService(&stubWallet{}),
		GatewayLog:            logStore,
		RazorpayWebhookSecret: secret,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

// Each exit path, and what it must record.
func TestFR_PAY_001_EveryWebhookExitPathIsRecorded(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		secret     string
		signed     bool
		wantStatus string
	}{
		{
			name:   "an authentic capture that credits a wallet",
			body:   `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_ok","amount":118000,"notes":{"subscriber_id":"1"}}}}}`,
			secret: testWebhookSecret, signed: true,
			wantStatus: billing.GatewayCredited,
		},
		{
			name:   "an event this system does not act on",
			body:   `{"event":"payment.failed","payload":{"payment":{"entity":{"id":"pay_failed"}}}}`,
			secret: testWebhookSecret, signed: true,
			wantStatus: billing.GatewayIgnored,
		},
		{
			name:   "a capture naming no subscriber",
			body:   `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_orphan","amount":50000,"notes":{}}}}}`,
			secret: testWebhookSecret, signed: true,
			wantStatus: billing.GatewayUnmatched,
		},
		{
			name:   "an unreadable payload",
			body:   `not json at all`,
			secret: testWebhookSecret, signed: true,
			wantStatus: billing.GatewayRejected,
		},
		{
			name:   "a bad signature",
			body:   `{"event":"payment.captured"}`,
			secret: testWebhookSecret, signed: false,
			wantStatus: billing.GatewayRejected,
		},
		{
			name:   "a callback with no webhook secret configured",
			body:   `{"event":"payment.captured"}`,
			secret: "", signed: false,
			wantStatus: billing.GatewayUnconfigured,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingGatewayLog{}
			mux := gatewayHarness(t, store, tc.secret)

			if tc.signed {
				signedWebhook(t, mux, tc.body)
			} else {
				req := httptest.NewRequest("POST", "/webhooks/razorpay", strings.NewReader(tc.body))
				req.Header.Set("X-Razorpay-Signature", "deadbeef")
				mux.ServeHTTP(httptest.NewRecorder(), req)
			}

			got := store.all()
			if len(got) != 1 {
				t.Fatalf("want exactly 1 recorded event, got %d: %+v", len(got), got)
			}
			if got[0].Status != tc.wantStatus {
				t.Errorf("recorded status %q, want %q", got[0].Status, tc.wantStatus)
			}
			if got[0].Kind != billing.GatewayKindCallback {
				t.Errorf("recorded kind %q", got[0].Kind)
			}
			if got[0].Gateway == "" {
				t.Error("the event names no gateway")
			}
		})
	}
}

// Nothing recorded may carry the signature, the secret or the raw body.
// Authenticity is decided before anything is written, so keeping the
// signature would preserve a credential-shaped value for no gain.
func TestFR_PAY_001_NothingRecordedCarriesTheSignatureOrSecret(t *testing.T) {
	store := &recordingGatewayLog{}
	mux := gatewayHarness(t, store, testWebhookSecret)
	body := `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_ok","amount":118000,"notes":{"subscriber_id":"1"}}}}}`
	signedWebhook(t, mux, body)

	for _, e := range store.all() {
		blob := strings.Join([]string{
			e.Gateway, e.Kind, e.Event, e.GatewayOrderID, e.GatewayPaymentID, e.Status, e.Detail,
		}, "\x00")
		if strings.Contains(blob, testWebhookSecret) {
			t.Error("a recorded event carries the webhook secret")
		}
		// The raw body would bring the whole payload along with it.
		if strings.Contains(blob, "payload") || strings.Contains(blob, "notes") {
			t.Errorf("a recorded event looks like it carries the raw body: %q", e.Detail)
		}
	}
}

// A recording failure must not change what the webhook does. A payment
// that credited a wallet has credited it whether or not its log row was
// written, and failing the callback would make the gateway retry a credit
// that already happened.
func TestFR_PAY_001_ARecordingFailureDoesNotChangeTheResponse(t *testing.T) {
	body := `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_ok","amount":118000,"notes":{"subscriber_id":"1"}}}}}`

	ok := signedWebhook(t, gatewayHarness(t, &recordingGatewayLog{}, testWebhookSecret), body)
	broken := signedWebhook(t, gatewayHarness(t,
		&recordingGatewayLog{err: context.DeadlineExceeded}, testWebhookSecret), body)

	if ok.Code != broken.Code {
		t.Errorf("a failing log changed the response: %d with a working log, %d with a broken one",
			ok.Code, broken.Code)
	}
	if ok.Code != http.StatusOK {
		t.Errorf("the happy path answered %d", ok.Code)
	}
}

// With no log wired at all the webhook behaves exactly as before.
func TestFR_PAY_001_NoLogWiredIsSafe(t *testing.T) {
	mux := gatewayHarness(t, nil, testWebhookSecret)
	body := `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_ok","amount":118000,"notes":{"subscriber_id":"1"}}}}}`
	if rec := signedWebhook(t, mux, body); rec.Code != http.StatusOK {
		t.Errorf("got %d with no gateway log configured", rec.Code)
	}
}

// The amount is recorded in rupees, not the paise the gateway sends.
func TestFR_PAY_001_AmountsAreRecordedInRupees(t *testing.T) {
	store := &recordingGatewayLog{}
	mux := gatewayHarness(t, store, testWebhookSecret)
	// 118000 paise = 1180.00 rupees.
	signedWebhook(t, mux, `{"event":"payment.captured","payload":{"payment":{"entity":{"id":"pay_ok","amount":118000,"notes":{"subscriber_id":"1"}}}}}`)

	got := store.all()
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	if got[0].Amount.StringFixed(2) != "1180.00" {
		t.Errorf("recorded amount %s, want 1180.00 — paise leaked through", got[0].Amount.StringFixed(2))
	}
}
