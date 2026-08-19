package radius

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
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
}

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
func (c *VerifierCache) Store(_ context.Context, username, password, passwordHash string) error {
	if c == nil || c.store == nil {
		return nil
	}
	c.store.Set(verifierKey(username), c.verifier(password, passwordHash), verifierCacheTTL)
	return nil
}
