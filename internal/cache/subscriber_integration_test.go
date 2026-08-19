//go:build integration

// Integration tests for the read-through subscriber authentication cache.
//
// This file exists because subscriber.go was entirely uncovered while sitting
// directly on the RADIUS authentication hot path — the one place in this
// codebase where a caching bug is also an authentication bug. A stale or
// wrongly-served entry here does not degrade performance, it authenticates
// somebody it should not.
//
// An internal test package (not cache_test, unlike the session store's tests
// next door) so the cache's own store can be inspected directly. It used to
// run against a real in-process Redis (miniredis) and assert on keys and
// TTLs there; the store is now an in-process map, and reaching it through
// the unexported field is the equivalent — still the real implementation,
// with no fake in between.
package cache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// fakeAuthDB is a radius.DBQuerier that counts lookups, so a test can prove a
// second request was served from cache rather than hitting the database again.
type fakeAuthDB struct {
	mu    sync.Mutex
	sub   *radius.Subscriber
	err   error
	calls int
}

func (f *fakeAuthDB) GetSubscriberByUsername(_ context.Context, _ string) (*radius.Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.sub, nil
}

func (f *fakeAuthDB) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newSubscriberCache(t *testing.T, db radius.DBQuerier, ttl time.Duration) *SubscriberCache {
	t.Helper()
	c := NewSubscriberCache(db, ttl)
	t.Cleanup(c.store.Close)
	return c
}

func sampleAuthSubscriber() *radius.Subscriber {
	return &radius.Subscriber{
		ID:           4242,
		Username:     "sub4242",
		PasswordHash: "$2a$12$abcdefghijklmnopqrstuv",
		Status:       "active",
		RateLimitStr: "100M/100M",
		FUPActive:    false,
		FUPThrottle:  "",
	}
}

// cached reports whether a live entry exists for username.
func cached(c *SubscriberCache, username string) bool {
	_, ok := c.store.Get(SubscriberCacheKey(username))
	return ok
}

func TestFR_AAA_002_SubscriberCacheKeyFormat(t *testing.T) {
	if got := SubscriberCacheKey("sub1"); got != "subscriber:auth:sub1" {
		t.Errorf("key: want subscriber:auth:sub1, got %q", got)
	}
}

// TestFR_AAA_002_SubscriberCache_MissThenHit is the core read-through
// behaviour: the first lookup reaches PostgreSQL, the second must not.
func TestFR_AAA_002_SubscriberCache_MissThenHit(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c := newSubscriberCache(t, db, time.Minute)

	first, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if first == nil || first.ID != 4242 {
		t.Fatalf("first lookup returned %+v", first)
	}
	if db.callCount() != 1 {
		t.Fatalf("want 1 DB call after a cold miss, got %d", db.callCount())
	}
	if !cached(c, "sub4242") {
		t.Fatal("the miss should have populated the cache")
	}

	second, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if db.callCount() != 1 {
		t.Errorf("second lookup must be served from cache; DB calls went to %d", db.callCount())
	}

	// Every auth-relevant field must survive the round trip. PasswordHash in
	// particular: a field silently lost here would make the cached record
	// fail every bcrypt comparison.
	if second.ID != first.ID || second.Username != first.Username ||
		second.PasswordHash != first.PasswordHash || second.Status != first.Status ||
		second.RateLimitStr != first.RateLimitStr || second.FUPActive != first.FUPActive ||
		second.FUPThrottle != first.FUPThrottle {
		t.Errorf("cached record differs from the source:\n first  = %+v\n second = %+v", first, second)
	}
}

// TestFR_AAA_002_SubscriberCache_ThrottledFieldsRoundTrip covers the FUP fields
// specifically, since they drive the rate limit applied to a live session.
func TestFR_AAA_002_SubscriberCache_ThrottledFieldsRoundTrip(t *testing.T) {
	sub := sampleAuthSubscriber()
	sub.FUPActive = true
	sub.FUPThrottle = "2M/2M"
	db := &fakeAuthDB{sub: sub}
	c := newSubscriberCache(t, db, time.Minute)

	if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	got, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("cached lookup: %v", err)
	}
	if !got.FUPActive || got.FUPThrottle != "2M/2M" {
		t.Errorf("FUP fields lost through the cache: FUPActive=%v FUPThrottle=%q", got.FUPActive, got.FUPThrottle)
	}
}

