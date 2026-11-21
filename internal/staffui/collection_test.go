package staffui

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// Field payment collection — FR-COL-001.

type fakeCollection struct {
	mine      []billing.CollectionItem
	others    []billing.CollectionItem
	loads     []billing.CollectorLoad
	takings   []billing.CollectorTakings
	roundFor  int
	window    int
	recorded  []recordedCollection
	assignSub int
	assignTo  *int
	assigned  bool
	recordErr error
}

type recordedCollection struct {
	subscriberID int
	amount       decimal.Decimal
	method       string
	unscoped     bool
}

func (f *fakeCollection) AssignCollector(_ context.Context, subscriberID int, collectorID *int) error {
	f.assignSub, f.assignTo, f.assigned = subscriberID, collectorID, true
	return nil
}

func (f *fakeCollection) CollectionRoundFor(_ context.Context, collectorID, windowDays int) ([]billing.CollectionItem, error) {
	f.roundFor, f.window = collectorID, windowDays
	return f.others, nil
}

func (f *fakeCollection) MyCollectionRound(ctx context.Context, windowDays int) ([]billing.CollectionItem, error) {
	f.window = windowDays
	// The real store resolves the signed-in username to a staff id, so a
	// context with no subject is a 500 in production. The fake refuses it
	// for the same reason, because ignoring it is what let that bug ship
	// as far as the browser.
	if middleware.SubjectFromContext(ctx) == "" {
		return nil, errors.New("db: no staff account \"\": db: not found")
	}
	return f.mine, nil
}

func (f *fakeCollection) ListCollectors(context.Context) ([]billing.CollectorLoad, error) {
	return f.loads, nil
}

func (f *fakeCollection) CollectorTakings(context.Context, time.Time, time.Time) ([]billing.CollectorTakings, error) {
	return f.takings, nil
}

func (f *fakeCollection) RecordCollection(_ context.Context, subscriberID int, amount decimal.Decimal, pd billing.PaymentDetail, unscoped bool) (*billing.Transaction, error) {
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	f.recorded = append(f.recorded, recordedCollection{
		subscriberID: subscriberID, amount: amount, method: pd.Method, unscoped: unscoped,
	})
	return &billing.Transaction{Amount: amount}, nil
}

func dueItem(id int, user, price, balance string, expiry *time.Time) billing.CollectionItem {
	return billing.CollectionItem{
		SubscriberID: id, Username: user, MobileNumber: "+919876543210",
		PlanName:   "Fibre 100",
		PlanPrice:  decimal.RequireFromString(price),
		Balance:    decimal.RequireFromString(balance),
		PlanExpiry: expiry, Status: "grace_period", DunningState: "reminded",
	}
}

func collectionScreen(mine ...billing.CollectionItem) (*Handler, *fakeCollection) {
	f := &fakeCollection{mine: mine}
	return &Handler{collection: f}, f
}

func TestFR_COL_001_TheRoundShowsWhatToAskForAtTheDoor(t *testing.T) {
	lapsed := time.Now().Add(-48 * time.Hour)
	h, _ := collectionScreen(
		dueItem(1, "overdue@isp", "799.00", "0.00", &lapsed),
		dueItem(2, "part@isp", "799.00", "300.00", &lapsed),
	)

	w := servePR(h, "technician", "GET", "/staff/collections", nil, h.Collections)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"overdue@isp", "part@isp",
		// The digits, not the leading "+": html/template escapes it to
		// &#43; in text context, which the browser renders correctly.
		"919876543210",
		// The figure the collector asks for, not the plan price.
		"799.00", "499.00",
		// Lapsed subscribers are the ones worth visiting first, and the
		// reason is on the page.
		"off service",
		// The total, so a collector knows what the round is worth.
		"outstanding",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

// A subscriber who paid online between the list being drawn and the visit
// stays on it, marked. A row that silently vanishes looks like a bug to the
// person working the round.
func TestFR_COL_001_AnAlreadyPaidSubscriberIsMarkedNotHidden(t *testing.T) {
	lapsed := time.Now().Add(-24 * time.Hour)
	h, _ := collectionScreen(dueItem(3, "paid@isp", "799.00", "799.00", &lapsed))

	body := servePR(h, "technician", "GET", "/staff/collections", nil, h.Collections).Body.String()
	if !strings.Contains(body, "paid@isp") {
		t.Error("a subscriber who has paid dropped off the round silently")
	}
	if !strings.Contains(body, "already paid") {
		t.Error("the page does not say why there is nothing to collect")
	}
	if !strings.Contains(body, "Nothing to collect") {
		t.Error("a covered subscriber is still offered a payment form")
	}
}

// The security property this feature turns on. A technician may record only
// for their own round, and that is enforced by passing unscoped=false — the
// API then checks the assignment. Owner and billing pass unscoped=true,
// because they can already record from the subscriber's page.
func TestFR_COL_001_OnlyOwnerAndBillingRecordUnscoped(t *testing.T) {
	cases := []struct {
		role         string
		wantUnscoped bool
	}{
		{"technician", false},
		{"isp_owner", true},
		{"billing_admin", true},
	}

	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			h, f := collectionScreen()
			form := url.Values{"amount": {"799.00"}, "method": {billing.MethodCash}}
			w := servePR(h, tc.role, "POST", "/staff/collections/5/payment", form, h.RecordCollectionForm)
			if w.Code != 303 {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			if len(f.recorded) != 1 {
				t.Fatalf("recorded %d payments, want 1", len(f.recorded))
			}
			got := f.recorded[0]
			if got.unscoped != tc.wantUnscoped {
				t.Errorf("%s recorded with unscoped=%v, want %v — a field collector "+
					"must not be able to record for somebody else's round",
					tc.role, got.unscoped, tc.wantUnscoped)
			}
			if got.subscriberID != 5 {
				t.Errorf("recorded against subscriber %d, want 5", got.subscriberID)
			}
			if !got.amount.Equal(decimal.RequireFromString("799.00")) {
				t.Errorf("recorded %s, want 799.00", got.amount)
			}
		})
	}
}

