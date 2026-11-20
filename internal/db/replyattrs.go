package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// Per-plan, per-vendor RADIUS reply attributes — FR-CAT-004 | migration 066.
// The dictionary and validation live in internal/nas/dictionary.go.

const replyAttrColumns = `
	id, plan_id, vendor, attribute, value, enabled,
	COALESCE(created_by, ''), created_at`

func scanReplyAttr(row pgx.Row, a *nas.ReplyAttribute) error {
	return row.Scan(&a.ID, &a.PlanID, &a.Vendor, &a.Name, &a.Value, &a.Enabled,
		&a.CreatedBy, &a.CreatedAt)
}

// replyAttrTaken maps the unique constraint to a readable error. Caught
// rather than pre-read, because a pre-read races.
func replyAttrTaken(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "uq_plan_reply_attribute" {
		return nas.ErrReplyAttributeExists
	}
	return err
}

// SetPlanReplyAttribute adds or updates one attribute on a plan.
//
// Upsert rather than separate add and edit paths: the console's form is the
// same either way, and an operator correcting a value should not have to
// find and delete the old row first. The unique constraint is what makes
// "the same attribute" well defined.
func (s *NASStore) SetPlanReplyAttribute(ctx context.Context, in nas.ReplyAttribute, by string) (*nas.ReplyAttribute, error) {
	in = in.Normalise()
	if err := in.Validate(); err != nil {
		return nil, err
	}
	const q = `
		INSERT INTO plan_reply_attributes (plan_id, vendor, attribute, value, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (plan_id, vendor, attribute) DO UPDATE SET
			value = EXCLUDED.value,
			enabled = EXCLUDED.enabled,
			updated_by = EXCLUDED.created_by,
			updated_at = NOW()
		RETURNING ` + replyAttrColumns

	var out nas.ReplyAttribute
	err := scanReplyAttr(s.pool.QueryRow(ctx, q,
		in.PlanID, string(in.Vendor), in.Name, in.Value, in.Enabled, by), &out)
	if err != nil {
		return nil, fmt.Errorf("db: set reply attribute %s on plan %d: %w",
			in.Name, in.PlanID, replyAttrTaken(err))
	}
	return &out, nil
}

// DeletePlanReplyAttribute removes one attribute.
func (s *NASStore) DeletePlanReplyAttribute(ctx context.Context, id int) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM plan_reply_attributes WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("db: delete reply attribute %d: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: reply attribute %d not found", id)
	}
	return nil
}

// SetPlanReplyAttributeEnabled turns one attribute on or off without losing
// what it was set to.
func (s *NASStore) SetPlanReplyAttributeEnabled(ctx context.Context, id int, enabled bool, by string) error {
	const q = `
		UPDATE plan_reply_attributes
		   SET enabled = $2, updated_by = $3, updated_at = NOW()
		 WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id, enabled, by)
	if err != nil {
		return fmt.Errorf("db: set reply attribute %d enabled=%v: %w", id, enabled, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: reply attribute %d not found", id)
	}
	return nil
}

// ListPlanReplyAttributes returns every attribute on a plan, enabled or not,
// for the console. Ordered so the console can group by vendor without
// sorting again.
func (s *NASStore) ListPlanReplyAttributes(ctx context.Context, planID int) ([]nas.ReplyAttribute, error) {
	const q = `
		SELECT ` + replyAttrColumns + `
		FROM plan_reply_attributes
		WHERE plan_id = $1
		ORDER BY vendor, attribute`

	rows, err := s.pool.Query(ctx, q, planID)
	if err != nil {
		return nil, fmt.Errorf("db: list reply attributes for plan %d: %w", planID, err)
	}
	defer rows.Close()

	var out []nas.ReplyAttribute
	for rows.Next() {
		var a nas.ReplyAttribute
		if err := scanReplyAttr(rows, &a); err != nil {
			return nil, fmt.Errorf("db: scan reply attribute: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EnabledReplyAttributes returns the attributes the authentication path
// should send for a plan — enabled only, every vendor, because the caller
// filters to the answering NAS's vendor.
//
// Served by idx_plan_reply_attributes_plan, the partial index on enabled
// rows. This runs on the authentication path, which NFR-PERF-001 holds to a
// 15ms p99, so it is one indexed read returning a handful of rows.
func (s *NASStore) EnabledReplyAttributes(ctx context.Context, planID int) ([]nas.ReplyAttribute, error) {
	const q = `
		SELECT ` + replyAttrColumns + `
		FROM plan_reply_attributes
		WHERE plan_id = $1 AND enabled
		ORDER BY vendor, attribute`

	rows, err := s.pool.Query(ctx, q, planID)
	if err != nil {
		return nil, fmt.Errorf("db: enabled reply attributes for plan %d: %w", planID, err)
	}
	defer rows.Close()

	var out []nas.ReplyAttribute
	for rows.Next() {
		var a nas.ReplyAttribute
		if err := scanReplyAttr(rows, &a); err != nil {
			return nil, fmt.Errorf("db: scan reply attribute: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListEnabledReplyAttributes returns every enabled reply attribute across
// all plans, for nas.Resolver's periodic refresh.
//
// Loaded whole rather than per plan because the resolver caches the lot: the
// set is operational scale, so one query a minute replaces one per
// authentication.
func (s *NASStore) ListEnabledReplyAttributes(ctx context.Context) ([]nas.ReplyAttribute, error) {
	const q = `
		SELECT ` + replyAttrColumns + `
		FROM plan_reply_attributes
		WHERE enabled
		ORDER BY plan_id, vendor, attribute`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list enabled reply attributes: %w", err)
	}
	defer rows.Close()

	var out []nas.ReplyAttribute
	for rows.Next() {
		var a nas.ReplyAttribute
		if err := scanReplyAttr(rows, &a); err != nil {
			return nil, fmt.Errorf("db: scan reply attribute: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
