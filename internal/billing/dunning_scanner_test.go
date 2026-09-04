package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// The rules under test here decide when a paying customer stops being warned
// and starts being cut off, so the boundaries matter more than the happy path:
// an off-by-one day either suspends someone early or lets a non-payer stay
// online for an extra cycle.

func day(n float64) time.Time {
	return time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC).Add(time.Duration(n * float64(24*time.Hour)))
}

var scanNow = day(0)

func TestFR_BIL_004_NextDunningState_Ladder(t *testing.T) {
	cases := []struct {
		name       string
		current    billing.DunningState
		expiryDays float64 // relative to scanNow; negative means already expired
		want       billing.DunningState
	}{
		{"well before expiry stays active", billing.DunningActive, 30, billing.DunningActive},
		{"just outside the 7-day window stays active", billing.DunningActive, 7.5, billing.DunningActive},
		{"exactly 7 days out enters remind_7d", billing.DunningActive, 7, billing.DunningRemind7d},
		{"5 days out is still remind_7d", billing.DunningActive, 5, billing.DunningRemind7d},
		{"exactly 3 days out enters remind_3d", billing.DunningRemind7d, 3, billing.DunningRemind3d},
		{"exactly 1 day out enters remind_1d", billing.DunningRemind3d, 1, billing.DunningRemind1d},
		{"on expiry enters grace", billing.DunningRemind1d, 0, billing.DunningGracePeriod},
		{"1 day overdue is still grace", billing.DunningRemind1d, -1, billing.DunningGracePeriod},
		{"grace ends at 3 days overdue", billing.DunningGracePeriod, -3, billing.DunningSoftSuspended},
		{"5 days overdue is still soft", billing.DunningGracePeriod, -5, billing.DunningSoftSuspended},
		{"soft suspension hardens at 6 days overdue", billing.DunningSoftSuspended, -6, billing.DunningHardSuspended},
		{"long overdue stays hard suspended", billing.DunningHardSuspended, -90, billing.DunningHardSuspended},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := billing.NextDunningState(tc.current, day(tc.expiryDays), scanNow)
			if got != tc.want {
				t.Errorf("expiry %+.1fd from now: want %s, got %s", tc.expiryDays, tc.want, got)
			}
		})
	}
}

// TestFR_BIL_004_NextDunningState_PaymentRestores covers the edge a subscriber
// actually cares about: they paid, so whatever stage they were stuck at, they
// go back to active. Without this a renewing customer stays suspended until
// somebody notices.
func TestFR_BIL_004_NextDunningState_PaymentRestores(t *testing.T) {
	for _, from := range []billing.DunningState{
		billing.DunningGracePeriod, billing.DunningSoftSuspended, billing.DunningHardSuspended,
	} {
		got := billing.NextDunningState(from, day(30), scanNow)
		if got != billing.DunningActive {
			t.Errorf("from %s with a future expiry: want active, got %s", from, got)
		}
	}
}

func TestFR_NOTIF_001_TemplateForDunningState(t *testing.T) {
	cases := []struct {
		state    billing.DunningState
		wantTmpl string
		wantSend bool
	}{
		{billing.DunningRemind7d, "TMPL-003", true},
		{billing.DunningRemind3d, "TMPL-003", true},
		{billing.DunningRemind1d, "TMPL-003", true},
		{billing.DunningGracePeriod, "TMPL-003", true},
		// Separate ids: soft suspension is still recoverable by paying, hard
		// suspension has already cut the line. Both sent TMPL-004 until the
		// spec realignment, so a subscriber actually cut off received the
		// softer of the two warnings.
		{billing.DunningSoftSuspended, "TMPL-004", true},
		{billing.DunningHardSuspended, "TMPL-005", true},
		// Restoration is announced by the renewal scanner, which is where it
		// happens, not from this ladder.
		{billing.DunningActive, "", false},
	}
	for _, tc := range cases {
		gotTmpl, gotSend := billing.TemplateForDunningState(tc.state)
		if gotTmpl != tc.wantTmpl || gotSend != tc.wantSend {
			t.Errorf("%s: want (%q,%v), got (%q,%v)", tc.state, tc.wantTmpl, tc.wantSend, gotTmpl, gotSend)
		}
	}
}

// TestFR_NOTIF_001_DunningNoticeTaskID_ScopedPerCycle pins the idempotency key.
// Without the expiry in it, a subscriber who lapsed, renewed and lapsed again
// would be silently suppressed by the previous cycle's task id and never
// warned a second time.
func TestFR_NOTIF_001_DunningNoticeTaskID_ScopedPerCycle(t *testing.T) {
	first := billing.DunningNoticeTaskID(7, billing.DunningRemind3d, day(0))
	same := billing.DunningNoticeTaskID(7, billing.DunningRemind3d, day(0))
	nextCycle := billing.DunningNoticeTaskID(7, billing.DunningRemind3d, day(30))
	otherStage := billing.DunningNoticeTaskID(7, billing.DunningRemind1d, day(0))
	otherSub := billing.DunningNoticeTaskID(8, billing.DunningRemind3d, day(0))

	if first != same {
		t.Error("the same subscriber, stage and cycle must produce a stable id")
	}
	for name, other := range map[string]string{
		"next billing cycle":   nextCycle,
		"different stage":      otherStage,
		"different subscriber": otherSub,
	} {
		if first == other {
			t.Errorf("%s must produce a different id, got the same", name)
		}
	}
}

// ── Scanner ─────────────────────────────────────────────────────────────────

type scanDB struct {
	candidates []billing.DunningCandidate
	states     map[int]billing.DunningState
	setCalls   []dunningSetCall
	listErr    error
}

