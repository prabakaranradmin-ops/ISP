package staffui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// The gateway log screen — FR-PAY-001.

type fakeGatewayLog struct {
	events    []billing.GatewayLogEntry
	summary   []billing.GatewayStatusCount
	gotFilter billing.GatewayLogFilter
	err       error
}

func (f *fakeGatewayLog) ListGatewayEvents(_ context.Context, flt billing.GatewayLogFilter) ([]billing.GatewayLogEntry, error) {
	f.gotFilter = flt
	return f.events, f.err
}

func (f *fakeGatewayLog) SummariseGatewayEvents(_ context.Context, _ billing.GatewayLogFilter) ([]billing.GatewayStatusCount, error) {
	return f.summary, nil
}

func gatewayEntry(status, paymentID string, subscriberID int, amount string) billing.GatewayLogEntry {
	e := billing.GatewayLogEntry{
		ID: 1, Gateway: billing.DefaultGateway, Kind: billing.GatewayKindCallback,
		Event: "payment.captured", GatewayPaymentID: paymentID,
		Status: status, CreatedAt: time.Now(),
	}
	if subscriberID > 0 {
		e.SubscriberID = &subscriberID
		e.Username = "acme"
	}
	if amount != "" {
		v := decimal.RequireFromString(amount)
		e.Amount = &v
	}
	return e
}

func TestFR_PAY_001_GatewayLogScreenRendersEvents(t *testing.T) {
	f := &fakeGatewayLog{
		events: []billing.GatewayLogEntry{
			gatewayEntry(billing.GatewayCredited, "pay_ok", 5, "1180.00"),
			gatewayEntry(billing.GatewayUnmatched, "pay_orphan", 0, "500.00"),
		},
		summary: []billing.GatewayStatusCount{
			{Status: billing.GatewayCredited, Count: 40},
			{Status: billing.GatewayUnmatched, Count: 2},
		},
	}
	h := &Handler{gatewayLog: f}

	w := servePR(h, "billing_admin", "GET", "/staff/gateway-log", nil, h.GatewayLog)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"pay_ok", "pay_orphan", "1180.00", "500.00", "acme",
		"Credited to the wallet", "Paid, but matched no subscriber",
		// The summary leads with the number that matters.
		"need attention",
		// And says what to do about the one that does.
		"record the payment against",
		// The security claim is on the page.
		"No signature, secret or raw request body is kept",
		// And the requirement's own phrase, because it explains why rows
		// exist for things that credited nobody.
		"whether or not it credited a wallet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}

	// An unmatched row has no subscriber to link to and must not render a
	// link to nowhere.
	if strings.Contains(body, `/staff/subscribers/0`) {
		t.Error("an unmatched event rendered a link to subscriber 0")
	}
}

// The number that leads the page counts only what needs attention.
// 'credited' and 'ignored' are working as intended and would drown it.
func TestFR_PAY_001_OnlyProblemsAreCountedAsNeedingAttention(t *testing.T) {
	f := &fakeGatewayLog{
		summary: []billing.GatewayStatusCount{
			{Status: billing.GatewayCredited, Count: 100},
			{Status: billing.GatewayIgnored, Count: 50},
			{Status: billing.GatewayCreated, Count: 30},
			{Status: billing.GatewayUnmatched, Count: 2},
			{Status: billing.GatewayFailed, Count: 1},
			{Status: billing.GatewayRejected, Count: 4},
			{Status: billing.GatewayUnconfigured, Count: 1},
		},
	}
	h := &Handler{gatewayLog: f}
	body := servePR(h, "isp_owner", "GET", "/staff/gateway-log", nil, h.GatewayLog).Body.String()

	// 2 + 1 + 4 + 1 = 8. Not 188.
	if !strings.Contains(body, "<strong>8</strong>") {
		t.Errorf("the page does not report 8 events needing attention")
	}

	t.Run("with nothing wrong it says so plainly", func(t *testing.T) {
		g := &fakeGatewayLog{summary: []billing.GatewayStatusCount{
			{Status: billing.GatewayCredited, Count: 100},
			{Status: billing.GatewayIgnored, Count: 50},
		}}
		hh := &Handler{gatewayLog: g}
		body := servePR(hh, "isp_owner", "GET", "/staff/gateway-log", nil, hh.GatewayLog).Body.String()
		if !strings.Contains(body, "every authenticated payment reached a wallet") {
			t.Error("a clean window is not reported as clean")
		}
	})
}

