package staffui

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// Scheduled plan changes in the console — FR-BIL-023.

type fakePlanChanges struct {
	err         error
	scheduled   []billing.SchedulePlanChangeRequest
	actor       string
	cancelledID int
	reason      string
	list        []billing.ScheduledPlanChange
	gotFilter   billing.PlanChangeFilter
}

func (f *fakePlanChanges) SchedulePlanChange(ctx context.Context, req billing.SchedulePlanChangeRequest) (*billing.ScheduledPlanChange, error) {
	if _, err := req.Normalise(time.Now()); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.scheduled, f.actor = append(f.scheduled, req), middleware.SubjectFromContext(ctx)
	return &billing.ScheduledPlanChange{ID: 77, SubscriberID: req.SubscriberID}, nil
}

func (f *fakePlanChanges) CancelPlanChange(ctx context.Context, id int, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return billing.ErrPlanChangeInvalid
	}
	if f.err != nil {
		return f.err
	}
	f.cancelledID, f.reason, f.actor = id, reason, middleware.SubjectFromContext(ctx)
	return nil
}

func (f *fakePlanChanges) ListPlanChanges(_ context.Context, flt billing.PlanChangeFilter) ([]billing.ScheduledPlanChange, error) {
	f.gotFilter = flt
	return f.list, nil
}

func TestFR_BIL_023_ScheduleFormReachesTheStore(t *testing.T) {
	// Far enough ahead to stay valid however long this test lives.
	when := time.Now().AddDate(0, 0, 20).Format("2006-01-02")

	t.Run("billing schedules one, attributed, and lands back on the subscriber", func(t *testing.T) {
		f := &fakePlanChanges{}
		h := &Handler{planChanges: f}
		form := url.Values{"new_plan_id": {"2"}, "effective_on": {when}, "reason": {"Downgrade"}}
		w := servePR(h, "billing_admin", "POST", "/staff/subscribers/5/scheduled-plan-change", form, h.SchedulePlanChangeForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.scheduled) != 1 {
			t.Fatalf("store got %+v", f.scheduled)
		}
		got := f.scheduled[0]
		if got.SubscriberID != 5 || got.NewPlanID != 2 || got.Reason != "Downgrade" {
			t.Errorf("store got %+v", got)
		}
		if got.EffectiveOn.Format("2006-01-02") != when {
			t.Errorf("effective_on reached the store as %v", got.EffectiveOn)
		}
		if f.actor != "billing_admin_user" {
			t.Errorf("attributed to %q", f.actor)
		}
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/staff/subscribers/5?msg=") {
			t.Errorf("redirect %q", loc)
		}
	})

	t.Run("a CSR cannot schedule or cancel", func(t *testing.T) {
		f := &fakePlanChanges{}
		h := &Handler{planChanges: f}
		if code := servePR(h, "csr", "POST", "/staff/subscribers/5/scheduled-plan-change",
			url.Values{"new_plan_id": {"2"}, "effective_on": {when}}, h.SchedulePlanChangeForm).Code; code != 403 {
			t.Errorf("schedule: got %d, want 403", code)
		}
		if code := servePR(h, "csr", "POST", "/staff/scheduled-plan-changes/77/cancel",
			url.Values{"reason": {"x"}, "subscriber_id": {"5"}}, h.CancelPlanChangeForm).Code; code != 403 {
			t.Errorf("cancel: got %d, want 403", code)
		}
		if len(f.scheduled) != 0 || f.cancelledID != 0 {
			t.Error("a refused request still reached the store")
		}
	})

	t.Run("a missing date comes back worded, and nothing is saved", func(t *testing.T) {
		f := &fakePlanChanges{}
		h := &Handler{planChanges: f}
		w := servePR(h, "isp_owner", "POST", "/staff/subscribers/5/scheduled-plan-change",
			url.Values{"new_plan_id": {"2"}, "effective_on": {""}}, h.SchedulePlanChangeForm)
		if w.Code != 200 {
			t.Fatalf("got %d, want the page re-rendered with an error", w.Code)
		}
		if !strings.Contains(w.Body.String(), "give the date") {
			t.Error("the page does not say what was wrong")
		}
		if len(f.scheduled) != 0 {
			t.Error("a dateless request reached the store")
		}
	})

	t.Run("a date already past is refused by the domain rules", func(t *testing.T) {
		f := &fakePlanChanges{}
		h := &Handler{planChanges: f}
		past := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
		w := servePR(h, "isp_owner", "POST", "/staff/subscribers/5/scheduled-plan-change",
			url.Values{"new_plan_id": {"2"}, "effective_on": {past}}, h.SchedulePlanChangeForm)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "already passed") {
			t.Errorf("unhelpful page: %s", firstError(w.Body.String()))
		}
		if len(f.scheduled) != 0 {
			t.Error("a past-dated request reached the store")
		}
	})

	t.Run("cancelling carries its reason and returns to the subscriber", func(t *testing.T) {
		f := &fakePlanChanges{}
		h := &Handler{planChanges: f}
		form := url.Values{"reason": {"Customer changed their mind"}, "subscriber_id": {"5"}}
		w := servePR(h, "billing_admin", "POST", "/staff/scheduled-plan-changes/77/cancel", form, h.CancelPlanChangeForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if f.cancelledID != 77 || f.reason != "Customer changed their mind" {
			t.Errorf("store got %d / %q", f.cancelledID, f.reason)
		}
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/staff/subscribers/5?msg=") {
			t.Errorf("redirect %q", loc)
		}
	})

	t.Run("an unconfigured store says so rather than failing", func(t *testing.T) {
		h := &Handler{}
		if code := servePR(h, "isp_owner", "POST", "/staff/subscribers/5/scheduled-plan-change",
			url.Values{"new_plan_id": {"2"}, "effective_on": {when}}, h.SchedulePlanChangeForm).Code; code != 503 {
			t.Errorf("got %d, want 503", code)
		}
	})
}

