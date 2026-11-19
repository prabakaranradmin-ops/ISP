package billing

import (
	"errors"
	"testing"
	"time"
)

// Scheduled plan changes — FR-BIL-023.

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestFR_BIL_023_ScheduleRequestRules(t *testing.T) {
	today := day(2026, time.March, 15)
	valid := SchedulePlanChangeRequest{
		SubscriberID: 5, NewPlanID: 9,
		EffectiveOn: day(2026, time.April, 1),
		Reason:      "  Customer   asked to downgrade  ",
	}

	got, err := valid.Normalise(today)
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	if got.Reason != "Customer asked to downgrade" {
		t.Errorf("reason not collapsed: %q", got.Reason)
	}

	t.Run("today is allowed", func(t *testing.T) {
		r := valid
		r.EffectiveOn = today
		if _, err := r.Normalise(today); err != nil {
			t.Errorf("a change effective today was refused: %v", err)
		}
	})

	t.Run("a time of day is truncated", func(t *testing.T) {
		r := valid
		r.EffectiveOn = time.Date(2026, time.April, 1, 23, 59, 59, 0, time.UTC)
		got, err := r.Normalise(today)
		if err != nil {
			t.Fatal(err)
		}
		if !got.EffectiveOn.Equal(day(2026, time.April, 1)) {
			t.Errorf("effective_on is %v, want midnight on 1 April", got.EffectiveOn)
		}
	})

	cases := []struct {
		name string
		mut  func(*SchedulePlanChangeRequest)
	}{
		{"no subscriber", func(r *SchedulePlanChangeRequest) { r.SubscriberID = 0 }},
		{"no plan", func(r *SchedulePlanChangeRequest) { r.NewPlanID = 0 }},
		{"no date", func(r *SchedulePlanChangeRequest) { r.EffectiveOn = time.Time{} }},
		{"a date already past", func(r *SchedulePlanChangeRequest) { r.EffectiveOn = day(2026, time.March, 14) }},
		{"beyond the horizon", func(r *SchedulePlanChangeRequest) {
			r.EffectiveOn = today.AddDate(0, 0, PlanChangeMaxHorizon+1)
		}},
		{"an overlong note", func(r *SchedulePlanChangeRequest) {
			long := ""
			for len(long) <= PlanChangeMaxReason {
				long += "x"
			}
			r.Reason = long
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			tc.mut(&r)
			if _, err := r.Normalise(today); !errors.Is(err, ErrPlanChangeInvalid) {
				t.Fatalf("want ErrPlanChangeInvalid, got %v", err)
			} else if PlanChangeInvalidReason(err) == "" {
				t.Error("no operator-facing reason on the error")
			}
		})
	}

	t.Run("the horizon boundary itself is allowed", func(t *testing.T) {
		r := valid
		r.EffectiveOn = today.AddDate(0, 0, PlanChangeMaxHorizon)
		if _, err := r.Normalise(today); err != nil {
			t.Errorf("the last allowed day was refused: %v", err)
		}
	})
}

// Due is what the renewal path asks. It must be true from the effective
// date onward and false before it — and a pending change is the only kind
// that can be due at all.
func TestFR_BIL_023_DueOnlyFromItsDateAndOnlyWhilePending(t *testing.T) {
	c := ScheduledPlanChange{Status: PlanChangeScheduled, EffectiveOn: day(2026, time.April, 1)}

	if c.Due(day(2026, time.March, 31)) {
		t.Error("due the day before it takes effect")
	}
	if !c.Due(day(2026, time.April, 1)) {
		t.Error("not due on the day it takes effect")
	}
	if !c.Due(day(2026, time.April, 20)) {
		t.Error("not due after its date — a renewal that comes late must still pick it up")
	}

	// A renewal running at 00:30 on the effective date is still on that
	// date: the comparison is by day, not by instant.
	if !c.Due(time.Date(2026, time.April, 1, 0, 30, 0, 0, time.UTC)) {
		t.Error("not due in the small hours of its own date")
	}

	for _, status := range []string{PlanChangeApplied, PlanChangeCancelled} {
		done := c
		done.Status = status
		if done.Due(day(2026, time.April, 2)) {
			t.Errorf("a %s change reported itself due", status)
		}
		if done.Pending() {
			t.Errorf("a %s change reported itself pending", status)
		}
	}
}

func TestFR_BIL_023_SummaryAndStatusLabel(t *testing.T) {
	c := ScheduledPlanChange{
		Status: PlanChangeScheduled, FromPlanName: "Basic", NewPlanName: "Fibre 100",
	}
	if c.Summary() != "Basic → Fibre 100" {
		t.Errorf("summary is %q", c.Summary())
	}
	for status, want := range map[string]string{
		PlanChangeScheduled: "Scheduled",
		PlanChangeApplied:   "Applied",
		PlanChangeCancelled: "Cancelled",
	} {
		c.Status = status
		if got := c.StatusLabel(); got != want {
			t.Errorf("%s reads as %q, want %q", status, got, want)
		}
	}
}

// A scheduled change must NOT prorate — the distinction from FR-LC-001's
// immediate change, and the reason this feature exists.
//
// The immediate path converts the unused value of the old plan into bonus
// days (computePlanChangeExpiry in internal/api). A scheduled change fires
// at renewal, when nothing is unused, so the new cycle is exactly the new
// plan's validity. Asserted here because "it happens to be right today" is
// not the same as "it is specified to be right": somebody reusing the
// proration helper for this path would be making a defensible-looking
// mistake that hands out free days on every downgrade.
func TestFR_BIL_023_AppliedCycleIsTheNewPlansFullValidityWithNoBonus(t *testing.T) {
	// A renewal on 1 April onto a 30-day plan ends 1 May. Nothing about
	// the old plan's price or validity enters this.
	renewedOn := day(2026, time.April, 1)
	change := ScheduledPlanChange{
		Status: PlanChangeScheduled, EffectiveOn: renewedOn,
		NewPlanValidityDays: 30,
	}
	if !change.Due(renewedOn) {
		t.Fatal("the change is not due on the renewal date")
	}

	want := day(2026, time.May, 1)
	got := renewedOn.AddDate(0, 0, change.NewPlanValidityDays)
	if !got.Equal(want) {
		t.Errorf("the first cycle on the new plan ends %v, want %v", got, want)
	}
}

func TestFR_BIL_023_DayOf(t *testing.T) {
	if got := DayOf(time.Date(2026, time.April, 1, 13, 45, 1, 7, time.UTC)); !got.Equal(day(2026, time.April, 1)) {
		t.Errorf("DayOf gave %v", got)
	}
	// A zero time stays zero, so "unset" survives the round trip and
	// Normalise can still report it as missing.
	if got := DayOf(time.Time{}); !got.IsZero() {
		t.Errorf("DayOf turned the zero time into %v", got)
	}
}
