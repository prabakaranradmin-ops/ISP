// Unit tests for the RADIUS package's pure helpers and the brute-force guard.
//
// Every test in this file previously passed without calling the code it named.
// TestBruteForceKeyFormat compared a string literal to itself; the two
// TestRateLimitSelection_* tests re-implemented the FUP branch inline in the
// test body instead of calling RateLimitForSubscriber; TestDedupKey built its
// key by concatenation and discarded the input it claimed to use via
// `_ = inputOctets`. One even carried the comment "suppress unused import in
// stub file", which is what they were. They reported the package as tested
// while RateLimitForSubscriber and IsBlocked sat at 0% coverage.
//
// They are replaced rather than added to. Dedup key construction is not
// re-tested here because it has no exported function to call — it is built
// inline in handleAccounting and is already covered end to end by
// TestFR_AAA_003_Dedup_DuplicateInterimSkipped in integration_test.go.
package radius_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// newGuard returns a guard and the counter behind it, so a test can seed a
// starting count or inspect the lockout TTL directly. Fully in-process since
// the move off Redis — see internal/localcache's package doc.
func newGuard(t *testing.T) (*radius.BruteForceGuard, *localcache.Counter) {
	t.Helper()
	counter := localcache.NewCounter(time.Hour) // no sweep during a test
	t.Cleanup(counter.Close)
	return radius.NewBruteForceGuard(counter), counter
}

// seedFailures brings a username's counter up to n recorded failures.
func seedFailures(t *testing.T, g *radius.BruteForceGuard, username string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := g.RecordFailure(context.Background(), username); err != nil {
			t.Fatalf("seed failure %d: %v", i, err)
		}
	}
}

func TestFR_SEC_001_BruteForceKeyFormat(t *testing.T) {
	if got := radius.BruteForceKey("testuser"); got != "bf_attempts:testuser" {
		t.Errorf("BruteForceKey: want bf_attempts:testuser, got %q", got)
	}
}

