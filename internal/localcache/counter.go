package localcache

import (
	"sync"
	"time"
)

type counterEntry struct {
	count   int64
	expires time.Time // zero means "no expiry set yet"
}

// Counter is a mutex-protected fixed-window counter, replacing the Redis
// INCR+EXPIRE pattern used for rate limiting and brute-force lockouts.
//
// Deliberately split into Incr and Expire, matching Redis's own INCR (never
// touches a key's TTL) and EXPIRE (sets/refreshes it) rather than fusing
// them: the two callers in this codebase need different semantics on top of
// the same primitive — a fixed window that only arms its TTL on the first
// increment (internal/hotspot's redemption limiter), and a sliding lockout
// that refreshes its TTL on every increment (internal/radius's brute-force
// guard) — and a single "IncrAndExpire" method could only implement one of
// them.
type Counter struct {
	mu      sync.Mutex
	entries map[string]counterEntry
	stop    chan struct{}
	stopped sync.Once
}

// NewCounter constructs a Counter with a background sweep goroutine running
// every sweepInterval. A zero or negative sweepInterval uses
// defaultSweepInterval.
func NewCounter(sweepInterval time.Duration) *Counter {
	if sweepInterval <= 0 {
		sweepInterval = defaultSweepInterval
	}
	c := &Counter{
		entries: make(map[string]counterEntry),
		stop:    make(chan struct{}),
	}
	go c.sweepLoop(sweepInterval)
	return c
}

// Incr increments key's counter and returns the new value. A key with no
// live entry (never set, or its previous expiry has passed) starts at 1,
// with no expiry set — callers that want the count to reset after a window
// must call Expire, exactly as with Redis INCR.
func (c *Counter) Incr(key string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || (!e.expires.IsZero() && time.Now().After(e.expires)) {
		e = counterEntry{}
	}
	e.count++
	c.entries[key] = e
	return e.count
}

// Expire sets (or refreshes) key's TTL to ttl from now. A no-op if key has
// no entry.
func (c *Counter) Expire(key string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return
	}
	e.expires = time.Now().Add(ttl)
	c.entries[key] = e
}

// Get returns key's current count, and whether a live entry exists at all.
func (c *Counter) Get(key string) (count int64, exists bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || (!e.expires.IsZero() && time.Now().After(e.expires)) {
		return 0, false
	}
	return e.count, true
}

// Reset removes key's counter entirely.
func (c *Counter) Reset(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Len reports how many live (unexpired) counters are held. Intended for
// tests and diagnostics.
func (c *Counter) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	n := 0
	for _, e := range c.entries {
		if e.expires.IsZero() || now.Before(e.expires) {
			n++
		}
	}
	return n
}

// ExpireNow forces key to be treated as already expired, without deleting
// it outright. This is what a test uses in place of advancing a clock: the
// entry stops being visible to Get and the next Incr restarts the window,
// exactly as a naturally-elapsed TTL would.
func (c *Counter) ExpireNow(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return
	}
	e.expires = time.Now().Add(-time.Second)
	c.entries[key] = e
}

// TTL reports how long remains before key expires. Zero means the key is
// absent, already expired, or has no expiry armed — the three cases callers
// have no reason to distinguish, since none of them is "expiring soon".
// Intended for tests and diagnostics.
func (c *Counter) TTL(key string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || e.expires.IsZero() {
		return 0
	}
	if remaining := time.Until(e.expires); remaining > 0 {
		return remaining
	}
	return 0
}

// Close stops the background sweep goroutine. Safe to call more than once.
func (c *Counter) Close() {
	c.stopped.Do(func() { close(c.stop) })
}

func (c *Counter) sweepLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-ticker.C:
			c.sweep(now)
		}
	}
}

func (c *Counter) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(c.entries, k)
		}
	}
}