// Which statuses count as needing attention is the judgement this screen
// turns on, so it is pinned rather than left to the template.
func TestFR_PAY_001_NeedsAttentionCoversMoneyThatDidNotArrive(t *testing.T) {
	needs := map[string]bool{
		billing.GatewayUnmatched:    true,
		billing.GatewayFailed:       true,
		billing.GatewayRejected:     true,
		billing.GatewayUnconfigured: true,
		billing.GatewayCredited:     false,
		billing.GatewayIgnored:      false,
		billing.GatewayCreated:      false,
	}
	for status, want := range needs {
		e := billing.GatewayLogEntry{Status: status}
		if got := e.NeedsAttention(); got != want {
			t.Errorf("%s: NeedsAttention()=%v, want %v", status, got, want)
		}
		if e.StatusLabel() == "" || e.StatusLabel() == status {
			t.Errorf("%s has no operator-facing label", status)
		}
	}
	// Every outcome that needs attention also tells an operator what to
	// do: the status says what happened, not what to do about it.
	for status, want := range needs {
		if want && billing.GatewayStatusAdvice[status] == "" {
			t.Errorf("%s needs attention but carries no advice", status)
		}
	}
}

func TestFR_PAY_001_GatewayLogFiltersReachTheStore(t *testing.T) {
	t.Run("status, kind, payment id and dates pass through", func(t *testing.T) {
		f := &fakeGatewayLog{}
		h := &Handler{gatewayLog: f}
		servePR(h, "billing_admin", "GET",
			"/staff/gateway-log?status=unmatched&kind=callback&payment_id=pay_x&from=2026-04-01&to=2026-04-30",
			nil, h.GatewayLog)

		got := f.gotFilter
		if got.Status != "unmatched" || got.Kind != "callback" || got.PaymentID != "pay_x" {
			t.Errorf("store got %+v", got)
		}
		if got.From == nil || got.From.Format("2006-01-02") != "2026-04-01" {
			t.Errorf("from reached the store as %v", got.From)
		}
		if got.To == nil || got.To.Format("2006-01-02") != "2026-05-01" {
			t.Errorf("to reached the store as %v, want the day after so the 30th is included", got.To)
		}
	})

	t.Run("the problems-only view filters", func(t *testing.T) {
		f := &fakeGatewayLog{}
		h := &Handler{gatewayLog: f}
		servePR(h, "billing_admin", "GET", "/staff/gateway-log?problems=true", nil, h.GatewayLog)
		if !f.gotFilter.ProblemsOnly {
			t.Error("the problems-only view did not reach the store")
		}
	})

	t.Run("an unknown kind is ignored rather than passed on", func(t *testing.T) {
		f := &fakeGatewayLog{}
		h := &Handler{gatewayLog: f}
		servePR(h, "billing_admin", "GET", "/staff/gateway-log?kind=sideways", nil, h.GatewayLog)
		if f.gotFilter.Kind != "" {
			t.Errorf("an unknown kind reached the store as %q", f.gotFilter.Kind)
		}
	})

	t.Run("an unconfigured log says so rather than failing", func(t *testing.T) {
		h := &Handler{}
		w := servePR(h, "isp_owner", "GET", "/staff/gateway-log", nil, h.GatewayLog)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "not configured") {
			t.Errorf("got %d", w.Code)
		}
	})

	t.Run("an empty log is explained", func(t *testing.T) {
		h := &Handler{gatewayLog: &fakeGatewayLog{}}
		body := servePR(h, "isp_owner", "GET", "/staff/gateway-log", nil, h.GatewayLog).Body.String()
		if !strings.Contains(body, "No gateway events recorded yet") {
			t.Error("an empty log renders no explanation")
		}
	})
}

// Billing tier, same reach as the ledger: every row is money arriving.
func TestFR_PAY_001_GatewayLogReach(t *testing.T) {
	has := func(role string) bool {
		for _, sec := range AllowedSections(role, true) {
			if sec.Key == "gateway-log" {
				return true
			}
		}
		return false
	}
	for _, role := range []string{"isp_owner", "billing_admin"} {
		if !has(role) {
			t.Errorf("%s cannot reach the gateway log", role)
		}
	}
	for _, role := range []string{"csr", "noc_engineer", "technician"} {
		if has(role) {
			t.Errorf("%s can reach the gateway log", role)
		}
	}
}
