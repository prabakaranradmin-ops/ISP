package hotspot

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
)

// Redemption attempt limiting — FR-HSP-001 | MDS §4.23.
//
// The captive portal's redemption endpoints take a bearer credential from an
// unauthenticated stranger. Nothing else stands between a script and the
// voucher space, so this limiter is load-bearing rather than a nicety, and a
// deployment without one refuses to redeem at all (see Handler.limiterReady).

const (
	// DefaultAttemptLimit is the number of redemption attempts one client may
	// make per window. Ten is comfortably above what a person mistyping a
	// printed code needs, and far below what a search of the code space does.
	DefaultAttemptLimit = 10
	// DefaultAttemptWindow is the counting window. Fixed rather than sliding:
	// the worst case is 2x the limit across a window boundary, which does not
	// change the order of magnitude of the search this makes infeasible, and a
	// fixed window costs one increment instead of a sorted-set round trip.
	DefaultAttemptWindow = 15 * time.Minute
)

// Limiter counts attempts per client in an in-process fixed-window counter.
//
// On the shared, multi-process deployment this used to run against, the
// counter had to live in Redis so the limit held across every API replica —
// a per-process counter would have multiplied the real limit by the replica
// count. A single-machine install runs exactly one api process, which
// retires that requirement (see internal/localcache's package doc).
type Limiter struct {
	counter *localcache.Counter
	limit   int
	window  time.Duration
}

// NewLimiter constructs a limiter with the default limit and window.
func NewLimiter() *Limiter {
	return &Limiter{counter: localcache.NewCounter(0), limit: DefaultAttemptLimit, window: DefaultAttemptWindow}
}

// SetLimit overrides the attempts-per-window budget.
func (l *Limiter) SetLimit(limit int, window time.Duration) {
	l.limit, l.window = limit, window
}

// Allow records an attempt for key and reports whether it may proceed.
//
// The attempt is counted before the voucher is checked, not after a failure:
// counting only failures would let an attacker who occasionally guesses right
// keep going indefinitely, and the cost of counting a legitimate user's
// successful redemption is one slot out of ten they will never use again.
//
// An unconfigured limiter returns an error and the caller refuses the
// attempt. That is the deliberate direction to fail: a broken limiter means
// the portal is unmetered, and an unmetered voucher endpoint is worse than
// an unavailable one.
func (l *Limiter) Allow(_ context.Context, key string) (bool, error) {
	if l == nil || l.counter == nil {
		return false, fmt.Errorf("hotspot: attempt limiter is not configured")
	}
	counterKey := "hotspot:attempts:" + key

	count := l.counter.Incr(counterKey)
	// Only on the first attempt in a window, so a burst of attempts cannot keep
	// pushing the expiry out and turn the fixed window into a rolling one that
	// never resets for a legitimate user.
	if count == 1 {
		l.counter.Expire(counterKey, l.window)
	}
	return count <= int64(l.limit), nil
}
