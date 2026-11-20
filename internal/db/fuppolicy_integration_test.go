//go:build integration

// Multi-tier fair-use policies and data top-ups — FR-CAT-003 | migration 067.
//
// The property that matters most is the one the old single boolean made
// impossible: a subscriber who crosses a second threshold must be moved to
// the harder tier. Under the previous model they were already flagged
// throttled, the scanner skipped anyone already throttled, and every tier
// after the first was unreachable — a policy that looked configured and had
// one step.
package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
)

const tbGB = int64(1) << 30

func threeTierPolicy() fup.Policy {
	return fup.Policy{
		Name: "Retail fair use",
		Tiers: []fup.Tier{
			{ThresholdBytes: 500 * tbGB, ThrottleRate: "10M/10M"},
			{ThresholdBytes: 1000 * tbGB, ThrottleRate: "2M/2M"},
			{ThresholdBytes: 2000 * tbGB, ThrottleRate: "512k/512k"},
		},
	}
}

func TestFR_CAT_003_APolicyRoundTripsWithItsTiersInOrder(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.FUP()

	// Deliberately out of order on the way in: an operator types tiers in
	// whatever order they think of them.
	in := fup.Policy{Name: "Jumbled", Tiers: []fup.Tier{
		{ThresholdBytes: 2000 * tbGB, ThrottleRate: "512k/512k"},
		{ThresholdBytes: 500 * tbGB, ThrottleRate: "10M/10M"},
		{ThresholdBytes: 1000 * tbGB, ThrottleRate: "2M/2M"},
	}}

	saved, err := store.SaveFUPPolicy(ctx, in, "owner")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(saved.Tiers) != 3 {
		t.Fatalf("saved %d tiers, want 3", len(saved.Tiers))
	}
	// Ascending, because Policy.TierFor relies on the order rather than
	// sorting on every scanner pass.
	for i := 1; i < len(saved.Tiers); i++ {
		if saved.Tiers[i].ThresholdBytes <= saved.Tiers[i-1].ThresholdBytes {
			t.Errorf("tiers came back unsorted: %+v", saved.Tiers)
		}
	}
	if saved.CreatedBy != "owner" {
		t.Errorf("created_by = %q", saved.CreatedBy)
	}

	// Saving again replaces the tiers rather than adding to them.
	again := *saved
	again.Tiers = []fup.Tier{{ThresholdBytes: 100 * tbGB, ThrottleRate: "1M/1M"}}
	replaced, err := store.SaveFUPPolicy(ctx, again, "owner")
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if len(replaced.Tiers) != 1 {
		t.Errorf("after replacing, the policy has %d tiers: %+v", len(replaced.Tiers), replaced.Tiers)
	}
}

func TestFR_CAT_003_TheDatabaseRefusesAnIncoherentTier(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	policy, err := database.FUP().SaveFUPPolicy(ctx, threeTierPolicy(), "owner")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	cases := []struct {
		name   string
		cols   string
		vals   string
		expect string
	}{
		{"a zero threshold", "threshold_bytes, throttle_rate", "0, '1M/1M'", "threshold_bytes_check"},
		{"a negative threshold", "threshold_bytes, throttle_rate", "-1, '1M/1M'", "threshold_bytes_check"},
		{"an empty rate", "threshold_bytes, throttle_rate", "123, '   '", "throttle_rate_check"},
		// Two tiers at one threshold would both apply, and which throttle a
		// subscriber received would depend on row order.
		{"a duplicate threshold", "threshold_bytes, throttle_rate",
			"536870912000, '1M/1M'", "uq_fup_tier_threshold"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One statement per transaction: a failed statement aborts the
			// transaction, so sharing one would let later cases pass on
			// "current transaction is aborted" rather than on the
			// constraint under test.
			_, err := pool.Exec(ctx,
				"INSERT INTO fup_policy_tiers (policy_id, "+tc.cols+") VALUES ($1, "+tc.vals+")",
				policy.ID)
			if err == nil {
				t.Fatalf("the database accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("refused for the wrong reason: %v (wanted %q)", err, tc.expect)
			}
		})
	}
}