// A collector told they cannot record for somebody must be told what to do
// about it, not just refused.
func TestFR_COL_001_RecordingOutsideYourRoundIsExplained(t *testing.T) {
	h, f := collectionScreen()
	f.recordErr = billing.ErrNotYourCollection

	form := url.Values{"amount": {"799.00"}, "method": {billing.MethodCash}}
	w := servePR(h, "technician", "POST", "/staff/collections/9/payment", form, h.RecordCollectionForm)
	if w.Code != 303 {
		t.Fatalf("got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "m=notyours") {
		t.Fatalf("redirect %q does not carry the refusal", loc)
	}
	msg := collectionMessages["notyours"]
	if !strings.Contains(msg, "not on your round") {
		t.Errorf("the message does not say why: %q", msg)
	}
	if !strings.Contains(msg, "assign") {
		t.Errorf("the message does not say what to do about it: %q", msg)
	}
}

func TestFR_COL_001_AnUnusableAmountIsRefusedWithAReason(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"not a number", url.Values{"amount": {"a lot"}, "method": {billing.MethodCash}}, "as a number"},
		{"zero", url.Values{"amount": {"0"}, "method": {billing.MethodCash}}, "more than zero"},
		{"negative", url.Values{"amount": {"-100"}, "method": {billing.MethodCash}}, "more than zero"},
		{"a cheque date that is not a date", url.Values{
			"amount": {"799.00"}, "method": {billing.MethodCheque}, "cheque_date": {"last Tuesday"},
		}, "not a date"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, f := collectionScreen()
			w := servePR(h, "technician", "POST", "/staff/collections/5/payment", tc.form, h.RecordCollectionForm)
			if w.Code != 303 {
				t.Fatalf("got %d", w.Code)
			}
			decoded, err := url.QueryUnescape(w.Header().Get("Location"))
			if err != nil {
				t.Fatalf("undecodable redirect: %v", err)
			}
			if !strings.Contains(decoded, tc.want) {
				t.Errorf("the collector is told %q, which does not explain the problem (wanted %q)",
					decoded, tc.want)
			}
			if len(f.recorded) != 0 {
				t.Error("an unusable amount reached the payment path")
			}
		})
	}
}

// A collector must not be able to look at somebody else's round, and must
// not be offered the assignment or reconciliation controls.
func TestFR_COL_001_ACollectorSeesOnlyTheirOwnRound(t *testing.T) {
	h, f := collectionScreen(dueItem(1, "mine@isp", "799.00", "0.00", nil))
	f.others = []billing.CollectionItem{dueItem(2, "someone-elses@isp", "799.00", "0.00", nil)}

	// Typing the parameter is the real test; the control not being offered
	// is a convenience.
	body := servePR(h, "technician", "GET", "/staff/collections?collector=99", nil, h.Collections).Body.String()

	if strings.Contains(body, "someone-elses@isp") {
		t.Error("a technician saw another collector's round by passing a parameter")
	}
	if !strings.Contains(body, "mine@isp") {
		t.Error("the technician's own round is missing")
	}
	if f.roundFor != 0 {
		t.Errorf("CollectionRoundFor was called with %d for a technician", f.roundFor)
	}
	// No assignment form, no takings table.
	if strings.Contains(body, "/staff/collections/assign") {
		t.Error("a technician is offered the assignment form")
	}
	if strings.Contains(body, "Takings for") {
		t.Error("a technician is shown the cash reconciliation")
	}
}

