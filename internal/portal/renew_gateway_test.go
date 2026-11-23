//go:build integration

package portal_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/shopspring/decimal"
)

// A subscriber who tries to pay and cannot must leave a trace (FR-PAY-001).
//
// This is the whole point of recording gateway orders: the subscriber's own
// account shows nothing, so "I tried to pay and it wouldn't let me" is
// otherwise unanswerable, and a run of failures is a gateway outage nobody
// would see until customers started calling.
//
// None of these three paths had a test. Renew sat at 50% covered and the
// uncovered half was every branch where something goes wrong — which is the
// half that matters for a money path.

type gwRecorder struct {
	mu     sync.Mutex
	events []billing.GatewayEvent
	err    error
}

func (g *gwRecorder) RecordGatewayEvent(_ context.Context, e billing.GatewayEvent) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.events = append(g.events, e)
	return g.err
}

func (g *gwRecorder) all() []billing.GatewayEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]billing.GatewayEvent(nil), g.events...)
}

// stubRazorpay returns whatever the test needs, including a failure.
type stubRazorpay struct {
	orderID, link string
	err           error
	calls         int
}

func (s *stubRazorpay) CreateOrder(_ context.Context, _ int, _ decimal.Decimal) (string, string, error) {
	s.calls++
	if s.err != nil {
		return "", "", s.err
	}
	return s.orderID, s.link, nil
}

// renewHarness builds a portal with a chosen gateway and logger.
func renewHarness(t *testing.T, rz portal.RazorpayOrderCreator, gw portal.GatewayLogger) (*http.ServeMux, string) {
	t.Helper()
	store := newITPortalStore(&itSubscriberSeed{
		ID: 1, Username: "payer@isp", Password: "portalpassword",
		PlanName: "Home 50M", Balance: decimal.RequireFromString("0.00"), Status: "active",
	})
	h := portal.NewHandler(store, store, store, store, rz, itPortalSecret)
	if gw != nil {
		h.SetGatewayLogger(gw)
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	// Through a real login, so the token is one the middleware accepts
	// rather than one this test minted to its own taste.
	p := &itPortal{mux: mux, store: store}
	return mux, p.login(t, "payer@isp", "portalpassword")
}

func renew(t *testing.T, mux *http.ServeMux, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/portal/renew", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestFR_PAY_001_NoGatewayConfiguredIsRecordedNotSilent(t *testing.T) {
	gw := &gwRecorder{}
	mux, tok := renewHarness(t, nil, gw) // no Razorpay at all

	rec := renew(t, mux, tok, `{"amount":"499.00"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when no gateway is configured, got %d — %s", rec.Code, rec.Body.String())
	}

	events := gw.all()
	if len(events) != 1 {
		t.Fatalf("the attempt must be recorded, got %d events", len(events))
	}
	if events[0].Status != billing.GatewayUnconfigured {
		t.Errorf("status = %q, want %q", events[0].Status, billing.GatewayUnconfigured)
	}
	if events[0].SubscriberID != 1 {
		t.Errorf("the event must name the subscriber who tried, got %d", events[0].SubscriberID)
	}
	if !events[0].Amount.Equal(decimal.RequireFromString("499.00")) {
		t.Errorf("the event must carry what they tried to pay, got %s", events[0].Amount)
	}
}

func TestFR_PAY_001_AGatewayErrorIsRecordedWithItsReason(t *testing.T) {
	rz := &stubRazorpay{err: errors.New("razorpay: 502 upstream")}
	gw := &gwRecorder{}
	mux, tok := renewHarness(t, rz, gw)

	rec := renew(t, mux, tok, `{"amount":"799.00"}`)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("want 502 when the gateway fails, got %d — %s", rec.Code, rec.Body.String())
	}
	// The subscriber is told it failed, not why: the upstream message is for
	// the operator's log, not the customer's screen.
	if strings.Contains(rec.Body.String(), "upstream") {
		t.Errorf("the gateway's own error leaked to the subscriber: %s", rec.Body.String())
	}

	events := gw.all()
	if len(events) != 1 {
		t.Fatalf("the failure must be recorded, got %d events", len(events))
	}
	if events[0].Status != billing.GatewayFailed {
		t.Errorf("status = %q, want %q", events[0].Status, billing.GatewayFailed)
	}
	// The operator needs the reason even though the subscriber does not.
	if !strings.Contains(events[0].Detail, "upstream") {
		t.Errorf("the recorded detail should carry the gateway's reason, got %q", events[0].Detail)
	}
}

func TestFR_PAY_001_ASuccessfulOrderIsRecordedWithItsOrderID(t *testing.T) {
	rz := &stubRazorpay{orderID: "order_ABC123", link: "https://rzp.example/pay/ABC123"}
	gw := &gwRecorder{}
	mux, tok := renewHarness(t, rz, gw)

	rec := renew(t, mux, tok, `{"amount":"599.00"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for a created order, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "order_ABC123") {
		t.Errorf("the response should carry the order id, got %s", rec.Body.String())
	}

	events := gw.all()
	if len(events) != 1 {
		t.Fatalf("a created order must be recorded too, got %d events", len(events))
	}
	if events[0].Status != billing.GatewayCreated {
		t.Errorf("status = %q, want %q", events[0].Status, billing.GatewayCreated)
	}
	if events[0].GatewayOrderID != "order_ABC123" {
		t.Errorf("the record must carry the order id so a payment can be matched to it later, got %q",
			events[0].GatewayOrderID)
	}
}

// Logging is a side effect, and a side effect that fails must not take the
// request with it — the subscriber's order was created either way.
func TestFR_PAY_001_AFailureToRecordDoesNotFailThePayment(t *testing.T) {
	rz := &stubRazorpay{orderID: "order_XYZ", link: "https://rzp.example/pay/XYZ"}
	gw := &gwRecorder{err: errors.New("gateway_log table is gone")}
	mux, tok := renewHarness(t, rz, gw)

	rec := renew(t, mux, tok, `{"amount":"100.00"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("a logging failure must not fail a payment that the gateway accepted, got %d — %s",
			rec.Code, rec.Body.String())
	}
}

// With no logger attached at all the handler must still work: gateway
// logging is wired in cmd/api and absent in any deployment that has not
// configured it.
func TestFR_PAY_001_NoLoggerAttachedIsSafe(t *testing.T) {
	rz := &stubRazorpay{orderID: "order_NOLOG", link: "https://rzp.example/pay/NOLOG"}
	mux, tok := renewHarness(t, rz, nil)

	if rec := renew(t, mux, tok, `{"amount":"250.00"}`); rec.Code != http.StatusOK {
		t.Errorf("want 200 with no gateway logger attached, got %d — %s", rec.Code, rec.Body.String())
	}
}

// An amount that is not an amount is refused before the gateway is called —
// a malformed request must not become an order.
func TestFR_PAY_001_AnInvalidAmountNeverReachesTheGateway(t *testing.T) {
	rz := &stubRazorpay{orderID: "order_SHOULD_NOT_HAPPEN"}
	gw := &gwRecorder{}
	mux, tok := renewHarness(t, rz, gw)

	for _, body := range []string{
		`{"amount":"0"}`, `{"amount":"-10.00"}`, `{"amount":"abc"}`, `{"amount":""}`,
	} {
		rec := renew(t, mux, tok, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: want 422, got %d", body, rec.Code)
		}
	}
	if rz.calls != 0 {
		t.Errorf("an invalid amount reached the gateway %d time(s)", rz.calls)
	}
	if len(gw.all()) != 0 {
		t.Errorf("an invalid amount was recorded as a gateway event: %+v", gw.all())
	}
}
