package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

func TestFR_BIL_017_PolicyValidate(t *testing.T) {
	ok := billing.DefaultDunningPolicy()
	if err := ok.Validate(); err != nil {
		t.Fatalf("default must be valid: %v", err)
	}
	bad := map[string]billing.DunningPolicy{
		"out of order":     {RemindFirstDays: 3, RemindSecondDays: 7, RemindLastDays: 1, GraceDays: 3, SoftSuspendDays: 3},
		"after expiry":     {RemindFirstDays: 7, RemindSecondDays: 3, RemindLastDays: -1, GraceDays: 3, SoftSuspendDays: 3},
		"first too early":  {RemindFirstDays: 45, RemindSecondDays: 3, RemindLastDays: 1, GraceDays: 3, SoftSuspendDays: 3},
		"grace too long":   {RemindFirstDays: 7, RemindSecondDays: 3, RemindLastDays: 1, GraceDays: 31, SoftSuspendDays: 3},
		"negative suspend": {RemindFirstDays: 7, RemindSecondDays: 3, RemindLastDays: 1, GraceDays: 3, SoftSuspendDays: -1},
	}
	for name, p := range bad {
		if err := p.Validate(); !errors.Is(err, billing.ErrPolicyInvalid) {
			t.Errorf("%s: want ErrPolicyInvalid, got %v", name, err)
		}
	}
}

func TestFR_BIL_017_CustomLadder(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	day := func(d float64) time.Time { return now.Add(time.Duration(d * 24 * float64(time.Hour))) }
	p := billing.DunningPolicy{RemindFirstDays: 10, RemindSecondDays: 5, RemindLastDays: 2, GraceDays: 7, SoftSuspendDays: 5}

	cases := []struct {
		expiry float64
		want   billing.DunningState
	}{
		{11, billing.DunningActive},
		{10, billing.DunningRemind7d}, // first reminder, now at 10 days
		{5, billing.DunningRemind3d},
		{2, billing.DunningRemind1d},
		{-1, billing.DunningGracePeriod},
		{-6.9, billing.DunningGracePeriod}, // a 7-day grace still holds
		{-7, billing.DunningSoftSuspended},
		{-12, billing.DunningHardSuspended},
	}
	for _, tc := range cases {
		if got := billing.NextDunningStateWith(p, billing.DunningActive, day(tc.expiry), now, nil); got != tc.want {
			t.Errorf("expiry %+.1f days: want %s, got %s", tc.expiry, tc.want, got)
		}
	}
	// The shipped ladder at -4 days would already be soft-suspended; this
	// one must not be, which is the whole point of making it configurable.
	if got := billing.NextDunningState(billing.DunningGracePeriod, day(-4), now); got != billing.DunningSoftSuspended {
		t.Errorf("default ladder at -4: want soft_suspended, got %s", got)
	}
}

func TestFR_BIL_016_GraceExtension(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	p := billing.DefaultDunningPolicy()
	expiredLongAgo := now.AddDate(0, 0, -20)
	friday := now.AddDate(0, 0, 4)
	yesterday := now.AddDate(0, 0, -1)

	for _, from := range []billing.DunningState{billing.DunningGracePeriod, billing.DunningSoftSuspended, billing.DunningHardSuspended} {
		if got := billing.NextDunningStateWith(p, from, expiredLongAgo, now, &friday); got != billing.DunningGracePeriod {
			t.Errorf("from %s with grace until Friday: want grace_period, got %s", from, got)
		}
	}
	if got := billing.NextDunningStateWith(p, billing.DunningGracePeriod, expiredLongAgo, now, &yesterday); got != billing.DunningHardSuspended {
		t.Errorf("an expired extension must hand back to the ladder: want hard_suspended, got %s", got)
	}
	// A subscriber who has paid is active whatever extension is left on the row.
	paid := now.AddDate(0, 0, 25)
	if got := billing.NextDunningStateWith(p, billing.DunningGracePeriod, paid, now, &friday); got != billing.DunningActive {
		t.Errorf("paid with grace set: want active, got %s", got)
	}
}

func TestFR_BIL_016_017_ScannerUsesPolicyAndGrace(t *testing.T) {
	now := time.Now()
	friday := now.AddDate(0, 0, 4)
	db := &scanDB{
		states: map[int]billing.DunningState{1: billing.DunningHardSuspended},
		candidates: []billing.DunningCandidate{
			{SubscriberID: 1, State: billing.DunningHardSuspended, PlanExpiry: now.AddDate(0, 0, -20), GraceUntil: &friday},
		},
		policy: &billing.DunningPolicy{RemindFirstDays: 12, RemindSecondDays: 3, RemindLastDays: 1, GraceDays: 3, SoftSuspendDays: 3},
	}
	s := billing.NewDunningScanner(db, nil)
	if err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.gotWindow != 12 {
		t.Errorf("the candidate window must follow the first reminder, got %d", db.gotWindow)
	}
	if db.states[1] != billing.DunningGracePeriod {
		t.Errorf("a grace extension must lift a hard suspension, got %s", db.states[1])
	}

	broken := &scanDB{states: map[int]billing.DunningState{}, policyErr: errors.New("db down")}
	if err := billing.NewDunningScanner(broken, nil).Scan(context.Background()); err != nil {
		t.Fatalf("an unreadable policy must fall back, not fail the pass: %v", err)
	}
	if broken.gotWindow != 7 {
		t.Errorf("fallback window should be the default 7, got %d", broken.gotWindow)
	}
}