func TestFR_COL_001_OwnerCanViewAnotherRoundAndReconcile(t *testing.T) {
	h, f := collectionScreen()
	f.others = []billing.CollectionItem{dueItem(2, "theirs@isp", "799.00", "0.00", nil)}
	f.loads = []billing.CollectorLoad{{StaffID: 99, Username: "ravi", Role: "technician", Assigned: 4}}
	f.takings = []billing.CollectorTakings{{
		Collector: "ravi",
		ByMethod: map[string]decimal.Decimal{
			billing.MethodCash: decimal.RequireFromString("2400.00"),
			billing.MethodUPI:  decimal.RequireFromString("1600.00"),
		},
		Count: 5, Total: decimal.RequireFromString("4000.00"),
	}}

	body := servePR(h, "isp_owner", "GET", "/staff/collections?collector=99", nil, h.Collections).Body.String()

	if f.roundFor != 99 {
		t.Errorf("the owner's request read round %d, want 99", f.roundFor)
	}
	if !strings.Contains(body, "theirs@isp") {
		t.Error("the owner cannot see another collector's round")
	}
	for _, want := range []string{
		"Takings for", "ravi", "2400.00", "4000.00",
		// Cash is the line that has to be handed in, and the page says so.
		"handed in",
		"/staff/collections/assign",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the owner's page does not show %q", want)
		}
	}
}

// Zero is a meaningful window — "only those already lapsed" — so it must
// survive, and a nonsense value must not silently become zero and show a
// collector a shorter round than they asked for.
func TestFR_COL_001_TheLookAheadWindowIsReadCarefully(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{"", 7}, {"0", 0}, {"3", 3}, {"30", 30},
		{"soon", 7}, {"-5", 7}, {"9999", 7},
	} {
		if got := collectionWindow(tc.raw); got != tc.want {
			t.Errorf("collectionWindow(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}

	h, f := collectionScreen()
	servePR(h, "technician", "GET", "/staff/collections?window=0", nil, h.Collections)
	if f.window != 0 {
		t.Errorf("a window of 0 reached the store as %d, losing the lapsed-only view", f.window)
	}
}

func TestFR_COL_001_AssignmentIsWithheldFromCollectors(t *testing.T) {
	h, f := collectionScreen()
	form := url.Values{"subscriber_id": {"5"}, "collector_id": {"99"}}
	w := servePR(h, "technician", "POST", "/staff/collections/assign", form, h.AssignCollectorForm)
	if w.Code != 403 {
		t.Errorf("a technician assigning a collector got %d, want 403", w.Code)
	}
	if f.assigned {
		t.Error("the assignment reached the store despite the refusal")
	}

	h2, f2 := collectionScreen()
	servePR(h2, "billing_admin", "POST", "/staff/collections/assign", form, h2.AssignCollectorForm)
	if !f2.assigned || f2.assignSub != 5 || f2.assignTo == nil || *f2.assignTo != 99 {
		t.Errorf("billing's assignment reached the store as sub=%d to=%v", f2.assignSub, f2.assignTo)
	}

	// An empty selection takes them off the round, which has to be
	// expressible rather than a one-way door.
	h3, f3 := collectionScreen()
	servePR(h3, "isp_owner", "POST", "/staff/collections/assign",
		url.Values{"subscriber_id": {"5"}, "collector_id": {""}}, h3.AssignCollectorForm)
	if !f3.assigned {
		t.Fatal("clearing the collector did not reach the store")
	}
	if f3.assignTo != nil {
		t.Errorf("clearing passed collector %v, want nil", f3.assignTo)
	}
}

// Finance tier plus technicians, and withheld from the desk-based roles.
func TestFR_COL_001_CollectionsReach(t *testing.T) {
	has := func(role string) bool {
		for _, sec := range AllowedSections(role, true) {
			if sec.Key == "collections" {
				return true
			}
		}
		return false
	}
	for _, role := range []string{"isp_owner", "billing_admin", "technician"} {
		if !has(role) {
			t.Errorf("%s cannot reach collections", role)
		}
	}
	for _, role := range []string{"csr", "noc_engineer"} {
		if has(role) {
			t.Errorf("%s can reach collections", role)
		}
	}
}

func TestFR_COL_001_AnUnconfiguredStoreSaysSoRatherThanFailing(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/collections", nil, h.Collections)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}
