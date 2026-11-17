package staffui

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

type fakeDiagnostics struct {
	res      *billing.Diagnostics
	err      error
	gotLimit int
}

func (f *fakeDiagnostics) BillingDiagnostics(_ context.Context, limit int) (*billing.Diagnostics, error) {
	f.gotLimit = limit
	return f.res, f.err
}

func serveDiag(h *Handler, role string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/staff/diagnostics", nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	w := httptest.NewRecorder()
	h.BillingDiagnostics(w, r)
	return w
}

func TestFR_BIL_019_DiagnosticsScreen(t *testing.T) {
	now := time.Now()
	last := now.AddDate(0, 0, -40)
	after := decimal.NewFromInt(500)
	found := &billing.Diagnostics{
		RanAt: now,
		Uninvoiced: []billing.UninvoicedCycle{
			{SubscriberID: 3, Username: "never_invoiced", PlanName: "Home", PlanPrice: decimal.NewFromInt(799), CycleStart: now, PlanExpiry: now},
			{SubscriberID: 4, Username: "plan_changed", PlanName: "Home", PlanPrice: decimal.NewFromInt(799), CycleStart: now, PlanExpiry: now, LastInvoice: &last},
		},
		UninvoicedCount: 2,
		Mismatches: []billing.BalanceMismatch{
			{SubscriberID: 5, Username: "edited", Stored: decimal.NewFromInt(700), LedgerSum: decimal.NewFromInt(500), LastAfter: &after, Entries: 1},
			{SubscriberID: 6, Username: "no_ledger", Stored: decimal.NewFromInt(100)},
		},
		MismatchCount: 2,
		Drifts: []billing.PriceDrift{{SubscriberID: 7, Username: "old_price", PlanName: "Home",
			CataloguePrice: decimal.NewFromInt(799), BilledBase: decimal.NewFromInt(699), InvoiceID: 12, InvoicedAt: now}},
		DriftCount: 1,
	}

	t.Run("each finding is listed with its figures and a link to the subscriber", func(t *testing.T) {
		f := &fakeDiagnostics{res: found}
		w := serveDiag(&Handler{diagnostics: f}, "billing_admin")
		body := w.Body.String()
		if w.Code != 200 || f.gotLimit != diagnosticsLimit {
			t.Fatalf("got %d, limit %d", w.Code, f.gotLimit)
		}
		for _, want := range []string{
			`/staff/subscribers/3`, `>never<`, // never invoiced is said, not left blank
			`/staff/subscribers/5`, "700.00", "500.00", "200.00",
			"no entries", // a wallet with no ledger at all
			`/staff/subscribers/7`, "699.00", "-100.00", "INV-000012",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.Contains(body, "Nothing to look at") {
			t.Error("findings must not read as all clear")
		}
	})

	t.Run("a clean run says so", func(t *testing.T) {
		w := serveDiag(&Handler{diagnostics: &fakeDiagnostics{res: &billing.Diagnostics{RanAt: now}}}, "isp_owner")
		if !strings.Contains(w.Body.String(), "Nothing to look at") {
			t.Error("a clean run should say it is clean")
		}
	})

	t.Run("a failed run never looks clean", func(t *testing.T) {
		w := serveDiag(&Handler{diagnostics: &fakeDiagnostics{err: errors.New("db down")}}, "isp_owner")
		body := w.Body.String()
		if strings.Contains(body, "Nothing to look at") || !strings.Contains(body, "could not be run") {
			t.Errorf("failure page: %.300s", body)
		}
	})

	t.Run("a CSR is kept out", func(t *testing.T) {
		f := &fakeDiagnostics{res: found}
		if code := serveDiag(&Handler{diagnostics: f}, "csr").Code; code != 403 || f.gotLimit != 0 {
			t.Errorf("got %d", code)
		}
	})
}
