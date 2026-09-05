package api

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// Proration decides how much of a subscriber's unused money survives a plan
// change, so its edges are worth pinning directly rather than only through an
// HTTP test that cannot control the clock.
//
// computePlanChangeExpiry takes `now`, which makes that possible; the
// integration test goes through the router and cannot, which is how a
// boundary case sat in it unnoticed.

func prorationFixture(t *testing.T, now time.Time, remaining time.Duration) *PlanChangeInfo {
	t.Helper()
	expiry := now.Add(remaining)
	return &PlanChangeInfo{
		Username: "sub", CurrentExpiry: &expiry,
		// 300/30 = 10/day old, 600/30 = 20/day new: every 2 days of old
		// credit buys 1 day of the new plan.
		OldPrice: decimal.NewFromInt(300), OldValidityDays: 30,
		NewPrice: decimal.NewFromInt(600), NewValidityDays: 30,
	}
}

func totalDays(now, expiry time.Time) int { return int(expiry.Sub(now).Hours() / 24) }

// The bonus-day count is floored, and the cliff that produces is sharper than
// it looks: a subscriber with 10 days left gets 5 bonus days, and one with
// 10 days less a single microsecond gets 4.
//
// In production CurrentExpiry is a stored timestamp and now is wall-clock, so
// the remainder is essentially never a whole number of days — meaning the
// lower branch is the one that runs, and a plan change quietly keeps up to a
// full day of the subscriber's paid-for value. On this fixture that is 20
// rupees a change.
//
// Pinned rather than corrected: flooring is defensible (a partial day cannot
// be granted) and MDS 4.14 does not say which way to round, so moving to
// nearest is an operator's decision about customer value, not a bug fix.
func TestComputePlanChangeExpiry_BonusDaysAreFloored(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name      string
		remaining time.Duration
		wantTotal int
	}{
		{"exactly 10 days buys 5 bonus days", 10 * 24 * time.Hour, 35},
		{"a microsecond less costs a whole bonus day", 10*24*time.Hour - time.Microsecond, 34},
		{"11 days is clear of the boundary", 11 * 24 * time.Hour, 35},
		{"no time left means no bonus", 0, 30},
		{"an already-expired plan cannot go negative", -5 * 24 * time.Hour, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := computePlanChangeExpiry(prorationFixture(t, now, tc.remaining), now)
			if d := totalDays(now, got); d != tc.wantTotal {
				t.Errorf("total validity = %d days, want %d", d, tc.wantTotal)
			}
		})
	}
}

// A subscriber with no expiry at all (never activated) must still get the new
// plan's full validity, and no bonus derived from a nil pointer.
func TestComputePlanChangeExpiry_NoCurrentExpiry(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := prorationFixture(t, now, 0)
	info.CurrentExpiry = nil

	if d := totalDays(now, computePlanChangeExpiry(info, now)); d != 30 {
		t.Errorf("total validity = %d days, want the new plan's 30", d)
	}
}

// A free new plan would divide by zero deriving its daily rate; the guard
// must leave the subscriber with the plan's own validity rather than panic.
func TestComputePlanChangeExpiry_ZeroPricedNewPlanDoesNotPanic(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	info := prorationFixture(t, now, 10*24*time.Hour)
	info.NewPrice = decimal.Zero

	if d := totalDays(now, computePlanChangeExpiry(info, now)); d != 30 {
		t.Errorf("total validity = %d days, want 30", d)
	}
}
