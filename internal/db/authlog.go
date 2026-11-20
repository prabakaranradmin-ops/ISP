package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// RADIUS authentication attempt log — FR-NET-004 | migration 063.
//
// There is no password column and nothing here writes one. See
// internal/radius/authlog.go.

// AuthLogStore records and reads authentication attempts.
type AuthLogStore struct{ pool dbPool }

// NewAuthLogStore constructs an AuthLogStore.
func NewAuthLogStore(pool dbPool) *AuthLogStore { return &AuthLogStore{pool: pool} }

// AuthLog returns the authentication attempt store.
func (d *DB) AuthLog() *AuthLogStore { return NewAuthLogStore(d.pool) }

var _ radius.AuthLogStore = (*AuthLogStore)(nil)

// RecordAuthAttempts appends a batch of attempts as one multi-row INSERT.
//
// Deliberately not pgx's CopyFrom, which would be the faster bulk path:
// every store here holds the minimal dbPool interface (hapool.go) so
// haPool can sit between them and catch SQLSTATE 25006 after a Patroni
// failover, evicting the stale connection. Widening that interface for
// CopyFrom would route this write around that detection, so a batch
// landing mid-failover would fail repeatedly instead of resetting the
// pool. One INSERT of a few hundred rows, a few dozen times a second at
// the daemon's rated load, is well inside what that costs.
func (s *AuthLogStore) RecordAuthAttempts(ctx context.Context, attempts []radius.AuthAttempt) error {
	if len(attempts) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(attempts))
	for _, a := range attempts {
		username := a.Username
		if len(username) > 253 {
			username = username[:253]
		}
		result := a.Result
		if result != radius.ResultAccept && result != radius.ResultReject {
			// The CHECK would refuse the whole batch over one bad row, and
			// a batch is other attempts' only chance of being recorded.
			// An unrecognised result is stored as a reject with an
			// internal_error reason, which is both true and findable.
			result = radius.ResultReject
			a.RejectReason = radius.RejectInternalError
		}
		reason := a.RejectReason
		if result == radius.ResultAccept {
			reason = "" // the CHECK requires it empty on an accept
		} else if strings.TrimSpace(reason) == "" {
			reason = radius.RejectInternalError
		}
		// nas_ip is INET and will not take an empty string.
		var nasIP any
		if strings.TrimSpace(a.NASIP) != "" {
			nasIP = a.NASIP
		}
		at := a.At
		if at.IsZero() {
			at = time.Now()
		}
		rows = append(rows, []any{username, nasIP, a.MAC, result, reason, a.AuthMethod, at})
	}

	// VALUES ($1,$2,...),($8,$9,...) — 7 placeholders per row. At the
	// batch size the writer uses this is well under Postgres's 65535
	// parameter ceiling, and the writer's own batching is what keeps it
	// there.
	const cols = 7
	var (
		sb   strings.Builder
		args = make([]any, 0, len(rows)*cols)
	)
	sb.WriteString(`INSERT INTO radius_auth_log
		(username, nas_ip, mac, result, reject_reason, auth_method, created_at) VALUES `)
	for i, r := range rows {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i * cols
		fmt.Fprintf(&sb, "($%d,$%d::inet,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7)
		args = append(args, r...)
	}

	if _, err := s.pool.Exec(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("db: record %d authentication attempts: %w", len(attempts), err)
	}
	return nil
}

// PurgeAuthLog deletes attempts older than the retention window and
// returns how many went.
//
// Bounded per call so one pass cannot hold a long transaction over a table
// this size: the scanner calls it on a tick, and a backlog drains over
// several passes rather than in one statement that locks rows for
// minutes.
func (s *AuthLogStore) PurgeAuthLog(ctx context.Context, olderThan time.Duration, limit int) (int64, error) {
	if olderThan <= 0 {
		olderThan = radius.AuthLogRetention
	}
	if limit <= 0 || limit > 100000 {
		limit = 10000
	}
	cutoff := time.Now().Add(-olderThan)

	tag, err := s.pool.Exec(ctx, `
		DELETE FROM radius_auth_log
		 WHERE id IN (
		     SELECT id FROM radius_auth_log WHERE created_at < $1 ORDER BY created_at LIMIT $2
		 )`, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("db: purge authentication attempts before %s: %w",
			cutoff.Format(time.RFC3339), err)
	}
	return tag.RowsAffected(), nil
}

// ListAuthAttempts returns attempts newest first.
func (s *AuthLogStore) ListAuthAttempts(ctx context.Context, f radius.AuthLogFilter) ([]radius.AuthLogEntry, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	const q = `
		SELECT id, username, COALESCE(host(nas_ip), ''), mac, result, reject_reason,
		       auth_method, created_at
		  FROM radius_auth_log
		 WHERE ($1 = '' OR username = $1)
		   AND ($2 = '' OR result = $2)
		   AND ($3 = '' OR reject_reason = $3)
		   AND ($4::timestamptz IS NULL OR created_at >= $4)
		   AND ($5::timestamptz IS NULL OR created_at < $5)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $6`

	rows, err := s.pool.Query(ctx, q,
		strings.TrimSpace(f.Username), strings.TrimSpace(f.Result),
		strings.TrimSpace(f.Reason), f.From, f.To, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: list authentication attempts: %w", err)
	}
	defer rows.Close()

	out := make([]radius.AuthLogEntry, 0, f.Limit)
	for rows.Next() {
		var e radius.AuthLogEntry
		if err := rows.Scan(&e.ID, &e.Username, &e.NASIP, &e.MAC, &e.Result,
			&e.RejectReason, &e.AuthMethod, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan authentication attempt: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate authentication attempts: %w", err)
	}
	return out, nil
}

// SummariseAuthRejects groups the rejects in a window by reason.
//
// The first thing to look at: "forty subscribers failed on bad_password
// this morning" and "forty failed on not_authorised" are different
// incidents with different causes, and a flat list of attempts hides
// which one is happening.
func (s *AuthLogStore) SummariseAuthRejects(ctx context.Context, since time.Time) ([]radius.AuthRejectSummary, error) {
	const q = `
		SELECT reject_reason, COUNT(*)
		  FROM radius_auth_log
		 WHERE result = 'reject' AND created_at >= $1
		 GROUP BY reject_reason
		 ORDER BY COUNT(*) DESC`

	rows, err := s.pool.Query(ctx, q, since)
	if err != nil {
		return nil, fmt.Errorf("db: summarise authentication rejects: %w", err)
	}
	defer rows.Close()

	out := make([]radius.AuthRejectSummary, 0, 8)
	for rows.Next() {
		var r radius.AuthRejectSummary
		if err := rows.Scan(&r.Reason, &r.Count); err != nil {
			return nil, fmt.Errorf("db: scan reject summary: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
