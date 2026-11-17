//go:build integration

// Recurring-billing run history — FR-BIL-020. API shape and gate; the
// recording itself is proven against a real database in internal/db.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

type stubBillingRuns struct{ gotFilter billing.BillingRunFilter }

func (s *stubBillingRuns) ListBillingRuns(_ context.Context, f billing.BillingRunFilter) ([]billing.BillingRun, error) {
	s.gotFilter = f
	return []billing.BillingRun{{ID: 7, Kind: billing.RunKindRecurringBilling, Status: billing.RunRunning,
		StartedAt: time.Now(), AmountCharged: decimal.Zero}}, nil
}

func (s *stubBillingRuns) GetBillingRun(_ context.Context, id int) (*billing.BillingRun, error) {
	if id != 7 {
		return nil, nil
	}
	done := time.Now()
	return &billing.BillingRun{ID: 7, Kind: billing.RunKindRecurringBilling, Status: billing.RunCompleted,
		StartedAt: done.Add(-3 * time.Second), FinishedAt: &done, Candidates: 2, Renewed: 1, Failed: 1,
		AmountCharged: decimal.RequireFromString("118"),
		Failures:      []billing.RunFailure{{SubscriberID: 3, Username: "u", Stage: billing.StageExpiry, Message: "db down", Charged: true}},
	}, nil
}

func runsHarness(t *testing.T, s *stubBillingRuns) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), BillingRuns: s})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func TestFR_BIL_020_BillingRunsAPI(t *testing.T) {
	t.Run("the list passes its filters through; a running run has no end yet", func(t *testing.T) {
		s := &stubBillingRuns{}
		code, body := cnDo(t, runsHarness(t, s), "GET", "/api/v1/billing/runs?include_idle=true&problems=true&limit=20", ``, "billing_admin", "meena")
		if code != 200 || !s.gotFilter.IncludeIdle || !s.gotFilter.ProblemsOnly || s.gotFilter.Limit != 20 {
			t.Fatalf("got %d, filter %+v", code, s.gotFilter)
		}
		var got []map[string]any
		_ = json.Unmarshal([]byte(body), &got)
		if len(got) != 1 || got[0]["finished_at"] != nil {
			t.Errorf("runs: %s", body)
		}
	})

	t.Run("one run carries its failures and whether each was charged", func(t *testing.T) {
		code, body := cnDo(t, runsHarness(t, &stubBillingRuns{}), "GET", "/api/v1/billing/runs/7", ``, "isp_owner", "owner")
		var got struct {
			AmountCharged string           `json:"amount_charged"`
			Duration      float64          `json:"duration_seconds"`
			Failures      []map[string]any `json:"failures"`
		}
		if code != 200 || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("got %d: %s", code, body)
		}
		if got.AmountCharged != "118.00" || got.Duration < 2.9 || len(got.Failures) != 1 ||
			got.Failures[0]["stage"] != "expiry" || got.Failures[0]["charged"] != true {
			t.Errorf("run: %s", body)
		}
	})

	t.Run("an unknown run is a 404", func(t *testing.T) {
		if code, _ := cnDo(t, runsHarness(t, &stubBillingRuns{}), "GET", "/api/v1/billing/runs/99", ``, "isp_owner", "owner"); code != 404 {
			t.Errorf("got %d", code)
		}
	})

	t.Run("a CSR cannot read run history", func(t *testing.T) {
		s := &stubBillingRuns{}
		if code, _ := cnDo(t, runsHarness(t, s), "GET", "/api/v1/billing/runs", ``, "csr", "divya"); code != http.StatusForbidden {
			t.Errorf("got %d", code)
		}
	})
}