func (d *scanDB) ListDunningCandidates(context.Context) ([]billing.DunningCandidate, error) {
	if d.listErr != nil {
		return nil, d.listErr
	}
	return d.candidates, nil
}

func (d *scanDB) GetSubscriberDunningState(_ context.Context, id int) (billing.DunningState, time.Time, error) {
	return d.states[id], time.Time{}, nil
}

func (d *scanDB) SetSubscriberDunningState(_ context.Context, id int, state billing.DunningState, status string) error {
	d.states[id] = state
	d.setCalls = append(d.setCalls, dunningSetCall{state: state, status: status})
	return nil
}

// TestFR_BIL_004_Scanner_AdvancesOneStageAtATime verifies the scanner drives
// the real state machine rather than writing the target state directly. Every
// hop must be a legal edge, which is what stops the ladder being bypassed.
func TestFR_BIL_004_Scanner_AdvancesOneStageAtATime(t *testing.T) {
	db := &scanDB{
		states: map[int]billing.DunningState{1: billing.DunningActive},
		candidates: []billing.DunningCandidate{
			{SubscriberID: 1, Username: "sub1", State: billing.DunningActive, PlanExpiry: day(5)},
		},
	}
	s := billing.NewDunningScanner(db, nil)
	billing.SetScannerClock(s, func() time.Time { return scanNow })

	if err := s.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := db.states[1]; got != billing.DunningRemind7d {
		t.Errorf("want remind_7d, got %s", got)
	}
	if len(db.setCalls) != 1 {
		t.Errorf("want exactly 1 transition, got %d", len(db.setCalls))
	}
}

// TestFR_BIL_004_Scanner_CatchesUpThroughEveryStage is the case that exists
// because dunning never ran: subscribers whose expiry passed weeks ago sit at
// "active" and must climb the whole ladder. Each hop still has to be a legal
// edge, so the scanner steps rather than jumping.
func TestFR_BIL_004_Scanner_CatchesUpThroughEveryStage(t *testing.T) {
	db := &scanDB{
		states: map[int]billing.DunningState{1: billing.DunningActive},
		candidates: []billing.DunningCandidate{
			{SubscriberID: 1, Username: "sub1", State: billing.DunningActive, PlanExpiry: day(-40)},
		},
	}
	s := billing.NewDunningScanner(db, nil)
	billing.SetScannerClock(s, func() time.Time { return scanNow })

	if err := s.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := db.states[1]; got != billing.DunningHardSuspended {
		t.Fatalf("want hard_suspended, got %s", got)
	}

	// active → remind_7d → remind_3d → remind_1d → grace → soft → hard
	if len(db.setCalls) != 6 {
		t.Errorf("want 6 single-stage transitions, got %d", len(db.setCalls))
	}
	// And the subscriber status must end up enforcing the suspension, not just
	// the dunning label.
	if last := db.setCalls[len(db.setCalls)-1]; last.status != "hard_suspended" {
		t.Errorf("final subscriber status: want hard_suspended, got %s", last.status)
	}
}

func TestFR_BIL_004_Scanner_LeavesSettledSubscribersAlone(t *testing.T) {
	db := &scanDB{
		states: map[int]billing.DunningState{1: billing.DunningActive},
		candidates: []billing.DunningCandidate{
			{SubscriberID: 1, Username: "sub1", State: billing.DunningActive, PlanExpiry: day(30)},
		},
	}
	s := billing.NewDunningScanner(db, nil)
	billing.SetScannerClock(s, func() time.Time { return scanNow })

	if err := s.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(db.setCalls) != 0 {
		t.Errorf("a subscriber 30 days from expiry must not be touched, got %d writes", len(db.setCalls))
	}
}

// TestFR_BIL_004_Scanner_RestoresOnPayment covers the path back: expiry moved
// into the future, so a suspended subscriber returns to active and RADIUS
// stops refusing them.
func TestFR_BIL_004_Scanner_RestoresOnPayment(t *testing.T) {
	db := &scanDB{
		states: map[int]billing.DunningState{1: billing.DunningHardSuspended},
		candidates: []billing.DunningCandidate{
			{SubscriberID: 1, Username: "sub1", State: billing.DunningHardSuspended, PlanExpiry: day(28)},
		},
	}
	s := billing.NewDunningScanner(db, nil)
	billing.SetScannerClock(s, func() time.Time { return scanNow })

	if err := s.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got := db.states[1]; got != billing.DunningActive {
		t.Errorf("want active after payment, got %s", got)
	}
	if len(db.setCalls) != 1 || db.setCalls[0].status != "active" {
		t.Errorf("want a single restore to active status, got %+v", db.setCalls)
	}
}

// TestFR_BIL_004_Scanner_OneFailureDoesNotStopTheRun — a single unadvanceable
// subscriber must not stall collections for everyone behind them in the list.
func TestFR_BIL_004_Scanner_OneFailureDoesNotStopTheRun(t *testing.T) {
	db := &scanDB{
		states: map[int]billing.DunningState{
			1: "not_a_real_state", // no legal edge out of this
			2: billing.DunningActive,
		},
		candidates: []billing.DunningCandidate{
			{SubscriberID: 1, Username: "broken", State: "not_a_real_state", PlanExpiry: day(-40)},
			{SubscriberID: 2, Username: "sub2", State: billing.DunningActive, PlanExpiry: day(5)},
		},
	}
	s := billing.NewDunningScanner(db, nil)
	billing.SetScannerClock(s, func() time.Time { return scanNow })

	if err := s.Scan(context.Background()); err != nil {
		t.Fatalf("Scan should not fail the whole run: %v", err)
	}
	if got := db.states[2]; got != billing.DunningRemind7d {
		t.Errorf("the second subscriber must still advance, got %s", got)
	}
}
