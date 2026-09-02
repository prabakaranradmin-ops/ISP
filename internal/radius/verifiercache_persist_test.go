// Tests for the fast-verifier cache's persistent L2 tier (migration 046).
//
// Internal package: the tiers' interaction is the subject, and both the
// store field and verifierKey are unexported.
package radius

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
)

// fakeVerifierStore is an in-memory VerifierStore that records calls, so a
// test can assert what reached L2 without a database.
type fakeVerifierStore struct {
	rows      []PersistedVerifier
	saved     int
	deleted   []string
	loadErr   error
	saveErr   error
	deleteErr error
}

func (f *fakeVerifierStore) LoadActive(context.Context) ([]PersistedVerifier, error) {
	return f.rows, f.loadErr
}

func (f *fakeVerifierStore) Save(_ context.Context, _ int, username string, verifier []byte, expiresAt time.Time) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved++
	f.rows = append(f.rows, PersistedVerifier{Username: username, Verifier: verifier, ExpiresAt: expiresAt})
	return nil
}

func (f *fakeVerifierStore) DeleteByUsername(_ context.Context, username string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, username)
	return nil
}

func newTestVerifierCache(store VerifierStore) *VerifierCache {
	c := NewVerifierCache(localcache.New[[]byte](0), []byte("test-verifier-secret-at-least-32-bytes"))
	if store != nil {
		c.SetPersistence(store)
	}
	return c
}