// TestFR_AAA_004_RateLimitForSubscriber covers every branch of the effective
// rate-limit decision, including the one the old tests missed entirely:
// FUPActive with an empty throttle string must fall back to the plan rate
// rather than return "".
func TestFR_AAA_004_RateLimitForSubscriber(t *testing.T) {
	cases := []struct {
		name string
		sub  radius.Subscriber
		want string
	}{
		{
			name: "FUP inactive uses the plan rate",
			sub:  radius.Subscriber{RateLimitStr: "100M/100M", FUPActive: false, FUPThrottle: "10M/10M"},
			want: "100M/100M",
		},
		{
			name: "FUP active uses the throttle",
			sub:  radius.Subscriber{RateLimitStr: "100M/100M", FUPActive: true, FUPThrottle: "10M/10M"},
			want: "10M/10M",
		},
		{
			name: "FUP active with an empty throttle falls back to the plan rate",
			sub:  radius.Subscriber{RateLimitStr: "100M/100M", FUPActive: true, FUPThrottle: ""},
			want: "100M/100M",
		},
		{
			name: "FUP inactive with an empty plan rate returns empty",
			sub:  radius.Subscriber{RateLimitStr: "", FUPActive: false, FUPThrottle: "10M/10M"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := radius.RateLimitForSubscriber(&tc.sub); got != tc.want {
				t.Errorf("RateLimitForSubscriber: want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestFR_SEC_001_BruteForceGuard_IsBlockedAtThreshold pins the boundary: the
// guard must block at exactly MaxFailedAttempts, not one either side.
func TestFR_SEC_001_BruteForceGuard_IsBlockedAtThreshold(t *testing.T) {
	ctx := context.Background()

	t.Run("below the threshold is not blocked", func(t *testing.T) {
		g, _ := newGuard(t)
		seedFailures(t, g, "u", radius.MaxFailedAttempts-1)
		blocked, err := g.IsBlocked(ctx, "u")
		if err != nil {
			t.Fatalf("IsBlocked: %v", err)
		}
		if blocked {
			t.Errorf("must not block at %d failures", radius.MaxFailedAttempts-1)
		}
	})

	t.Run("at the threshold is blocked", func(t *testing.T) {
		g, _ := newGuard(t)
		seedFailures(t, g, "u", radius.MaxFailedAttempts)
		blocked, err := g.IsBlocked(ctx, "u")
		if err != nil {
			t.Fatalf("IsBlocked: %v", err)
		}
		if !blocked {
			t.Errorf("must block at %d failures", radius.MaxFailedAttempts)
		}
	})
}

func TestFR_SEC_001_BruteForceGuard_UnknownUserNotBlocked(t *testing.T) {
	g, _ := newGuard(t)
	blocked, err := g.IsBlocked(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("IsBlocked: %v", err)
	}
	if blocked {
		t.Error("a username with no counter must not be blocked")
	}
}

// TestFR_SEC_001_BruteForceGuard_CheckReportsHasFailures covers the second
// return value, which exists purely to let handleAuth skip a Redis DELETE on
// the hot path when there is nothing to reset.
func TestFR_SEC_001_BruteForceGuard_CheckReportsHasFailures(t *testing.T) {
	ctx := context.Background()
	g, _ := newGuard(t)

	_, hasFailures, err := g.Check(ctx, "u")
	if err != nil {
		t.Fatalf("Check on a clean user: %v", err)
	}
	if hasFailures {
		t.Error("a user with no counter must report hasFailures=false")
	}

	if err := g.RecordFailure(ctx, "u"); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	blocked, hasFailures, err := g.Check(ctx, "u")
	if err != nil {
		t.Fatalf("Check after one failure: %v", err)
	}
	if !hasFailures {
		t.Error("after a recorded failure, hasFailures must be true")
	}
	if blocked {
		t.Error("one failure must not block")
	}
}

// There was a TestFR_SEC_001_BruteForceGuard_CorruptCounterDoesNotLockOut
// here, covering Check's handling of a counter Redis held as a non-numeric
// string ("a corrupt counter must not lock a subscriber out permanently").
// That failure mode no longer exists to test: the counter is an int64 in
// localcache.Counter, so there is no parse step that can fail and no way to
// represent a corrupt value. Removed rather than rewritten into something
// that asserts nothing.

// TestFR_SEC_001_BruteForceGuard_RecordFailureSetsLockoutTTL verifies the
// counter is given the lockout TTL. Without it the entry would live forever
// and a subscriber who failed ten times months apart would be locked out.
func TestFR_SEC_001_BruteForceGuard_RecordFailureSetsLockoutTTL(t *testing.T) {
	g, counter := newGuard(t)
	if err := g.RecordFailure(context.Background(), "u"); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	ttl := counter.TTL(radius.BruteForceKey("u"))
	if ttl <= 0 {
		t.Fatal("the failure counter must carry a TTL")
	}
	if ttl > radius.LockoutDuration {
		t.Errorf("TTL %s exceeds LockoutDuration %s", ttl, radius.LockoutDuration)
	}
}

// TestFR_SEC_001_BruteForceGuard_TTLRefreshedOnEachFailure — the lockout window
// must slide forward with continued attacks rather than expire while one is in
// progress.
func TestFR_SEC_001_BruteForceGuard_TTLRefreshedOnEachFailure(t *testing.T) {
	ctx := context.Background()
	g, counter := newGuard(t)

	if err := g.RecordFailure(ctx, "u"); err != nil {
		t.Fatalf("first failure: %v", err)
	}

	// Real elapsed time, rather than miniredis's FastForward — an in-process
	// counter has no clock to fast-forward. The window is asserted as
	// decay-then-refresh rather than by comparing two TTLs taken moments
	// apart: both would read within microseconds of the full LockoutDuration,
	// so which is larger would come down to measurement noise.
	const elapsed = 100 * time.Millisecond
	time.Sleep(elapsed)

	decayed := counter.TTL(radius.BruteForceKey("u"))
	if decayed >= radius.LockoutDuration {
		t.Fatalf("TTL should have decayed below the full window after %s, got %s", elapsed, decayed)
	}

	if err := g.RecordFailure(ctx, "u"); err != nil {
		t.Fatalf("second failure: %v", err)
	}

	refreshed := counter.TTL(radius.BruteForceKey("u"))
	if refreshed <= decayed {
		t.Errorf("a further failure must push the lockout window back out: %s -> %s", decayed, refreshed)
	}
	if refreshed > radius.LockoutDuration {
		t.Errorf("refreshed TTL %s exceeds LockoutDuration %s", refreshed, radius.LockoutDuration)
	}
}

func TestFR_SEC_001_BruteForceGuard_ResetClearsCounter(t *testing.T) {
	ctx := context.Background()
	g, counter := newGuard(t)

	seedFailures(t, g, "u", radius.MaxFailedAttempts)
	blocked, err := g.IsBlocked(ctx, "u")
	if err != nil || !blocked {
		t.Fatalf("want blocked before reset (blocked=%v err=%v)", blocked, err)
	}

	if err := g.Reset(ctx, "u"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, exists := counter.Get(radius.BruteForceKey("u")); exists {
		t.Error("Reset must delete the counter entry")
	}
	blocked, err = g.IsBlocked(ctx, "u")
	if err != nil {
		t.Fatalf("IsBlocked after reset: %v", err)
	}
	if blocked {
		t.Error("a subscriber must be able to authenticate again after a reset")
	}
}

// TestFR_SEC_001_BruteForceGuard_NilGuardIsInert covers the nil-receiver and
// nil-counter guards. The daemon can be constructed without a guard, and in
// that mode it must degrade to "never blocks" rather than panic on the
// authentication path.
func TestFR_SEC_001_BruteForceGuard_NilGuardIsInert(t *testing.T) {
	ctx := context.Background()

	for name, g := range map[string]*radius.BruteForceGuard{
		"nil guard":   nil,
		"nil counter": radius.NewBruteForceGuard(nil),
	} {
		t.Run(name, func(t *testing.T) {
			blocked, hasFailures, err := g.Check(ctx, "u")
			if err != nil || blocked || hasFailures {
				t.Errorf("Check: want (false,false,nil), got (%v,%v,%v)", blocked, hasFailures, err)
			}
			if err := g.RecordFailure(ctx, "u"); err != nil {
				t.Errorf("RecordFailure: want nil, got %v", err)
			}
			if err := g.Reset(ctx, "u"); err != nil {
				t.Errorf("Reset: want nil, got %v", err)
			}
			blocked, err = g.IsBlocked(ctx, "u")
			if err != nil || blocked {
				t.Errorf("IsBlocked: want (false,nil), got (%v,%v)", blocked, err)
			}
		})
	}
}

// There was a TestFR_SEC_001_BruteForceGuard_RedisDownSurfacesError here,
// asserting that a Redis read failure was reported rather than swallowed
// (silently treating it as "not blocked" would have disabled the lockout
// whenever Redis blipped). With the counter in process there is no network
// call that can fail, so the guard's methods no longer have an error path to
// exercise — they keep returning error only to preserve the interface their
// callers already handle.
