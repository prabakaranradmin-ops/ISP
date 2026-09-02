package radius

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
)

// verifierCacheTTL bounds how long a fast-path verifier stays valid after a
// successful bcrypt comparison. It is a secondary backstop, not the primary
// invalidation mechanism — see the passwordHash parameter below — so it can
// stay generous without widening the exposure window a password change
// leaves open.
const verifierCacheTTL = 5 * time.Minute

var radiusVerifierCacheHit = promauto.NewCounter(prometheus.CounterOpts{
	Name: "radius_verifier_cache_hit_total",
	Help: "RADIUS authentications that skipped bcrypt via the fast-verifier cache",
})

// VerifierCache lets repeat RADIUS authentications for the same
// (username, password) pair skip bcrypt cost-12 — measured at ~280ms per
// comparison, ~19x NFR-PERF-001's 15ms p99 budget — once the first request
// for that pair has already paid that cost.
//
// It never stores the password or the bcrypt hash. Instead, on a bcrypt
// success it caches HMAC-SHA256(secret, password || passwordHash): a keyed
// pseudorandom function nobody can invert or forge without the server-side
// secret, and which takes microseconds to compute versus bcrypt's ~280ms.
//
// passwordHash — the subscriber's *current* bcrypt hash from the DB/subscriber
// cache, which the caller already has on every request — is mixed into the
// verifier deliberately, not just the password: without it, a cached
// verifier keyed on password alone would keep accepting an old password for
// up to verifierCacheTTL after it was changed, since the cache would have no
// way to know the change happened. Binding to passwordHash makes any
// password change self-invalidate every cached verifier for that subscriber
// immediately (the hash component no longer matches), not just after a TTL.
//
// This does not weaken brute-force resistance: a wrong password guess can
// only produce a matching verifier by already knowing the correct password
// (the HMAC secret is not attacker-known), so every incorrect guess still
// falls through to the full bcrypt comparison exactly as before — see
// handleAuth. Only a request that already has the right password benefits.
//
// FR: NFR-PERF-001, NFR-SCAL-001 | DDS §5.1
type VerifierCache struct {
	store  *localcache.Store[[]byte]
	secret []byte
	// persist is the optional L2 (migration 046). Nil keeps the entirely
	// in-memory behaviour this cache shipped with.
	//
	// Two tiers rather than one because they solve different problems and
	// have different costs. L1 is a map lookup in microseconds and carries
	// the steady-state hot path; L2 is a database round trip, which is
	// cheap next to bcrypt's ~280ms but not next to L1, so it is read only
	// at startup (Warm) and written only on the path that just paid for
	// bcrypt anyway (Store). Nothing on the per-request fast path touches
	// the database.
	persist VerifierStore
}

// VerifierStore persists verifiers across restarts and makes them visible
// to other processes. Satisfied by *db.VerifierCacheStore.
//
// Every method takes a context and may fail; VerifierCache treats all such
// failures as non-fatal and falls through to bcrypt, because a cache that
// can take authentication down when its backing store is unavailable is
// worse than no cache.
type VerifierStore interface {
	// LoadActive returns every unexpired verifier, for warmup.
	LoadActive(ctx context.Context) ([]PersistedVerifier, error)
	// Save upserts one verifier.
	Save(ctx context.Context, subscriberID int, username string, verifier []byte, expiresAt time.Time) error
	// DeleteByUsername removes a subscriber's verifier — the immediate
	// invalidation a password change needs.
	DeleteByUsername(ctx context.Context, username string) error
}

// PersistedVerifier is one stored verifier, as warmup reads it back.
type PersistedVerifier struct {
	Username  string
	Verifier  []byte
	ExpiresAt time.Time
}

// SetPersistence enables the L2 tier. Optional and settable only before the
// daemon starts serving.
func (c *VerifierCache) SetPersistence(s VerifierStore) { c.persist = s }

// NewVerifierCache constructs a VerifierCache. secret is what makes the
// cached verifier unforgeable — config.Load enforces a 32-byte minimum on
// RADIUS_VERIFIER_SECRET for the radiusd service. It is deliberately a
// separate secret from the RADIUS shared secret (used for NAS protocol
// obfuscation, a different threat model entirely) rather than reusing it.
func NewVerifierCache(store *localcache.Store[[]byte], secret []byte) *VerifierCache {
	return &VerifierCache{store: store, secret: secret}
}

func verifierKey(username string) string {
	return "radius_verifier:" + username
}

// verifier binds the verifier to both the password and the subscriber's
// current password hash. A length-prefixed encoding (not a plain
// concatenation) keeps password="ab",hash="cd" distinct from
// password="a",hash="bcd" — both would otherwise HMAC the same bytes "abcd".
func (c *VerifierCache) verifier(password, passwordHash string) []byte {
	mac := hmac.New(sha256.New, c.secret)
	writeLengthPrefixed(mac, password)
	writeLengthPrefixed(mac, passwordHash)
	return mac.Sum(nil)
}

