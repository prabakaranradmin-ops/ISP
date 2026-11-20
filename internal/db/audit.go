package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Append-only staff audit log — FR-OBS-006 | migration 062.
//
// The table is append-only against the role the services run as: bss_app
// holds INSERT and SELECT and has UPDATE and DELETE revoked, with an RLS
// policy behind that as defence-in-depth (migration 062, following the
// pattern migration 019 established for lea_audit_log). This store
// therefore offers no update or delete method at all — not as a
// convention, but because the database would refuse one.

// AuditStore records and reads the staff audit trail.
type AuditStore struct{ pool dbPool }

// NewAuditStore constructs an AuditStore.
func NewAuditStore(pool dbPool) *AuditStore { return &AuditStore{pool: pool} }

// Audit returns the audit store.
func (d *DB) Audit() *AuditStore { return NewAuditStore(d.pool) }

var _ middleware.AuditSink = (*AuditStore)(nil)

// RecordAudit appends one entry.
//
// An actor is required by the table's CHECK; an action performed with no
// subject on the context would otherwise be stored as an empty string and
// read as if nobody did it. "unknown" is stored instead, which is both
// honest and findable — a run of them means a path is not setting its
// subject, which is the bug worth noticing.
func (s *AuditStore) RecordAudit(ctx context.Context, e middleware.AuditEntry) error {
	actor := strings.TrimSpace(e.ActorID)
	if actor == "" {
		actor = "unknown"
	}
	action := strings.TrimSpace(e.Action)
	if action == "" {
		// Nothing useful to record, and the CHECK would refuse it.
		return fmt.Errorf("db: audit entry has no action")
	}
	if len(action) > 100 {
		action = action[:100]
	}
	target := e.Target
	if len(target) > 200 {
		target = target[:200]
	}

	// The detail map is caller-supplied and may hold anything JSON can
	// carry. A value that will not marshal is dropped rather than losing
	// the whole entry: who did what matters more than its detail.
	var detail []byte
	if len(e.Detail) > 0 {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			detail = nil
		} else {
			detail = b
		}
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO staff_audit_log
			(correlation_id, actor_id, actor_role, action, target, detail)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		e.CorrelationID, actor, e.ActorRole, action, target, detail)
	if err != nil {
		return fmt.Errorf("db: record audit entry %q: %w", action, err)
	}
	return nil
}

// ListAudit returns entries newest first, filtered by actor, action and
// date (FR-OBS-006).
//
// The limit is capped rather than optional: this table only grows, and a
// screen that tried to render all of it would stop working at exactly the
// point the history became worth reading.
func (s *AuditStore) ListAudit(ctx context.Context, f middleware.AuditFilter) ([]middleware.AuditRecord, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	const q = `
		SELECT id, correlation_id, actor_id, actor_role, action, target, detail, created_at
		  FROM staff_audit_log
		 WHERE ($1 = '' OR actor_id = $1)
		   AND ($2 = '' OR action = $2)
		   AND ($3::timestamptz IS NULL OR created_at >= $3)
		   AND ($4::timestamptz IS NULL OR created_at < $4)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $5`

	rows, err := s.pool.Query(ctx, q,
		strings.TrimSpace(f.Actor), strings.TrimSpace(f.Action), f.From, f.To, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: list audit entries: %w", err)
	}
	defer rows.Close()

	out := make([]middleware.AuditRecord, 0, f.Limit)
	for rows.Next() {
		var (
			rec    middleware.AuditRecord
			detail []byte
		)
		if err := rows.Scan(&rec.ID, &rec.CorrelationID, &rec.ActorID, &rec.ActorRole,
			&rec.Action, &rec.Target, &detail, &rec.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan audit entry: %w", err)
		}
		if len(detail) > 0 {
			// A row whose detail will not unmarshal is still a real audit
			// entry; its detail is dropped rather than the entry hidden.
			_ = json.Unmarshal(detail, &rec.Detail) //nolint:errcheck
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate audit entries: %w", err)
	}
	return out, nil
}

// ListAuditActors returns the actors who appear in the trail, so the
// filter can offer them rather than making an operator type a username
// exactly.
func (s *AuditStore) ListAuditActors(ctx context.Context) ([]string, error) {
	return s.listDistinct(ctx, `
		SELECT DISTINCT actor_id FROM staff_audit_log ORDER BY actor_id LIMIT 200`)
}

// ListAuditActions returns the actions that appear in the trail.
func (s *AuditStore) ListAuditActions(ctx context.Context) ([]string, error) {
	return s.listDistinct(ctx, `
		SELECT DISTINCT action FROM staff_audit_log ORDER BY action LIMIT 200`)
}

func (s *AuditStore) listDistinct(ctx context.Context, q string) ([]string, error) {
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list audit filter values: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 32)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("db: scan audit filter value: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// CountAudit is how many entries match a filter, so the screen can say
// whether what it is showing is all of them.
func (s *AuditStore) CountAudit(ctx context.Context, f middleware.AuditFilter) (int, error) {
	const q = `
		SELECT COUNT(*) FROM staff_audit_log
		 WHERE ($1 = '' OR actor_id = $1)
		   AND ($2 = '' OR action = $2)
		   AND ($3::timestamptz IS NULL OR created_at >= $3)
		   AND ($4::timestamptz IS NULL OR created_at < $4)`

	var n int
	if err := s.pool.QueryRow(ctx, q,
		strings.TrimSpace(f.Actor), strings.TrimSpace(f.Action), f.From, f.To).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: count audit entries: %w", err)
	}
	return n, nil
}
