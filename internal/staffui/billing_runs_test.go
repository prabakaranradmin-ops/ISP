package staffui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

type fakeBillingRuns struct {
	gotFilter billing.BillingRunFilter
	runs      []billing.BillingRun
	one       *billing.BillingRun
}

func (f *fakeBillingRuns) ListBillingRuns(_ context.Context, flt billing.BillingRunFilter) ([]billing.BillingRun, error) {
	f.gotFilter = flt
	return f.runs, nil
}

func (f *fakeBillingRuns) GetBillingRun(_ context.Context, id int) (*billing.BillingRun, error) {
	if f.one != nil && f.one.ID == id {
		return f.one, nil
	}
	return nil, nil
}

func serveRuns(h *Handler, role, target string, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) == 3 {
		r.SetPathValue("id", parts[2])
	}
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}

func TestFR_BIL_020_BillingRunScreens(t *testing.T) {
	done := time.Now()
	runs := []billing.BillingRun{
		{ID: 12, Status: billing.RunCompleted, StartedAt: done.Add(-2 * time.Second), FinishedAt: &done,
			Candidates: 3, Renewed: 1, Activated: 1, Failed: 1, InvoicesRaised: 2, AmountCharged: decimal.RequireFromString("354")},
		{ID: 11, Status: billing.RunFailed, StartedAt: done, FinishedAt: &done, AmountCharged: decimal.Zero, Error: "connection refused"},
		{ID: 10, Status: billing.RunCompleted, StartedAt: done, FinishedAt: &done, Candidates: 1, Renewed: 1,
			InvoicesRaised: 0, InvoiceFailures: 1, AmountCharged: decimal.RequireFromString("118")},
	}

	t.Run("the list flags runs with failures and missing invoices", func(t *testing.T) {
		f := &fakeBillingRuns{runs: runs}
		h := &Handler{billingRuns: f}
		body := serveRuns(h, "billing_admin", "/staff/billing-runs", h.BillingRuns).Body.String()
		if f.gotFilter.IncludeIdle || f.gotFilter.ProblemsOnly {
			t.Errorf("default filter: %+v", f.gotFilter)
		}
		for _, want := range []string{`/staff/billing-runs/12`, "completed with failures", "354.00", ">failed<", "1 missing"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("filters reach the store", func(t *testing.T) {
		f := &fakeBillingRuns{}
		h := &Handler{billingRuns: f}
		serveRuns(h, "isp_owner", "/staff/billing-runs?problems=1&idle=1", h.BillingRuns)
		if !f.gotFilter.ProblemsOnly || !f.gotFilter.IncludeIdle {
			t.Errorf("filter: %+v", f.gotFilter)
		}
	})

	t.Run("a run's page names each failure, whether it was charged, and what to do", func(t *testing.T) {
		one := runs[0]
		one.Failures = []billing.RunFailure{{SubscriberID: 3, Username: "paid_not_renewed", Stage: billing.StageExpiry,
			Message: "extend plan_expiry: db down", Charged: true}}
		h := &Handler{billingRuns: &fakeBillingRuns{one: &one}}
		w := serveRuns(h, "billing_admin", "/staff/billing-runs/12", h.BillingRun)
		body := w.Body.String()
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		for _, want := range []string{"/staff/subscribers/3", "paid_not_renewed", ">charged<",
			"plan was not extended", "extend plan_expiry: db down"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("every failure step has advice", func(t *testing.T) {
		for _, st := range []string{billing.StagePrice, billing.StageDebit, billing.StageExpiry,
			billing.StageActivation, billing.StageInvoice, billing.StageDunning} {
			if stageAdvice[st] == "" {
				t.Errorf("no advice for %s", st)
			}
		}
	})

	t.Run("an unknown run is a 404", func(t *testing.T) {
		h := &Handler{billingRuns: &fakeBillingRuns{}}
		if code := serveRuns(h, "isp_owner", "/staff/billing-runs/99", h.BillingRun).Code; code != 404 {
			t.Errorf("got %d", code)
		}
	})

	t.Run("a CSR is kept out", func(t *testing.T) {
		f := &fakeBillingRuns{runs: runs}
		h := &Handler{billingRuns: f}
		if code := serveRuns(h, "csr", "/staff/billing-runs", h.BillingRuns).Code; code != 403 {
			t.Errorf("got %d", code)
		}
	})
}
