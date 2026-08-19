package radius

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
)

// Brute-force rate limiter metrics (FR-SEC-001)
var (
	bruteForceBlocked = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_bruteforce_blocked_total",
		Help: "Authentication requests blocked by brute-force limiter",
	})
)

const (
	MaxFailedAttempts = 10               // block after 10 consecutive failures
	LockoutDuration   = 15 * time.Minute // lockout window
)

// BruteForceGuard enforces per-username attempt limits via an in-process
// fixed counter — see internal/localcache's package doc for why this no
// longer needs to be a shared, network-visible store on a single-machine
// install.
//
// FR: FR-SEC-001 | DDS §5.1
type BruteForceGuard struct {
	counter *localcache.Counter
}

// NewBruteForceGuard constructs a BruteForceGuard backed by counter.
func NewBruteForceGuard(counter *localcache.Counter) *BruteForceGuard {
	return &BruteForceGuard{counter: counter}
}

// Check reports whether username is locked out, and whether any failure counter
// exists at all.
//
// hasFailures lets the caller skip the reset on a successful authentication
// when there is nothing to reset.
func (g *BruteForceGuard) Check(_ context.Context, username string) (blocked, hasFailures bool, err error) {
	if g == nil || g.counter == nil {
		return false, false, nil
	}
	count, exists := g.counter.Get(BruteForceKey(username))
	if !exists {
		return false, false, nil
	}
	if count >= MaxFailedAttempts {
		bruteForceBlocked.Inc()
		return true, true, nil
	}
	return false, true, nil
}

// IsBlocked reports whether username has reached MaxFailedAttempts.
func (g *BruteForceGuard) IsBlocked(ctx context.Context, username string) (bool, error) {
	blocked, _, err := g.Check(ctx, username)
	return blocked, err
}

// RecordFailure increments the failure counter and refreshes its lockout TTL.
func (g *BruteForceGuard) RecordFailure(_ context.Context, username string) error {
	if g == nil || g.counter == nil {
		return nil
	}
	key := BruteForceKey(username)
	g.counter.Incr(key)
	g.counter.Expire(key, LockoutDuration)
	return nil
}

// Reset clears the failure counter after a successful authentication.
func (g *BruteForceGuard) Reset(_ context.Context, username string) error {
	if g == nil || g.counter == nil {
		return nil
	}
	g.counter.Reset(BruteForceKey(username))
	return nil
}