// firstError pulls a short excerpt out of a rendered page, so a failure
// message says what the page actually said.
func firstError(body string) string {
	i := strings.Index(body, "alert")
	if i < 0 || i+200 > len(body) {
		return "(no alert found)"
	}
	return body[i : i+200]
}

// The plan card on the subscriber page: what it shows, and to whom.
func TestFR_BIL_023_SubscriberPagePlanChangeCard(t *testing.T) {
	pending := billing.ScheduledPlanChange{
		ID: 77, SubscriberID: 5, Status: billing.PlanChangeScheduled,
		FromPlanID: 1, FromPlanName: "Basic",
		NewPlanID: 2, NewPlanName: "Fibre 100",
		NewPlanPrice: decimal.RequireFromString("1200.00"),
		EffectiveOn:  time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC),
		ScheduledBy:  "meena", ScheduledAt: time.Now(), Reason: "Customer asked",
	}
	appliedAt := time.Now()
	invoiceID := 9
	applied := billing.ScheduledPlanChange{
		ID: 70, SubscriberID: 5, Status: billing.PlanChangeApplied,
		FromPlanName: "Trial", NewPlanName: "Basic",
		NewPlanPrice: decimal.RequireFromString("500.00"),
		EffectiveOn:  time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC),
		ScheduledBy:  "ravi", ScheduledAt: appliedAt,
		AppliedAt: &appliedAt, AppliedInvoiceID: &invoiceID,
	}

	render := func(role string, manage bool, changes []billing.ScheduledPlanChange, pendingPtr *billing.ScheduledPlanChange) string {
		rec := httptest.NewRecorder()
		s := testSession()
		s.Role = role
		(&Handler{}).render(rec, "subscriber_detail", pageData{Session: s, Data: subscriberDetailData{
			Subscriber:  &api.SubscriberRecord{ID: 5, Username: "acme", Status: "active", PlanID: 1},
			ShowBilling: true, ShowPlanChange: manage,
			Plans:                 []Plan{{ID: 1, Name: "Basic"}, {ID: 2, Name: "Fibre 100"}},
			PlanChanges:           changes,
			PendingPlanChange:     pendingPtr,
			PlanChangeDefaultDate: "2026-04-01",
		}})
		return rec.Body.String()
	}

	t.Run("a pending change is shown with its date, price and cancel form", func(t *testing.T) {
		body := render("billing_admin", true, []billing.ScheduledPlanChange{pending}, &pending)
		for _, want := range []string{
			"Basic → Fibre 100", "01 Apr 2026", "1200.00", "meena", "Customer asked",
			`action="/staff/scheduled-plan-changes/77/cancel"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the card does not show %q", want)
			}
		}
		// With one pending, the schedule form must not also be offered —
		// migration 059 allows exactly one.
		if strings.Contains(body, `action="/staff/subscribers/5/scheduled-plan-change"`) {
			t.Error("the schedule form is offered while one is already pending")
		}
	})

	t.Run("with none pending the schedule form is offered, excluding the current plan", func(t *testing.T) {
		body := render("isp_owner", true, nil, nil)
		if !strings.Contains(body, `action="/staff/subscribers/5/scheduled-plan-change"`) {
			t.Fatal("the schedule form is missing")
		}
		if !strings.Contains(body, `value="2"`) {
			t.Error("the other plan is not offered")
		}
		// The plan they are already on is not an option: the store would
		// refuse it, so offering it invites a pointless error.
		if strings.Contains(body, `<option value="1">Basic</option>`) {
			t.Error("the subscriber's current plan is offered as a target")
		}
		if !strings.Contains(body, `value="2026-04-01"`) {
			t.Error("the date is not pre-filled with the next renewal")
		}
	})

	t.Run("a CSR sees the change but gets no forms", func(t *testing.T) {
		body := render("csr", false, []billing.ScheduledPlanChange{pending}, &pending)
		if !strings.Contains(body, "Basic → Fibre 100") {
			t.Error("a CSR cannot see a scheduled change at all")
		}
		if strings.Contains(body, `action="/staff/scheduled-plan-changes/77/cancel"`) ||
			strings.Contains(body, `action="/staff/subscribers/5/scheduled-plan-change"`) {
			t.Error("a CSR was served the management forms")
		}
	})

	t.Run("history shows an applied change and links its invoice", func(t *testing.T) {
		body := render("billing_admin", true, []billing.ScheduledPlanChange{applied}, nil)
		if !strings.Contains(body, "Trial → Basic") || !strings.Contains(body, "Applied") {
			t.Error("the applied change is missing from the history")
		}
		if !strings.Contains(body, "INV-000009") {
			t.Error("the invoice number is missing")
		}
		if !strings.Contains(body, `href="/staff/invoices/9/pdf"`) {
			t.Error("the invoice link is wrong — a pointer may be leaking into the URL")
		}
	})
}
