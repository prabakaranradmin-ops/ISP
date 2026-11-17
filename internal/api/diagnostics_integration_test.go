//go:build integration

// Billing diagnostics — FR-BIL-019. The API shape and its gate; the checks
// themselves are proven against a real database in internal/db.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

type stubDiagnostics struct {
	gotLimit int
	err      error
}

func (s *stubDiagnostics) BillingDiagnostics(_ context.Context, limit int) (*billing.Diagnostics, error) {
	s.gotLimit = limit
	if s.err != nil {
		return nil, s.err
	}
	after := decimal.RequireFromString("500")
	return &billing.Diagnostics{
		RanAt:           time.Now(),
		Uninvoiced:      []billing.UninvoicedCycle{{SubscriberID: 3, Username: "never", PlanPrice: decimal.NewFromInt(799)}},
		UninvoicedCount: 41,
		Mismatches: []billing.BalanceMismatch{{SubscriberID: 4, Username: "edited", Stored: decimal.NewFromInt(700),
			LedgerSum: decimal.NewFromInt(500), LastAfter: &after, Entries: 1}},
		MismatchCount: 1,
	}, nil
}

func diagHarness(t *testing.T, d *stubDiagnostics) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Diagnostics: d,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func TestFR_BIL_019_DiagnosticsAPI(t *testing.T) {
	t.Run("billing reads all three lists with whole-match counts", func(t *testing.T) {
		d := &stubDiagnostics{}
		code, body := cnDo(t, diagHarness(t, d), "GET", "/api/v1/billing/diagnostics?limit=50", ``, "billing_admin", "meena")
		if code != 200 || d.gotLimit != 50 {
			t.Fatalf("got %d, limit %d: %s", code, d.gotLimit, body)
		}
		var got struct {
			Clean      bool `json:"clean"`
			Uninvoiced struct {
				Count int              `json:"count"`
				Items []map[string]any `json:"items"`
			} `json:"uninvoiced_cycles"`
			Mismatches struct {
				Items []map[string]any `json:"items"`
			} `json:"balance_mismatches"`
			Drift struct {
				Items []map[string]any `json:"items"`
			} `json:"price_drift"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Clean || got.Uninvoiced.Count != 41 || len(got.Uninvoiced.Items) != 1 {
			t.Errorf("uninvoiced: %+v", got.Uninvoiced)
		}
		if got.Uninvoiced.Items[0]["last_invoice_at"] != nil {
			t.Error("never invoiced must read as null, not a zero date")
		}
		if got.Mismatches.Items[0]["difference"] != "200.00" {
			t.Errorf("difference: %v", got.Mismatches.Items[0]["difference"])
		}
		if got.Drift.Items == nil {
			t.Error("an empty list must be [], not null")
		}
	})

	t.Run("a CSR cannot read it", func(t *testing.T) {
		d := &stubDiagnostics{}
		if code, _ := cnDo(t, diagHarness(t, d), "GET", "/api/v1/billing/diagnostics", ``, "csr", "divya"); code != http.StatusForbidden || d.gotLimit != 0 {
			t.Errorf("got %d", code)
		}
	})

	t.Run("a failing store is a 500, not an empty all-clear", func(t *testing.T) {
		d := &stubDiagnostics{err: errors.New("db down")}
		if code, body := cnDo(t, diagHarness(t, d), "GET", "/api/v1/billing/diagnostics", ``, "isp_owner", "owner"); code != 500 {
			t.Errorf("got %d: %s", code, body)
		}
	})
}