// TestVerifierCache_SurvivesRestart is the point of the whole L2 tier: a
// verifier stored by one process instance is usable by the next, so a
// restart does not force every reconnecting subscriber back through bcrypt.
func TestVerifierCache_SurvivesRestart(t *testing.T) {
	ctx := context.Background()
	store := &fakeVerifierStore{}

	// First "process": authenticate once, paying bcrypt, and cache the result.
	before := newTestVerifierCache(store)
	if err := before.Store(ctx, 42, "alice", "hunter2", "$2a$12$examplehash"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if store.saved != 1 {
		t.Fatalf("verifier was not written through to L2: saved=%d", store.saved)
	}

	// Second "process": fresh in-memory cache, same backing store.
	after := newTestVerifierCache(store)
	restored, err := after.Warm(ctx)
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if restored != 1 {
		t.Fatalf("warmup restored %d entries, want 1", restored)
	}

	ok, err := after.Check(ctx, "alice", "hunter2", "$2a$12$examplehash")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !ok {
		t.Error("a warmed verifier did not match — the restart would have cost a full bcrypt, " +
			"which is the outage this tier exists to prevent")
	}
}

// TestVerifierCache_WarmDoesNotExtendLifetime guards a subtle way this
// could go wrong: restoring entries with a *fresh* TTL rather than their
// remaining one would let a verifier outlive its expiry indefinitely, so
// long as the process restarted often enough. That would turn a bounded
// 5-minute window into an unbounded one.
func TestVerifierCache_WarmDoesNotExtendLifetime(t *testing.T) {
	ctx := context.Background()
	store := &fakeVerifierStore{
		rows: []PersistedVerifier{{
			Username:  "bob",
			Verifier:  []byte("0123456789abcdef0123456789abcdef"),
			ExpiresAt: time.Now().Add(2 * time.Second), // nearly expired
		}},
	}

	c := newTestVerifierCache(store)
	if _, err := c.Warm(ctx); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	ttl := c.store.TTL(verifierKey("bob"))
	if ttl <= 0 {
		t.Fatal("warmed entry has no TTL")
	}
	if ttl > 3*time.Second {
		t.Errorf("warmup extended the verifier's life to %s from ~2s remaining — "+
			"a restart loop could then keep a verifier alive indefinitely", ttl)
	}
}

// TestVerifierCache_WarmSkipsAlreadyExpired — a row can expire between the
// query and the loop. Restoring it would put a verifier into L1 that the
// database had already stopped considering valid.
func TestVerifierCache_WarmSkipsAlreadyExpired(t *testing.T) {
	ctx := context.Background()
	store := &fakeVerifierStore{
		rows: []PersistedVerifier{
			{Username: "stale", Verifier: []byte("0123456789abcdef0123456789abcdef"), ExpiresAt: time.Now().Add(-time.Second)},
			{Username: "fresh", Verifier: []byte("fedcba9876543210fedcba9876543210"), ExpiresAt: time.Now().Add(time.Minute)},
		},
	}

	c := newTestVerifierCache(store)
	restored, err := c.Warm(ctx)
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if restored != 1 {
		t.Errorf("restored %d, want 1 — the expired row should have been skipped", restored)
	}
	if _, ok := c.store.Get(verifierKey("stale")); ok {
		t.Error("an already-expired verifier was loaded into the in-memory cache")
	}
}

// TestVerifierCache_InvalidateClearsBothTiers covers what the hash binding
// cannot. A password change self-invalidates (the verifier is bound to the
// bcrypt hash), but a suspension or termination leaves the hash untouched —
// so without an explicit delete, the fast path would keep answering for an
// account that was just cut off.
func TestVerifierCache_InvalidateClearsBothTiers(t *testing.T) {
	ctx := context.Background()
	store := &fakeVerifierStore{}
	c := newTestVerifierCache(store)

	if err := c.Store(ctx, 7, "carol", "pw", "$2a$12$hash"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if ok, _ := c.Check(ctx, "carol", "pw", "$2a$12$hash"); !ok {
		t.Fatal("precondition failed: verifier not usable before invalidation")
	}

	if err := c.Invalidate(ctx, "carol"); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}

	if ok, _ := c.Check(ctx, "carol", "pw", "$2a$12$hash"); ok {
		t.Error("verifier still matched after invalidation — a suspended subscriber " +
			"would keep authenticating on the fast path")
	}
	if len(store.deleted) != 1 || store.deleted[0] != "carol" {
		t.Errorf("L2 delete not issued: %v", store.deleted)
	}
}

// TestVerifierCache_PasswordChangeSelfInvalidates pins the property the
// design leans on, so a future refactor that dropped passwordHash from the
// HMAC input would fail here rather than silently accepting old passwords
// for a TTL.
func TestVerifierCache_PasswordChangeSelfInvalidates(t *testing.T) {
	ctx := context.Background()
	c := newTestVerifierCache(&fakeVerifierStore{})

	if err := c.Store(ctx, 1, "dave", "oldpw", "$2a$12$oldhash"); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Same cached entry, but the subscriber's hash has since changed.
	if ok, _ := c.Check(ctx, "dave", "oldpw", "$2a$12$NEWhash"); ok {
		t.Error("a verifier cached against the old hash matched under the new one — " +
			"the old password would keep working until the TTL lapsed")
	}
}

// TestVerifierCache_L2FailuresDoNotBreakAuthentication — the cache is an
// optimisation. If its backing store is unavailable, authentication must
// degrade to bcrypt rather than fail, or a database hiccup becomes an
// authentication outage.
func TestVerifierCache_L2FailuresDoNotBreakAuthentication(t *testing.T) {
	ctx := context.Background()

	t.Run("warm failure is reported, not fatal", func(t *testing.T) {
		c := newTestVerifierCache(&fakeVerifierStore{loadErr: errors.New("db down")})
		restored, err := c.Warm(ctx)
		if err == nil {
			t.Error("want an error surfaced so the operator learns warmup is broken")
		}
		if restored != 0 {
			t.Errorf("restored=%d on a failed load", restored)
		}
	})

	t.Run("L1 still serves when the L2 write fails", func(t *testing.T) {
		c := newTestVerifierCache(&fakeVerifierStore{saveErr: errors.New("db down")})
		// Store reports the L2 failure to its caller...
		if err := c.Store(ctx, 1, "erin", "pw", "$2a$12$h"); err == nil {
			t.Error("want the L2 write failure surfaced")
		}
		// ...but the in-memory tier was still populated, so this process
		// keeps skipping bcrypt for the rest of its life.
		if ok, _ := c.Check(ctx, "erin", "pw", "$2a$12$h"); !ok {
			t.Error("L1 was not populated when the L2 write failed — a database " +
				"blip would then cost full bcrypt on every subsequent request")
		}
	})
}