// TestFR_AAA_002_SubscriberCache_CachedRecordIsACopy guards a hazard the JSON
// wire format used to make impossible for free: the cache now stores a Go
// struct rather than serialised bytes, so a caller mutating what it got back
// must not be able to reach into the cached entry and change what the next
// authentication sees.
func TestFR_AAA_002_SubscriberCache_CachedRecordIsACopy(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c := newSubscriberCache(t, db, time.Minute)
	ctx := context.Background()

	first, err := c.GetSubscriberByUsername(ctx, "sub4242")
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	first.Status = "hard_suspended"
	first.PasswordHash = "tampered"

	second, err := c.GetSubscriberByUsername(ctx, "sub4242")
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if second.Status != "active" || second.PasswordHash == "tampered" {
		t.Errorf("a caller's mutation leaked into the cache: %+v", second)
	}
}

// TestFR_AAA_002_SubscriberCache_NegativeEntry verifies an unknown username is
// cached as a negative result, so a flood of requests for a nonexistent user
// cannot become a flood of database queries.
func TestFR_AAA_002_SubscriberCache_NegativeEntry(t *testing.T) {
	db := &fakeAuthDB{sub: nil}
	c := newSubscriberCache(t, db, time.Minute)

	got, err := c.GetSubscriberByUsername(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil for an unknown username, got %+v", got)
	}
	if !cached(c, "ghost") {
		t.Fatal("a negative result must still be cached")
	}

	got2, err := c.GetSubscriberByUsername(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if got2 != nil {
		t.Errorf("cached negative entry must still return nil, got %+v", got2)
	}
	if db.callCount() != 1 {
		t.Errorf("the negative entry should have absorbed the second lookup; DB calls = %d", db.callCount())
	}
}

// TestFR_AAA_002_SubscriberCache_NegativeEntryExpiresFaster pins the shorter
// negative TTL. Without it, a newly provisioned subscriber stays locked out for
// a full TTL because of one lookup that happened before they existed.
func TestFR_AAA_002_SubscriberCache_NegativeEntryExpiresFaster(t *testing.T) {
	const ttl = 60 * time.Second
	db := &fakeAuthDB{sub: nil}
	c := newSubscriberCache(t, db, ttl)

	if _, err := c.GetSubscriberByUsername(context.Background(), "ghost"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	negTTL := c.store.TTL(SubscriberCacheKey("ghost"))
	if negTTL <= 0 || negTTL > ttl/2 {
		t.Errorf("negative entry TTL should be well under the positive TTL (%s), got %s", ttl, negTTL)
	}

	// And the positive TTL for comparison, so this test fails if the two are
	// ever collapsed into one value.
	dbPos := &fakeAuthDB{sub: sampleAuthSubscriber()}
	cPos := newSubscriberCache(t, dbPos, ttl)
	if _, err := cPos.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if posTTL := cPos.store.TTL(SubscriberCacheKey("sub4242")); posTTL <= negTTL {
		t.Errorf("positive TTL (%s) must exceed the negative TTL (%s)", posTTL, negTTL)
	}
}

// TestFR_AAA_002_SubscriberCache_TTLExpiryRefetches proves the TTL is real and
// the entry is reloaded once it lapses.
func TestFR_AAA_002_SubscriberCache_TTLExpiryRefetches(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c := newSubscriberCache(t, db, 30*time.Second)

	if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("prime: %v", err)
	}
	// Stands in for miniredis's FastForward — an in-process store has no
	// simulated clock, and waiting out a real 30 seconds is not a test.
	c.store.ExpireNow(SubscriberCacheKey("sub4242"))

	if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("post-expiry lookup: %v", err)
	}
	if db.callCount() != 2 {
		t.Errorf("an expired entry must be refetched from the DB; DB calls = %d, want 2", db.callCount())
	}
}