// The defect the old model could not express. A subscriber already throttled
// to the first tier, now past the second threshold, must be moved to the
// harder rate.
func TestFR_CAT_003_ASubscriberCrossingASecondThresholdIsMovedToTheHarderTier(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.FUP()

	policy, err := store.SaveFUPPolicy(ctx, threeTierPolicy(), "owner")
	if err != nil {
		t.Fatalf("save policy: %v", err)
	}
	// The plan's own legacy columns are set too, and must be ignored in
	// favour of the policy — otherwise attaching a policy would change
	// nothing.
	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", 9999*tbGB, "99M/99M", "799.00")
	if err := store.SetPlanFUPPolicy(ctx, 1, &policy.ID, "owner"); err != nil {
		t.Fatalf("attach policy: %v", err)
	}
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "heavy@isp", PlanID: 1})

	// Just past the first threshold.
	seedSession(ctx, t, pool, 1, "s-1", "10.10.0.1", 501*tbGB, 0)

	changes, err := store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 1 || changes[0].TargetRate != "10M/10M" {
		t.Fatalf("first threshold: got %+v, want one change to 10M/10M", changes)
	}
	if err := store.SetFUPAppliedRate(ctx, 1, changes[0].TargetRate); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Nothing more to do while they stay in the first band.
	changes, err = store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("a subscriber already on the right tier was re-selected: %+v", changes)
	}

	// Now past the second threshold. This is the case the boolean missed.
	if _, err := pool.Exec(ctx, `
		UPDATE subscriber_session_history SET input_octets = $1 WHERE session_id = 's-1'`,
		1001*tbGB); err != nil {
		t.Fatalf("grow usage: %v", err)
	}

	changes, err = store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("crossing the second threshold produced %d changes, want 1: %+v", len(changes), changes)
	}
	got := changes[0]
	if got.TargetRate != "2M/2M" {
		t.Errorf("target rate %q, want the second tier 2M/2M", got.TargetRate)
	}
	if got.AppliedRate != "10M/10M" {
		t.Errorf("applied rate %q, want the first tier", got.AppliedRate)
	}
	if !got.IsTightening() {
		t.Error("moving from one tier to a harder one is not reported as a tightening")
	}

	// And past the third.
	if _, err := pool.Exec(ctx, `
		UPDATE subscriber_session_history SET input_octets = $1 WHERE session_id = 's-1'`,
		5000*tbGB); err != nil {
		t.Fatalf("grow usage: %v", err)
	}
	if err := store.SetFUPAppliedRate(ctx, 1, "2M/2M"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	changes, err = store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 1 || changes[0].TargetRate != "512k/512k" {
		t.Errorf("far past the last threshold: got %+v, want 512k/512k", changes)
	}
}

// A top-up raises the thresholds, so a throttled subscriber is released —
// and every tier stays live above the new thresholds.
func TestFR_CAT_003_ATopupReleasesTheThrottleAndKeepsTheTiersLive(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.FUP()

	policy, err := store.SaveFUPPolicy(ctx, threeTierPolicy(), "owner")
	if err != nil {
		t.Fatalf("save policy: %v", err)
	}
	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", 0, "", "799.00")
	if err := store.SetPlanFUPPolicy(ctx, 1, &policy.ID, "owner"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "topup@isp", PlanID: 1})
	seedSession(ctx, t, pool, 1, "s-1", "10.10.0.1", 550*tbGB, 0)

	// Throttled to the first tier.
	if err := store.SetFUPAppliedRate(ctx, 1, "10M/10M"); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Buy 100GB.
	if _, err := store.AddDataTopup(ctx, fup.Topup{
		SubscriberID: 1, Bytes: 100 * tbGB,
		ExpiresAt: time.Now().Add(14 * 24 * time.Hour),
	}, "subscriber"); err != nil {
		t.Fatalf("add top-up: %v", err)
	}

	changes, err := store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("a top-up produced %d changes, want 1 release: %+v", len(changes), changes)
	}
	if !changes[0].IsRelease() {
		t.Errorf("the top-up did not release the throttle: target %q", changes[0].TargetRate)
	}
	if changes[0].ToppedUpBytes != 100*tbGB {
		t.Errorf("topped-up bytes reported as %d, want %d", changes[0].ToppedUpBytes, 100*tbGB)
	}
	if err := store.SetFUPAppliedRate(ctx, 1, ""); err != nil {
		t.Fatalf("release: %v", err)
	}

	// The tier is still there, 100GB further out — this is what
	// distinguishes added allowance from an exemption. Using 100GB more
	// throttles them again.
	if _, err := pool.Exec(ctx, `
		UPDATE subscriber_session_history SET input_octets = $1 WHERE session_id = 's-1'`,
		650*tbGB); err != nil {
		t.Fatalf("grow usage: %v", err)
	}
	changes, err = store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 1 || changes[0].TargetRate != "10M/10M" {
		t.Errorf("after using the top-up the subscriber is not throttled again: %+v", changes)
	}
}

