package alerting

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// captured records what a stub PagerDuty received, so a test can assert the
// wire format rather than only that a request was made.
type captured struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (c *captured) add(b map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bodies = append(c.bodies, b)
}

func (c *captured) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// stubPagerDuty stands in for the Events API, answering with status.
func stubPagerDuty(t *testing.T, status int, got *captured) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("PagerDuty received a body that is not valid JSON: %v", err)
		}
		got.add(parsed)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, status int, got *captured) *PagerDuty {
	t.Helper()
	p := NewPagerDuty("test-routing-key", "test-host")
	p.SetURL(stubPagerDuty(t, status, got).URL)
	return p
}

// TestPagerDuty_SendsTheEventsV2Shape pins the wire format. Getting this
// wrong is not a compile error and not a test failure anywhere else — it is
// a 400 from PagerDuty at 3am, on the one request that mattered.
func TestPagerDuty_SendsTheEventsV2Shape(t *testing.T) {
	got := &captured{}
	p := newTestClient(t, http.StatusAccepted, got)

	p.Trigger("dead_letter_queue_non_empty", 7)

	if got.count() != 1 {
		t.Fatalf("want 1 request, got %d", got.count())
	}
	body := got.bodies[0]

	if body["routing_key"] != "test-routing-key" {
		t.Errorf("routing_key: got %v", body["routing_key"])
	}
	if body["event_action"] != "trigger" {
		t.Errorf("event_action: got %v", body["event_action"])
	}
	// The dedup key is what stops PagerDuty opening a new incident per
	// alert. Without it, the dead-letter monitor's own rate limiting would
	// be the only thing between a stuck queue and a flooded on-call rota.
	dedup, _ := body["dedup_key"].(string)
	if !strings.Contains(dedup, "dead_letter_queue_non_empty") || !strings.Contains(dedup, "test-host") {
		t.Errorf("dedup_key should identify both host and event, got %q", dedup)
	}

	payload, ok := body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload missing or not an object: %v", body["payload"])
	}
	if payload["source"] != "test-host" {
		t.Errorf("source: got %v — the incident must say which box this is", payload["source"])
	}
	if payload["severity"] != "error" {
		t.Errorf("severity: got %v", payload["severity"])
	}
	if s, _ := payload["summary"].(string); !strings.Contains(s, "dead_letter_queue_non_empty") {
		t.Errorf("summary does not name the event: %q", s)
	}
	// The detail travels verbatim so the incident shows the count that
	// triggered it, rather than sending someone to correlate against logs.
	if payload["custom_details"] != float64(7) {
		t.Errorf("custom_details: got %v, want the original detail (7)", payload["custom_details"])
	}
	if _, err := time.Parse(time.RFC3339, payload["timestamp"].(string)); err != nil {
		t.Errorf("timestamp is not RFC3339: %v", err)
	}
}

// TestPagerDuty_NoRoutingKeyIsSilent — an unconfigured client must not
// attempt delivery. Trigger has no error return, so a stray request here
// would be an unexplained outbound call from a deployment that never asked
// for one.
func TestPagerDuty_NoRoutingKeyIsSilent(t *testing.T) {
	got := &captured{}
	p := NewPagerDuty("", "test-host")
	p.SetURL(stubPagerDuty(t, http.StatusAccepted, got).URL)

	p.Trigger("anything", 1)

	if got.count() != 0 {
		t.Errorf("a client with no routing key sent %d requests", got.count())
	}
}

// TestPagerDuty_RejectionDoesNotPanic — a revoked routing key returns 4xx
// forever. Trigger cannot report that to its caller, so the requirement is
// simply that it survives and records the failure rather than taking the
// monitor down with it.
func TestPagerDuty_RejectionDoesNotPanic(t *testing.T) {
	got := &captured{}
	p := newTestClient(t, http.StatusBadRequest, got)

	p.Trigger("dead_letter_queue_non_empty", 3) // must not panic

	if got.count() != 1 {
		t.Errorf("want the request attempted once, got %d", got.count())
	}
}

// TestPagerDuty_NilClientIsSafe — the daemon builds this conditionally, so a
// nil receiver is reachable if the wiring is ever reordered.
func TestPagerDuty_NilClientIsSafe(t *testing.T) {
	var p *PagerDuty
	p.Trigger("anything", 1) // must not panic
}
