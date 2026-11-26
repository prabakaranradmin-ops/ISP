package billing

import (
	"errors"
	"testing"
	"time"
)

// The effective date of a scheduled plan change, across zones and hours —
// FR-BIL-023.
//
// plan_change_test.go already covers the rules, and could not have caught
// this: it builds both the effective date and "today" in UTC, so the two
// sides always agree about which calendar date they mean.
//
// Production did not. Both handlers parsed effective_on as UTC midnight while
// the store passed time.Now() — local — as today, and DayOf keeps whatever
// location it is given. In IST the two disagree about the date between 00:00
// and 05:30, which is the only window in which it shows: three database tests
// failed the first time the suite ran inside it, and nothing failed during a
// working day. West of UTC the disagreement is permanent and the wrong way
// round, so scheduling a change for today was refused as already passed.
//
// These are the cases the UTC-on-both-sides tests cannot express.

func dayIn(t *testing.T, s string, loc *time.Location) time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return d
}

// The same calendar date is accepted whatever the hour and whatever the zone,
// as long as both sides were produced in that zone — which is what the
// handlers now do.
func TestFR_BIL_023_TheHourAndTheZoneDoNotChangeTheAnswer(t *testing.T) {
	zones := map[string]int{
		"IST, east of UTC":      5*3600 + 1800,
		"UTC":                   0,
		"US east, west of UTC":  -5 * 3600,
		"Pacific, the far side": 13 * 3600,
	}

	for name, offset := range zones {
		t.Run(name, func(t *testing.T) {
			loc := time.FixedZone("test", offset)
			// Every hour that matters: midnight and just after it are where
			// a zone mismatch shows, and midday is the control.
			for _, hour := range []int{0, 2, 5, 12, 23} {
				now := time.Date(2026, 11, 27, hour, 30, 0, 0, loc)

				if _, err := (SchedulePlanChangeRequest{
					SubscriberID: 1, NewPlanID: 2,
					EffectiveOn: dayIn(t, "2026-11-27", loc),
				}).Normalise(now); err != nil {
					t.Errorf("at %02d:30, scheduling for today was refused: %v", hour, err)
				}

				if _, err := (SchedulePlanChangeRequest{
					SubscriberID: 1, NewPlanID: 2,
					EffectiveOn: dayIn(t, "2026-11-28", loc),
				}).Normalise(now); err != nil {
					t.Errorf("at %02d:30, scheduling for tomorrow was refused: %v", hour, err)
				}

				if _, err := (SchedulePlanChangeRequest{
					SubscriberID: 1, NewPlanID: 2,
					EffectiveOn: dayIn(t, "2026-11-26", loc),
				}).Normalise(now); !errors.Is(err, ErrPlanChangeInvalid) {
					t.Errorf("at %02d:30, yesterday was accepted", hour)
				}
			}
		})
	}
}

// Mixing the zones is what broke, so it is worth stating what happens when
// they are mixed: the comparison is not about calendar dates any more.
//
// This documents the trap rather than endorsing it. A caller that produces
// the two sides in different zones gets an answer that depends on the offset
// between them, and the fix is to produce both in one zone — not to make
// DayOf clever.
func TestFR_BIL_023_MixingZonesMakesTheComparisonMeaningless(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)

	// What production used to do: the date parsed as UTC, today taken local.
	effectiveUTC := dayIn(t, "2026-11-27", time.UTC)
	todayIST := time.Date(2026, 11, 27, 2, 0, 0, 0, ist) // 02:00, inside the window

	// 27 Nov 00:00 UTC is 05:30 IST, so against 27 Nov 00:00 IST it is later
	// and this particular case slips through. The reverse direction is the
	// one that bit: a UTC-built "today" against an IST-built date.
	if _, err := (SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: effectiveUTC,
	}).Normalise(todayIST); err != nil {
		t.Errorf("premise check: %v", err)
	}

	// The failing direction, which is what the database fixtures were doing:
	// the date built in UTC while today is local and ahead of it.
	effectiveTodayUTC := dayIn(t, "2026-11-26", time.UTC) // "today" per UTC at 02:00 IST
	if _, err := (SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: effectiveTodayUTC,
	}).Normalise(todayIST); !errors.Is(err, ErrPlanChangeInvalid) {
		t.Error("the premise of this test is that a UTC-built today is refused against " +
			"a local today; if that stopped being true, the zone handling changed and " +
			"the comment above needs revisiting")
	}
}
