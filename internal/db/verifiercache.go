package db

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// VerifierCacheStore is the PostgreSQL L2 behind the RADIUS fast-verifier
// cache (migration 046). Satisfies radius.VerifierStore.
//
// PostgreSQL rather than Redis, deliberately. Redis was removed from this
// system entirely when session state moved to live_sessions (036) and the
// task queue to jobqueue_tasks (037) — IDD § 8.3 records what that bought:
// one datastore to operate, back up, secure and fail over instead of two.
// Reintroducing it for a cache measured in kilobytes would trade all of
// that away for a latency win this workload cannot use, because the L2 tier
// is read once at startup and written only after a bcrypt that already cost
// ~280ms. There is no hot-path read for Redis to accelerate.
//
// Being in PostgreSQL also buys something Redis would not: the write is
// transactional with everything else, and both processes already have a
// connection, which is what makes cross-process invalidation possible at
// all.
type VerifierCacheStore struct{ pool dbPool }

var _ radius.VerifierStore = (*VerifierCacheStore)(nil)

// VerifierCache exposes the verifier store.
func (d *DB) VerifierCache() *VerifierCacheStore { return &VerifierCacheStore{pool: d.pool} }

// LoadActive returns every unexpired verifier for warmup.
//
// Bounded by LIMIT rather than trusted to be small. The table is one row per
// recently-authenticated subscriber, so on a large deployment mid-storm it
// could be tens of thousands; warmup runs before the daemon serves traffic
// and holding all of that in one slice is avoidable risk for no benefit.
// Ordered by expires_at DESC so that if the limit does bite, what is kept is
// what stays useful longest.
func (s *VerifierCacheStore) LoadActive(ctx context.Context) ([]radius.PersistedVerifier, error) {
	const q = `
		SELECT username, verifier, expires_at
		FROM radius_verifier_cache
		WHERE expires_at > NOW()
		ORDER BY expires_at DESC
		LIMIT 100000`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: load verifier cache: %w", err)
	}
	defer rows.Close()

	out := make([]radius.PersistedVerifier, 0, 256)
	for rows.Next() {
		var v radius.PersistedVerifier
		if err := rows.Scan(&v.Username, &v.Verifier, &v.ExpiresAt); err != nil {
			return nil, fmt.Errorf("db: scan verifier row: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate verifier cache: %w", err)
	}
	return out, nil
}

// Save upserts one verifier.
//
// Conflict target is subscriber_id (the primary key) rather than username,
// even though both are unique: a username change would otherwise insert a
// second row for the same subscriber and violate the username unique
// constraint instead of replacing what is there.
func (s *VerifierCacheStore) Save(ctx context.Context, subscriberID int, username string, verifier []byte, expiresAt time.Time) error {
	const q = `
		INSERT INTO radius_verifier_cache (subscriber_id, username, verifier, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (subscriber_id) DO UPDATE SET
			username   = EXCLUDED.username,
			verifier   = EXCLUDED.verifier,
			expires_at = EXCLUDED.expires_at,
			updated_at = NOW()`

	if _, err := s.pool.Exec(ctx, q, subscriberID, username, verifier, expiresAt); err != nil {
		return fmt.Errorf("db: save verifier for subscriber %d: %w", subscriberID, err)
	}
	return nil
}

// DeleteByUsername removes a subscriber's verifier.
//
// By username because that is what the invalidation callers hold —
// api.SubscriberCacheInvalidator's interface is username-keyed, and
// rewriting it to carry an id would touch every lifecycle call site for no
// gain, the column being unique either way.
//
// A missing row is success, not an error: invalidating a subscriber who was
// never cached is the normal case, and callers invalidate defensively.
func (s *VerifierCacheStore) DeleteByUsername(ctx context.Context, username string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM radius_verifier_cache WHERE username = $1`, username); err != nil {
		return fmt.Errorf("db: delete verifier for %q: %w", username, err)
	}
	return nil
}

// ReapExpired deletes verifiers past their expiry and reports how many went.
//
// Expired rows are already inert — LoadActive filters them and a stale
// verifier could never match a current password hash anyway — so this is
// about bounding table size, not correctness. It also bounds the exposure
// window the migration's header describes: the set of verifiers an attacker
// with both the database and the secret could attack offline stays limited
// to recent authentications rather than growing without end.
func (s *VerifierCacheStore) ReapExpired(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM radius_verifier_cache WHERE expires_at <= NOW()`)
	if err != nil {
		return 0, fmt.Errorf("db: reap expired verifiers: %w", err)
	}
	return tag.RowsAffected(), nil
}
