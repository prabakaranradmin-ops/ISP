//go:build integration

// Reusable bandwidth profiles — FR-CAT-002 | migration 065.
//
// The property that matters most here is not that a profile can be stored,
// but that a stored profile actually reaches an Access-Accept. A profile the
// console can create and the RADIUS path never reads would pass every test
// in internal/nas and shape nothing — which is the exact failure mode
// internal/nas was created to stop reproducing, and what scripts/check_wiring.sh
// exists to catch elsewhere.
package db_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

func retailBurst() nas.BandwidthProfile {
	return nas.BandwidthProfile{Shaping: nas.Shaping{
		Name:               "Retail burst",
		MaxDownBPS:         50_000_000,
		MaxUpBPS:           25_000_000,
		MinDownBPS:         10_000_000,
		MinUpBPS:           5_000_000,
		BurstDownBPS:       100_000_000,
		BurstUpBPS:         50_000_000,
		BurstThresholdDown: 40_000_000,
		BurstThresholdUp:   20_000_000,
		BurstTimeSeconds:   10,
		Priority:           4,
	}}
}

func TestFR_CAT_002_AProfileRoundTripsThroughTheDatabase(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()
	_ = pool

	created, err := store.CreateBandwidthProfile(ctx, retailBurst(), "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("the created profile has no id")
	}

	got, err := store.BandwidthProfileByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	// Every field, because a profile that loses one silently shapes wrongly:
	// a dropped burst threshold, for instance, bursts forever.
	want := retailBurst()
	for _, c := range []struct {
		field    string
		got, exp int64
	}{
		{"max down", got.MaxDownBPS, want.MaxDownBPS},
		{"max up", got.MaxUpBPS, want.MaxUpBPS},
		{"min down", got.MinDownBPS, want.MinDownBPS},
		{"min up", got.MinUpBPS, want.MinUpBPS},
		{"burst down", got.BurstDownBPS, want.BurstDownBPS},
		{"burst up", got.BurstUpBPS, want.BurstUpBPS},
		{"burst threshold down", got.BurstThresholdDown, want.BurstThresholdDown},
		{"burst threshold up", got.BurstThresholdUp, want.BurstThresholdUp},
	} {
		if c.got != c.exp {
			t.Errorf("%s: stored %d, want %d", c.field, c.got, c.exp)
		}
	}
	if got.BurstTimeSeconds != want.BurstTimeSeconds {
		t.Errorf("burst time: stored %d, want %d", got.BurstTimeSeconds, want.BurstTimeSeconds)
	}
	if got.Priority != want.Priority {
		t.Errorf("priority: stored %d, want %d", got.Priority, want.Priority)
	}
	if !got.Active {
		t.Error("a new profile is not active")
	}
	if got.CreatedBy != "owner" {
		t.Errorf("created_by = %q, want %q", got.CreatedBy, "owner")
	}
}

// The database must refuse what Validate refuses, so the CHECK constraints
// are the authority rather than a restatement nobody tests.
func TestFR_CAT_002_TheDatabaseRefusesAnIncoherentProfile(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	// Inserted as raw SQL, deliberately bypassing Validate: the point is
	// that the table refuses these even when Go does not.
	cases := []struct {
		name   string
		cols   string
		vals   string
		expect string
	}{
		{"a zero ceiling", "name, max_down_bps, max_up_bps",
			"'zero', 0, 1000000", "bandwidth_profiles_max_down_bps_check"},
		{"a floor above the ceiling", "name, max_down_bps, max_up_bps, min_down_bps",
			"'floor', 10000000, 10000000, 20000000", "chk_bw_min_below_max"},
		{"a burst slower than the ceiling",
			"name, max_down_bps, max_up_bps, burst_down_bps, burst_threshold_down, burst_time_seconds",
			"'slow', 50000000, 50000000, 20000000, 10000000, 10", "chk_bw_burst_above_max"},
		{"a burst with no threshold",
			"name, max_down_bps, max_up_bps, burst_down_bps, burst_time_seconds",
			"'nothresh', 50000000, 50000000, 100000000, 10", "chk_bw_burst_complete_down"},
		{"a threshold that can never be crossed",
			"name, max_down_bps, max_up_bps, burst_down_bps, burst_threshold_down, burst_time_seconds",
			"'unreachable', 50000000, 50000000, 100000000, 60000000, 10", "chk_bw_threshold_reachable"},
		{"a priority outside MikroTik's range",
			"name, max_down_bps, max_up_bps, priority",
			"'prio', 10000000, 10000000, 9", "bandwidth_profiles_priority_check"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// One statement per transaction: a failed statement aborts the
			// transaction, so sharing one would let later cases pass on
			// "current transaction is aborted" rather than on the
			// constraint under test.
			_, err := pool.Exec(ctx,
				"INSERT INTO bandwidth_profiles ("+tc.cols+") VALUES ("+tc.vals+")")
			if err == nil {
				t.Fatalf("the database accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("refused for the wrong reason: %v (wanted %q)", err, tc.expect)
			}
		})
	}
}