func writeLengthPrefixed(mac hash.Hash, s string) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
	mac.Write(lenBuf[:]) //nolint:errcheck // hash.Hash.Write never returns an error
	mac.Write([]byte(s)) //nolint:errcheck
}

// Check reports whether password matches the cached verifier for username,
// given the subscriber's current passwordHash. A false result means "no
// cache entry, or it didn't match" — callers must still fall back to the
// authoritative bcrypt comparison in either case; treating a cache mismatch
// as an outright rejection would reject a legitimate subscriber whose
// password was recently changed.
func (c *VerifierCache) Check(_ context.Context, username, password, passwordHash string) (bool, error) {
	if c == nil || c.store == nil {
		return false, nil
	}
	cachedMAC, ok := c.store.Get(verifierKey(username))
	if !ok {
		return false, nil
	}
	if hmac.Equal(c.verifier(password, passwordHash), cachedMAC) {
		radiusVerifierCacheHit.Inc()
		return true, nil
	}
	return false, nil
}

// Store caches password's verifier for username, bound to the passwordHash
// it was just bcrypt-verified against, so the next request with the same
// password (and no intervening password change) can skip bcrypt entirely.
//
// subscriberID is used only by the L2 tier, which keys on it so a deleted
// subscriber's verifier is removed by the foreign key rather than by
// remembering to.
func (c *VerifierCache) Store(ctx context.Context, subscriberID int, username, password, passwordHash string) error {
	if c == nil || c.store == nil {
		return nil
	}
	mac := c.verifier(password, passwordHash)
	expiresAt := time.Now().Add(verifierCacheTTL)
	c.store.Set(verifierKey(username), mac, verifierCacheTTL)

	// Write-through, and only on this path — which has just spent ~280ms in
	// bcrypt, so a millisecond insert is not measurable against it. The
	// error is returned to the caller, which logs and continues: a failed
	// L2 write costs a bcrypt on the next restart, not a failed
	// authentication.
	if c.persist != nil {
		if err := c.persist.Save(ctx, subscriberID, username, mac, expiresAt); err != nil {
			return fmt.Errorf("radius: persist verifier for %q: %w", username, err)
		}
	}
	return nil
}

// Warm repopulates L1 from L2 at startup and reports how many entries were
// restored.
//
// This is the whole reason the L2 tier exists. An empty cache after a
// restart means every reconnecting subscriber pays full bcrypt, and on a
// 2-vCPU host that is roughly 7 authentications a second — so a restart
// during a reconnect event turns into a queue that drains for tens of
// minutes while clients retransmit into it.
//
// Entries are loaded with their *remaining* TTL rather than a fresh one, so
// warmup cannot extend the lifetime of a verifier beyond what it would have
// had without a restart. A row that expires in 40 seconds is restored with
// 40 seconds left.
//
// Never fatal: a failure here costs performance on a cold start, which is
// exactly the situation without this feature at all.
func (c *VerifierCache) Warm(ctx context.Context) (int, error) {
	if c == nil || c.store == nil || c.persist == nil {
		return 0, nil
	}
	rows, err := c.persist.LoadActive(ctx)
	if err != nil {
		return 0, fmt.Errorf("radius: warm verifier cache: %w", err)
	}

	now := time.Now()
	restored := 0
	for _, row := range rows {
		ttl := row.ExpiresAt.Sub(now)
		if ttl <= 0 {
			// Raced with expiry between the query and here. The reaper will
			// collect it; skipping keeps a stale verifier out of L1.
			continue
		}
		c.store.Set(verifierKey(row.Username), row.Verifier, ttl)
		restored++
	}
	return restored, nil
}

// Invalidate drops a subscriber's verifier from both tiers immediately.
//
// The verifier already self-invalidates on a password change — it is bound
// to the bcrypt hash, so a changed hash can never match a stored MAC (see
// this type's doc comment). This exists for the cases where that binding
// does not help: a suspension or termination leaves the hash untouched, and
// the cached verifier would otherwise stay valid for its remaining TTL.
//
// It is also what lets api_service invalidate radiusd's cache at all.
// Before the L2 tier, cmd/api/main.go's subCacheInvalidator was necessarily
// a no-op — one process cannot reach into another's memory — and
// invalidation waited out a TTL. Deleting the shared row is visible to both.
func (c *VerifierCache) Invalidate(ctx context.Context, username string) error {
	if c == nil {
		return nil
	}
	if c.store != nil {
		c.store.Delete(verifierKey(username))
	}
	if c.persist != nil {
		if err := c.persist.DeleteByUsername(ctx, username); err != nil {
			return fmt.Errorf("radius: invalidate persisted verifier for %q: %w", username, err)
		}
	}
	return nil
}
