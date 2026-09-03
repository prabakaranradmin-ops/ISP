package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// â”€â”€ Minimal stubs â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type stubDB struct{}

func (s *stubDB) CreateSubscriber(_ context.Context, sub api.SubscriberRecord, _ string) (*api.SubscriberRecord, error) {
	sub.ID = 1
	return &sub, nil
}

func (s *stubDB) GetSubscriberByID(_ context.Context, id int) (*api.SubscriberRecord, error) {
	if id == 404 {
		return nil, nil
	}
	return &api.SubscriberRecord{ID: id, Username: "test", Status: "active"}, nil
}

func (s *stubDB) UpdateSubscriber(_ context.Context, id int, _ *int, _ *string, _ *time.Time) (*api.SubscriberRecord, error) {
	return &api.SubscriberRecord{ID: id, Status: "active"}, nil
}

func (s *stubDB) GetSubscriberByUsername(_ context.Context, _ string) (*api.SubscriberRecord, error) {
	return nil, nil
}

type stubKYC struct{}

func (s *stubKYC) UpsertKYC(_ context.Context, _ int, _, _, _ string) error {
	return nil
}

// stubWallet implements billing.WalletQuerier
type stubWallet struct{}

func (s *stubWallet) GetTransactionByToken(_ context.Context, _ string) (*billing.Transaction, error) {
	return nil, nil
}
func (s *stubWallet) RecordRecharge(_ context.Context, p billing.RechargePosting) (*billing.Transaction, error) {
	return &billing.Transaction{
		ID:           1,
		SubscriberID: p.Credit.SubscriberID,
		EntryType:    p.Credit.EntryType,
		Amount:       p.Credit.Amount,
		BalanceAfter: p.NewBalance,
	}, nil
}
func (s *stubWallet) GetSubscriberBalance(_ context.Context, _ int) (decimal.Decimal, error) {
	return decimal.NewFromFloat(0), nil
}

// â”€â”€ Tests â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// newTestHandler builds a Handler with the minimal stubs every test in this
// file shares. Route-specific dependencies (ledger, sessions, tickets, ...)
// are left nil, which the handlers for those routes report as 503 rather than
// panicking -- none of the tests below exercise them.
func newTestHandler() *api.Handler {
	return api.NewHandler(api.HandlerDeps{
		DB:     &stubDB{},
		KYC:    &stubKYC{},
		Wallet: billing.NewWalletService(&stubWallet{}),
	})
}

func TestHealthRoute(t *testing.T) {
	h := newTestHandler()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, "test-secret")

	req := httptest.NewRequest("GET", "/health", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health: want 200, got %d", rec.Code)
	}
}

// TestGetSubscriber_UnauthenticatedReturns401 verifies the route is behind
// auth. It does NOT reach GetSubscriber's own not-found handling — that is
// covered separately (with a valid token) in integration_test.go, since a
// route can never exercise its own handler logic without first getting past
// the auth gate this test stops at.
func TestGetSubscriber_UnauthenticatedReturns401(t *testing.T) {
	h := newTestHandler()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, "test-secret")

	req := httptest.NewRequest("GET", "/api/v1/subscribers/404", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}
}

// TestWalletRecharge_UnauthenticatedReturns401 is the WalletRecharge
// counterpart to TestGetSubscriber_UnauthenticatedReturns401 — see its
// comment. The actual amount-validation behavior is covered (with a valid
// token) in integration_test.go.
func TestWalletRecharge_UnauthenticatedReturns401(t *testing.T) {
	h := newTestHandler()
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, "test-secret")

	body := map[string]any{
		"subscriber_id":     1,
		"amount":            "badnum",
		"payment_method":    "razorpay",
		"transaction_token": "tok_001",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/v1/wallets/recharge", bytes.NewReader(b)) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}
}

// Ensure decimal round-trips correctly in API responses.
func TestDecimalRoundTrip(t *testing.T) {
	d := decimal.NewFromFloat(799.0)
	if d.StringFixed(2) != "799.00" {
		t.Fatalf("want 799.00 got %s", d.StringFixed(2))
	}
}