// TestFR_AAA_002_SubscriberCache_DefaultTTLApplied covers the zero/negative TTL
// guard in the constructor.
func TestFR_AAA_002_SubscriberCache_DefaultTTLApplied(t *testing.T) {
	for _, ttl := range []time.Duration{0, -5 * time.Second} {
		db := &fakeAuthDB{sub: sampleAuthSubscriber()}
		c := newSubscriberCache(t, db, ttl)
		if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
			t.Fatalf("lookup with ttl=%s: %v", ttl, err)
		}
		got := c.store.TTL(SubscriberCacheKey("sub4242"))
		if got <= 0 || got > DefaultSubscriberTTL {
			t.Errorf("ttl=%s should fall back to DefaultSubscriberTTL (%s), got %s", ttl, DefaultSubscriberTTL, got)
		}
	}
}

// TestFR_AAA_002_SubscriberCache_InvalidateForcesReload covers the path a
// suspension depends on. Without invalidation, a suspended subscriber keeps
// authenticating until the TTL lapses.
func TestFR_AAA_002_SubscriberCache_InvalidateForcesReload(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c := newSubscriberCache(t, db, time.Minute)

	if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if err := c.InvalidateSubscriber(context.Background(), "sub4242"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if cached(c, "sub4242") {
		t.Fatal("invalidate must remove the entry")
	}

	// The reload must observe the *new* status, which is the entire point.
	suspended := sampleAuthSubscriber()
	suspended.Status = "hard_suspended"
	db.mu.Lock()
	db.sub = suspended
	db.mu.Unlock()

	got, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("post-invalidate lookup: %v", err)
	}
	if got.Status != "hard_suspended" {
		t.Errorf("status after invalidation: want hard_suspended, got %q", got.Status)
	}
}

// TestFR_AAA_002_SubscriberCache_InvalidateUnknownIsNotAnError — deleting a
// missing entry is a no-op, and invalidating a subscriber who was never
// cached must not fail the caller that is suspending them.
func TestFR_AAA_002_SubscriberCache_InvalidateUnknownIsNotAnError(t *testing.T) {
	c := newSubscriberCache(t, &fakeAuthDB{sub: sampleAuthSubscriber()}, time.Minute)
	if err := c.InvalidateSubscriber(context.Background(), "never-cached"); err != nil {
		t.Errorf("invalidating an uncached username should succeed, got %v", err)
	}
}

// There were three more tests here, all covering Redis failure modes that no
// longer exist now the cache is in-process:
//
//   - RedisDownFallsThrough: authentication surviving an unreachable Redis.
//     There is no longer a cache backend that can be unreachable; the
//     equivalent guarantee (a cache miss falls through to PostgreSQL) is
//     covered by MissThenHit and TTLExpiryRefetches above.
//   - InvalidateReportsRedisFailure: invalidation reporting rather than
//     swallowing a Redis error. InvalidateSubscriber cannot fail any more —
//     it deletes a map key — and still returns error only to satisfy the
//     interface cmd/api's SubCache wiring expects.
//   - CorruptEntryFallsThrough: a garbage cache entry treated as a miss.
//     Entries are typed structs rather than JSON bytes, so there is no
//     decode step that can fail and no way to store a corrupt one. The
//     hazard that replaced it — a caller mutating a struct the cache still
//     holds — is covered by CachedRecordIsACopy above.

// TestFR_AAA_002_SubscriberCache_DBErrorPropagates — a cache miss plus a real
// database error is a genuine failure and must not be swallowed into a nil
// subscriber, which the caller would read as "no such user".
func TestFR_AAA_002_SubscriberCache_DBErrorPropagates(t *testing.T) {
	wantErr := errors.New("connection refused")
	db := &fakeAuthDB{err: wantErr}
	c := newSubscriberCache(t, db, time.Minute)

	got, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if !errors.Is(err, wantErr) {
		t.Fatalf("want the DB error propagated, got %v", err)
	}
	if got != nil {
		t.Errorf("want nil subscriber alongside the error, got %+v", got)
	}
	if cached(c, "sub4242") {
		t.Error("a failed DB lookup must not be cached")
	}
}