// An expired top-up must stop counting, or last cycle's purchase keeps this
// cycle's subscriber un-throttled.
func TestFR_CAT_003_AnExpiredTopupStopsCounting(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.FUP()

	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", 500*tbGB, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "expired@isp", PlanID: 1})
	seedSession(ctx, t, pool, 1, "s-1", "10.10.0.1", 550*tbGB, 0)

	// Inserted directly: AddDataTopup refuses an expiry in the past, which
	// is the behaviour under test one layer up.
	if _, err := pool.Exec(ctx, `
		INSERT INTO data_topups (subscriber_id, bytes, expires_at, purchased_by, purchased_at)
		VALUES (1, $1, NOW() - interval '1 day', 'subscriber', NOW() - interval '31 days')`,
		100*tbGB); err != nil {
		t.Fatalf("seed expired top-up: %v", err)
	}

	total, err := store.ActiveTopupBytes(ctx, 1)
	if err != nil {
		t.Fatalf("active bytes: %v", err)
	}
	if total != 0 {
		t.Errorf("an expired top-up counts %d bytes toward the allowance", total)
	}

	changes, err := store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 1 || changes[0].TargetRate != "10M/10M" {
		t.Errorf("an expired top-up still lifts the throttle: %+v", changes)
	}

	// And the store refuses to create one that is already expired.
	if _, err := store.AddDataTopup(ctx, fup.Topup{
		SubscriberID: 1, Bytes: tbGB, ExpiresAt: time.Now().Add(-time.Hour),
	}, "staff"); err == nil {
		t.Error("the store accepted a top-up that had already expired")
	}
	if _, err := store.AddDataTopup(ctx, fup.Topup{
		SubscriberID: 1, Bytes: 0, ExpiresAt: time.Now().Add(time.Hour),
	}, "staff"); err == nil {
		t.Error("the store accepted a zero-byte top-up")
	}
}

// A manual speed override is a deliberate act and outranks fair use, so a
// subscriber under one must be left alone entirely — not have their override
// overwritten on the next 10-second tick.
func TestFR_CAT_003_ASpeedOverrideOutranksFairUse(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.FUP()

	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", 500*tbGB, "10M/10M", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "override@isp", PlanID: 1})
	seedSession(ctx, t, pool, 1, "s-1", "10.10.0.1", 900*tbGB, 0)

	if _, err := pool.Exec(ctx, `
		UPDATE subscribers
		   SET speed_override_rate_limit = '200M/200M',
		       speed_override_expires_at = NOW() + interval '1 hour'
		 WHERE id = 1`); err != nil {
		t.Fatalf("set override: %v", err)
	}

	changes, err := store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("a subscriber under a speed override was selected for a throttle change: %+v", changes)
	}

	// Once the override lapses, fair use applies again.
	if _, err := pool.Exec(ctx, `
		UPDATE subscribers SET speed_override_expires_at = NOW() - interval '1 minute' WHERE id = 1`); err != nil {
		t.Fatalf("expire override: %v", err)
	}
	changes, err = store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 1 {
		t.Errorf("after the override lapsed, fair use did not apply: %+v", changes)
	}
}

