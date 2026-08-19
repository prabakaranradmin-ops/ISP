package localcache

import (
	"testing"
	"time"
)

func TestCounter_IncrStartsAtOne(t *testing.T) {
	c := NewCounter(time.Hour)
	defer c.Close()

	if got := c.Incr("k"); got != 1 {
		t.Fatalf("first Incr = %d, want 1", got)
	}
	if got := c.Incr("k"); got != 2 {
		t.Fatalf("second Incr = %d, want 2", got)
	}
}

func TestCounter_IncrDoesNotSetExpiry(t *testing.T) {
	c := NewCounter(time.Hour)
	defer c.Close()

	c.Incr("k")
	// Mirrors Redis INCR: no TTL is armed until Expire is called explicitly.
	// Get must still see the entry as live (a bare Incr with no Expire never
	// expires on its own — matches Redis's own behavior for a bare INCR).
	got, exists := c.Get("k")
	if !exists || got != 1 {
		t.Fatalf("Get(k) = (%d, %v), want (1, true)", got, exists)
	}
}

func TestCounter_ExpireResetsWindow(t *testing.T) {
	c := NewCounter(time.Hour)
	defer c.Close()

	c.Incr("k")
	c.Expire("k", -time.Second) // already-passed expiry
	if _, exists := c.Get("k"); exists {
		t.Fatal("Get must not return a counter past its expiry")
	}
	// The next Incr after expiry must restart the window at 1, not continue
	// the old count — this is the fixed-window semantics
	// internal/hotspot/limiter.go depends on.
	if got := c.Incr("k"); got != 1 {
		t.Fatalf("Incr after expiry = %d, want 1 (window restarted)", got)
	}
}

func TestCounter_ExpireOnFirstIncrOnly_FixedWindow(t *testing.T) {
	// Reproduces internal/hotspot/limiter.go's exact pattern: Expire is only
	// called when Incr returns 1, so a burst of attempts cannot keep pushing
	// the expiry out.
	c := NewCounter(time.Hour)
	defer c.Close()

	if got := c.Incr("k"); got == 1 {
		c.Expire("k", 50*time.Millisecond)
	}
	c.Incr("k") // count 2, must NOT re-arm the expiry
	c.Incr("k") // count 3, must NOT re-arm the expiry

	time.Sleep(80 * time.Millisecond)
	if _, exists := c.Get("k"); exists {
		t.Fatal("a fixed window must expire on its original schedule regardless of later increments")
	}
}

func TestCounter_ExpireOnEveryIncr_SlidingLockout(t *testing.T) {
	// Reproduces internal/radius/ratelimit.go's exact pattern: Expire is
	// called after every Incr, so each new failure pushes the lockout out.
	c := NewCounter(time.Hour)
	defer c.Close()

	c.Incr("k")
	c.Expire("k", 200*time.Millisecond)

	time.Sleep(100 * time.Millisecond)
	c.Incr("k") // refreshes the expiry to 200ms from *this* point
	c.Expire("k", 200*time.Millisecond)

	time.Sleep(150 * time.Millisecond) // 250ms since the first Incr, but only 150ms since the refresh
	if got, exists := c.Get("k"); !exists || got != 2 {
		t.Fatalf("Get(k) = (%d, %v), want (2, true) — the refreshed expiry must still be live", got, exists)
	}
}

func TestCounter_Reset(t *testing.T) {
	c := NewCounter(time.Hour)
	defer c.Close()

	c.Incr("k")
	c.Incr("k")
	c.Reset("k")
	if _, exists := c.Get("k"); exists {
		t.Fatal("Get must not return a reset counter")
	}
	if got := c.Incr("k"); got != 1 {
		t.Fatalf("Incr after Reset = %d, want 1", got)
	}
}

func TestCounter_GetOnMissingKey(t *testing.T) {
	c := NewCounter(time.Hour)
	defer c.Close()

	if _, exists := c.Get("nope"); exists {
		t.Fatal("Get on a never-incremented key must report false")
	}
}

func TestCounter_SweepReclaimsExpiredEntries(t *testing.T) {
	c := NewCounter(20 * time.Millisecond)
	defer c.Close()

	c.Incr("k")
	c.Expire("k", -time.Second)
	time.Sleep(80 * time.Millisecond)
	if _, exists := c.Get("k"); exists {
		t.Fatal("swept counter must not be returned")
	}
}

func TestCounter_ConcurrentIncr(t *testing.T) {
	c := NewCounter(time.Hour)
	defer c.Close()

	const n = 100
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			c.Incr("k")
			done <- struct{}{}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
	got, exists := c.Get("k")
	if !exists || got != n {
		t.Fatalf("Get(k) = (%d, %v), want (%d, true) after %d concurrent Incr calls", got, exists, n, n)
	}
}

func TestCounter_CloseIsIdempotent(t *testing.T) {
	c := NewCounter(time.Hour)
	c.Close()
	c.Close() // must not panic
}