func TestFR_CAT_002_TwoLiveProfilesCannotShareAName(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()
	_ = pool

	first, err := store.CreateBandwidthProfile(ctx, retailBurst(), "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.CreateBandwidthProfile(ctx, retailBurst(), "owner"); !errors.Is(err, nas.ErrProfileNameTaken) {
		t.Errorf("a duplicate name gave %v, want ErrProfileNameTaken", err)
	}

	// Retiring the first frees the name, which is how a profile is
	// superseded: retire the old, create the replacement, leave the plans
	// on the old one exactly where they are.
	if err := store.SetBandwidthProfileActive(ctx, first.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := store.CreateBandwidthProfile(ctx, retailBurst(), "owner"); err != nil {
		t.Errorf("the name was not freed by retiring: %v", err)
	}
}

func TestFR_CAT_002_AProfileInUseCannotBeDeleted(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()
	_ = pool

	profile, err := store.CreateBandwidthProfile(ctx, retailBurst(), "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	planID := seedPlanForBandwidth(t, pool, 1)
	if err := store.SetPlanBandwidthProfile(ctx, planID, &profile.ID, "owner"); err != nil {
		t.Fatalf("attach: %v", err)
	}

	if err := store.DeleteBandwidthProfile(ctx, profile.ID); !errors.Is(err, nas.ErrProfileInUse) {
		t.Errorf("deleting a profile in use gave %v, want ErrProfileInUse", err)
	}

	// The listing must say how many plans, because that is what the console
	// shows before confirming an edit that re-shapes them.
	profiles, err := store.ListBandwidthProfiles(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, p := range profiles {
		if p.ID == profile.ID {
			found = true
			if p.InUseByPlans != 1 {
				t.Errorf("the listing reports %d plans on the profile, want 1", p.InUseByPlans)
			}
		}
	}
	if !found {
		t.Error("the profile is missing from the listing")
	}

	// Detaching it makes the delete possible, and the plan goes back to
	// shaping from its own rate_limit_string.
	if err := store.SetPlanBandwidthProfile(ctx, planID, nil, "owner"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := store.DeleteBandwidthProfile(ctx, profile.ID); err != nil {
		t.Errorf("delete after detaching: %v", err)
	}
}

// Retiring must leave the plans on the profile shaping exactly as they were.
// A retire that detached them would silently re-shape live subscribers,
// which is the whole reason retiring exists instead of deleting.
//
// Checked through the RADIUS authentication path rather than a dedicated
// read helper. That path is the only thing that decides what a subscriber
// actually gets, and a separate "what does this plan deliver" query would
// be a second implementation of the same question, free to disagree with
// the first — which is how a test passes while production is wrong.
func TestFR_CAT_002_RetiringDoesNotChangeWhatThePlansDeliver(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()
	radiusStore := database.Radius()

	profile, err := store.CreateBandwidthProfile(ctx, retailBurst(), "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	planID := seedPlanForBandwidth(t, pool, 1)
	if err := store.SetPlanBandwidthProfile(ctx, planID, &profile.ID, "owner"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	seedSubscriberOnPlan(t, pool, 1, planID, "retire_user")

	delivered := func(when string) string {
		t.Helper()
		sub, err := radiusStore.GetSubscriberByUsername(ctx, "retire_user")
		if err != nil || sub == nil {
			t.Fatalf("%s: auth lookup: %v", when, err)
		}
		if sub.Shaping == nil {
			t.Fatalf("%s: the subscriber has no shaping", when)
		}
		rl, err := sub.Shaping.MikrotikRateLimit()
		if err != nil {
			t.Fatalf("%s: render: %v", when, err)
		}
		return rl
	}

	before := delivered("before retiring")

	if err := store.SetBandwidthProfileActive(ctx, profile.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	if after := delivered("after retiring"); after != before {
		t.Errorf("retiring changed what the subscriber gets: before %q, after %q", before, after)
	}

	// And it must be gone from the dropdowns.
	live, err := store.ListBandwidthProfiles(ctx, false)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	for _, p := range live {
		if p.ID == profile.ID {
			t.Error("a retired profile is still offered for selection")
		}
	}
	withRetired, err := store.ListBandwidthProfiles(ctx, true)
	if err != nil {
		t.Fatalf("list with retired: %v", err)
	}
	if len(withRetired) != 1 {
		t.Errorf("the retired listing has %d profiles, want 1", len(withRetired))
	}
}

// The wiring that makes the feature real: a profile attached to a plan must
// reach the subscriber the RADIUS path authenticates, and must render to the
// full burst attribute. Without this the feature is inert.
func TestFR_CAT_002_TheProfileReachesTheRadiusAuthenticationPath(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	nasStore := database.NAS()
	radiusStore := database.Radius()

	profile, err := nasStore.CreateBandwidthProfile(ctx, retailBurst(), "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	planID := seedPlanForBandwidth(t, pool, 1)
	if err := nasStore.SetPlanBandwidthProfile(ctx, planID, &profile.ID, "owner"); err != nil {
		t.Fatalf("attach: %v", err)
	}
	seedSubscriberOnPlan(t, pool, 1, planID, "burst_user")

	sub, err := radiusStore.GetSubscriberByUsername(ctx, "burst_user")
	if err != nil {
		t.Fatalf("auth lookup: %v", err)
	}
	if sub == nil {
		t.Fatal("the seeded subscriber does not authenticate")
	}
	if sub.Shaping == nil {
		t.Fatal("the plan's bandwidth profile did not reach the authentication path — " +
			"a profile the console can create and RADIUS never reads shapes nothing")
	}

	rl, err := sub.Shaping.MikrotikRateLimit()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "50M/25M 100M/50M 40M/20M 10/10 4 10M/5M"
	if rl != want {
		t.Errorf("the authentication path would send\n got %q\nwant %q", rl, want)
	}

	// And a subscriber on a plan with no profile must still authenticate,
	// carrying no shaping.
	plainPlan := seedPlanForBandwidth(t, pool, 2)
	seedSubscriberOnPlan(t, pool, 2, plainPlan, "plain_user")
	plain, err := radiusStore.GetSubscriberByUsername(ctx, "plain_user")
	if err != nil || plain == nil {
		t.Fatalf("a subscriber on a profile-less plan does not authenticate: %v", err)
	}
	if plain.Shaping != nil {
		t.Errorf("a profile-less plan produced shaping: %+v", plain.Shaping)
	}
	if plain.RateLimitStr == "" {
		t.Error("a profile-less plan lost its rate_limit_string too")
	}
}

// ── helpers ─────────────────────────────────────────────────────

func seedPlanForBandwidth(t *testing.T, pool *pgxpool.Pool, id int) int {
	t.Helper()
	seedPlan(context.Background(), t, pool, id, fmt.Sprintf("BW test %d", id), "20M/10M", 0, "", "500.00")
	return id
}

func seedSubscriberOnPlan(t *testing.T, pool *pgxpool.Pool, id, planID int, username string) {
	t.Helper()
	seedSubscriber(context.Background(), t, pool, id, seedOpts{Username: username, PlanID: planID})
}