// A plan with no policy must behave exactly as it did: its own single
// threshold and throttle, expressed through the same query.
func TestFR_CAT_003_APlanWithNoPolicyBehavesAsBefore(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.FUP()

	seedPlan(ctx, t, pool, 1, "Legacy", "100M/100M", 500*tbGB, "10M/10M", "799.00")
	seedPlan(ctx, t, pool, 2, "Unlimited", "100M/100M", 0, "", "999.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "legacy@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "unlimited@isp", PlanID: 2})
	seedSession(ctx, t, pool, 1, "s-1", "10.10.0.1", 600*tbGB, 0)
	seedSession(ctx, t, pool, 2, "s-2", "10.10.0.2", 9000*tbGB, 0)

	changes, err := store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("want only the metered subscriber, got %d: %+v", len(changes), changes)
	}
	if changes[0].SubscriberID != 1 {
		t.Errorf("selected subscriber %d, want 1", changes[0].SubscriberID)
	}
	if changes[0].TargetRate != "10M/10M" {
		t.Errorf("target rate %q, want the plan's own throttle", changes[0].TargetRate)
	}
}

// Retiring a policy must leave the plans on it evaluating against it
// unchanged, and a policy plans still use cannot be deleted at all.
func TestFR_CAT_003_RetiringAPolicyDoesNotChangeWhatThePlansEnforce(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.FUP()

	policy, err := store.SaveFUPPolicy(ctx, threeTierPolicy(), "owner")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	seedPlan(ctx, t, pool, 1, "Metered", "100M/100M", 0, "", "799.00")
	if err := store.SetPlanFUPPolicy(ctx, 1, &policy.ID, "owner"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "retire@isp", PlanID: 1})
	seedSession(ctx, t, pool, 1, "s-1", "10.10.0.1", 1100*tbGB, 0)

	before, err := store.SessionsNeedingThrottleChange(ctx)
	if err != nil || len(before) != 1 {
		t.Fatalf("precondition: %+v, %v", before, err)
	}

	if err := store.DeleteFUPPolicy(ctx, policy.ID); !errors.Is(err, fup.ErrPolicyInUse) {
		t.Errorf("deleting a policy in use gave %v, want ErrPolicyInUse", err)
	}

	if err := store.SetFUPPolicyActive(ctx, policy.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	after, err := store.SessionsNeedingThrottleChange(ctx)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(after) != 1 || after[0].TargetRate != before[0].TargetRate {
		t.Errorf("retiring changed what the plan enforces: before %+v, after %+v", before, after)
	}

	// Out of the dropdowns, though.
	live, err := store.ListFUPPolicies(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, p := range live {
		if p.ID == policy.ID {
			t.Error("a retired policy is still offered for selection")
		}
	}

	// Detaching it makes the delete possible.
	if err := store.SetPlanFUPPolicy(ctx, 1, nil, "owner"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := store.DeleteFUPPolicy(ctx, policy.ID); err != nil {
		t.Errorf("delete after detaching: %v", err)
	}
	// Its tiers go with it.
	var tiers int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM fup_policy_tiers WHERE policy_id = $1`, policy.ID).Scan(&tiers); err != nil {
		t.Fatalf("count tiers: %v", err)
	}
	if tiers != 0 {
		t.Errorf("%d tiers outlived their policy", tiers)
	}
}

func TestFR_CAT_003_TwoLivePoliciesCannotShareAName(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.FUP()

	first, err := store.SaveFUPPolicy(ctx, threeTierPolicy(), "owner")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := store.SaveFUPPolicy(ctx, threeTierPolicy(), "owner"); !errors.Is(err, fup.ErrPolicyNameTaken) {
		t.Errorf("a duplicate name gave %v, want ErrPolicyNameTaken", err)
	}

	// Retiring frees the name, which is how a policy is superseded.
	if err := store.SetFUPPolicyActive(ctx, first.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := store.SaveFUPPolicy(ctx, threeTierPolicy(), "owner"); err != nil {
		t.Errorf("the name was not freed by retiring: %v", err)
	}
}
