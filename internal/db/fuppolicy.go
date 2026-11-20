package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
)

// Multi-tier fair-use policies and data top-ups — FR-CAT-003 | migration 067.
// The domain rules (tier resolution, validation) live in
// internal/fup/policy.go.

// ── Policies ────────────────────────────────────────────────────────────────

func fupPolicyNameTaken(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "uq_fup_policy_name_active" {
		return fup.ErrPolicyNameTaken
	}
	return err
}

// SaveFUPPolicy creates or replaces a policy and its tiers in one
// transaction.
//
// The tiers are deleted and re-inserted rather than diffed. A policy has at
// most a handful of them, and a diff would have to decide what a "changed"
// tier is — the threshold, the rate, or both — while the thing that matters
// is that the set a subscriber is evaluated against is never half-applied.
// One transaction gives that for free.
func (s *FUPStore) SaveFUPPolicy(ctx context.Context, p fup.Policy, by string) (*fup.Policy, error) {
	p = p.Normalise()
	if err := p.Validate(); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: begin save fup policy: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id int
	if p.ID == 0 {
		err = tx.QueryRow(ctx, `
			INSERT INTO fup_policies (name, created_by) VALUES ($1, $2) RETURNING id`,
			p.Name, by).Scan(&id)
	} else {
		id = p.ID
		var tag pgconn.CommandTag
		tag, err = tx.Exec(ctx, `
			UPDATE fup_policies SET name = $2, updated_by = $3, updated_at = NOW()
			WHERE id = $1`, id, p.Name, by)
		if err == nil && tag.RowsAffected() != 1 {
			return nil, fmt.Errorf("db: fup policy %d not found", id)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("db: save fup policy: %w", fupPolicyNameTaken(err))
	}

	if _, err := tx.Exec(ctx, `DELETE FROM fup_policy_tiers WHERE policy_id = $1`, id); err != nil {
		return nil, fmt.Errorf("db: clear tiers for policy %d: %w", id, err)
	}
	for _, t := range p.Tiers {
		if _, err := tx.Exec(ctx, `
			INSERT INTO fup_policy_tiers (policy_id, threshold_bytes, throttle_rate)
			VALUES ($1, $2, $3)`, id, t.ThresholdBytes, t.ThrottleRate); err != nil {
			return nil, fmt.Errorf("db: insert tier %d bytes for policy %d: %w",
				t.ThresholdBytes, id, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("db: commit save fup policy: %w", err)
	}
	return s.FUPPolicyByID(ctx, id)
}

// SetFUPPolicyActive retires or restores a policy, leaving the plans on it
// evaluating against it unchanged.
func (s *FUPStore) SetFUPPolicyActive(ctx context.Context, id int, active bool, by string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE fup_policies SET active = $2, updated_by = $3, updated_at = NOW()
		WHERE id = $1`, id, active, by)
	if err != nil {
		return fmt.Errorf("db: set fup policy %d active=%v: %w", id, active, fupPolicyNameTaken(err))
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: fup policy %d not found", id)
	}
	return nil
}

// DeleteFUPPolicy removes a policy no plan references.
//
// The count is read first only so the caller can be told to retire it
// instead of being handed a foreign-key violation; the database refuses it
// either way.
func (s *FUPStore) DeleteFUPPolicy(ctx context.Context, id int) error {
	var inUse int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM plans WHERE fup_policy_id = $1`, id).Scan(&inUse); err != nil {
		return fmt.Errorf("db: count plans on fup policy %d: %w", id, err)
	}
	if inUse > 0 {
		return fmt.Errorf("%w: %d plan(s)", fup.ErrPolicyInUse, inUse)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM fup_policies WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("db: delete fup policy %d: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: fup policy %d not found", id)
	}
	return nil
}

// FUPPolicyByID reads one policy with its tiers, ascending by threshold so
// internal/fup.Policy.TierFor can rely on the order.
func (s *FUPStore) FUPPolicyByID(ctx context.Context, id int) (*fup.Policy, error) {
	var p fup.Policy
	err := s.pool.QueryRow(ctx, `
		SELECT f.id, f.name, f.active, COALESCE(f.created_by, ''), f.created_at,
		       COALESCE(f.updated_by, ''), f.updated_at,
		       (SELECT count(*) FROM plans pl WHERE pl.fup_policy_id = f.id)
		FROM fup_policies f WHERE f.id = $1`, id).
		Scan(&p.ID, &p.Name, &p.Active, &p.CreatedBy, &p.CreatedAt,
			&p.UpdatedBy, &p.UpdatedAt, &p.InUseByPlans)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("db: fup policy %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("db: read fup policy %d: %w", id, err)
	}

	tiers, err := s.tiersFor(ctx, id)
	if err != nil {
		return nil, err
	}
	p.Tiers = tiers
	return &p, nil
}

func (s *FUPStore) tiersFor(ctx context.Context, policyID int) ([]fup.Tier, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, policy_id, threshold_bytes, throttle_rate
		FROM fup_policy_tiers WHERE policy_id = $1
		ORDER BY threshold_bytes`, policyID)
	if err != nil {
		return nil, fmt.Errorf("db: list tiers for policy %d: %w", policyID, err)
	}
	defer rows.Close()

	var out []fup.Tier
	for rows.Next() {
		var t fup.Tier
		if err := rows.Scan(&t.ID, &t.PolicyID, &t.ThresholdBytes, &t.ThrottleRate); err != nil {
			return nil, fmt.Errorf("db: scan tier: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListFUPPolicies returns the policies with their tiers and plan counts.
func (s *FUPStore) ListFUPPolicies(ctx context.Context, includeRetired bool) ([]fup.Policy, error) {
	q := `
		SELECT f.id, f.name, f.active, COALESCE(f.created_by, ''), f.created_at,
		       COALESCE(f.updated_by, ''), f.updated_at,
		       (SELECT count(*) FROM plans pl WHERE pl.fup_policy_id = f.id)
		FROM fup_policies f`
	if !includeRetired {
		q += ` WHERE f.active`
	}
	q += ` ORDER BY f.active DESC, lower(btrim(f.name))`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list fup policies: %w", err)
	}
	var out []fup.Policy
	for rows.Next() {
		var p fup.Policy
		if err := rows.Scan(&p.ID, &p.Name, &p.Active, &p.CreatedBy, &p.CreatedAt,
			&p.UpdatedBy, &p.UpdatedAt, &p.InUseByPlans); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: scan fup policy: %w", err)
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Tiers in a second pass rather than a join: a join would repeat every
	// policy row per tier and the assembly would have to de-duplicate, for
	// a listing that returns a handful of policies.
	for i := range out {
		tiers, err := s.tiersFor(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Tiers = tiers
	}
	return out, nil
}

// SetPlanFUPPolicy attaches a policy to a plan, or clears it with a nil
// policyID so the plan evaluates against its own threshold again.
func (s *FUPStore) SetPlanFUPPolicy(ctx context.Context, planID int, policyID *int, by string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE plans SET fup_policy_id = $2, updated_by = $3, updated_at = NOW()
		WHERE id = $1`, planID, policyID, by)
	if err != nil {
		return fmt.Errorf("db: set plan %d fup policy: %w", planID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: plan %d not found", planID)
	}
	return nil
}

// ── Top-ups ─────────────────────────────────────────────────────────────────

// AddDataTopup records a purchased data allowance.
//
// The caller is responsible for the wallet debit and passes its ledger id,
// so a top-up cannot exist without a payment behind it except where staff
// deliberately grant one — which is why purchasedBy is recorded separately
// and is never empty.
func (s *FUPStore) AddDataTopup(ctx context.Context, t fup.Topup, by string) (*fup.Topup, error) {
	if t.Bytes <= 0 {
		return nil, fmt.Errorf("a data top-up needs a size above zero")
	}
	if t.ExpiresAt.IsZero() || !t.ExpiresAt.After(time.Now()) {
		return nil, fmt.Errorf("a data top-up must expire at the end of a cycle that has not passed")
	}
	if by == "" {
		return nil, fmt.Errorf("a data top-up must record who bought it")
	}

	var out fup.Topup
	err := s.pool.QueryRow(ctx, `
		INSERT INTO data_topups (subscriber_id, bytes, expires_at, transaction_id, purchased_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, subscriber_id, bytes, expires_at, transaction_id, purchased_by, purchased_at`,
		t.SubscriberID, t.Bytes, t.ExpiresAt, t.TransactionID, by).
		Scan(&out.ID, &out.SubscriberID, &out.Bytes, &out.ExpiresAt,
			&out.TransactionID, &out.PurchasedBy, &out.PurchasedAt)
	if err != nil {
		return nil, fmt.Errorf("db: add data top-up for subscriber %d: %w", t.SubscriberID, err)
	}
	return &out, nil
}

// ListDataTopups returns a subscriber's top-ups, newest first, expired ones
// included — they are what a billing dispute is answered from.
func (s *FUPStore) ListDataTopups(ctx context.Context, subscriberID int) ([]fup.Topup, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, subscriber_id, bytes, expires_at, transaction_id, purchased_by, purchased_at
		FROM data_topups WHERE subscriber_id = $1
		ORDER BY purchased_at DESC`, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: list top-ups for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()

	var out []fup.Topup
	for rows.Next() {
		var t fup.Topup
		if err := rows.Scan(&t.ID, &t.SubscriberID, &t.Bytes, &t.ExpiresAt,
			&t.TransactionID, &t.PurchasedBy, &t.PurchasedAt); err != nil {
			return nil, fmt.Errorf("db: scan top-up: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ── The scanner's question ──────────────────────────────────────────────────

// topupBytes sums a subscriber's unexpired top-ups. Shared by the
// re-evaluation query below and by the console's display.
const topupBytes = `
	SELECT t.subscriber_id, SUM(t.bytes) AS topped_up
	FROM data_topups t
	WHERE t.expires_at > NOW()
	GROUP BY t.subscriber_id`

// SessionsNeedingThrottleChange returns the online subscribers whose applied
// throttle is not the one their usage now calls for — FR-CAT-003.
//
// This replaces "who is over their threshold and not yet throttled" with
// "whose rate is wrong", which is the question that covers every case at
// once:
//
//   - crossing the first threshold (nothing applied, a tier now matches)
//   - crossing a harder one (a gentler rate applied, a harder tier matches).
//     The old boolean could not see this: fup_active was already true, and
//     the scanner skipped anyone already throttled, so the harder tier was
//     unreachable.
//   - buying a top-up (a rate applied, the raised thresholds now match a
//     gentler tier or none)
//   - a top-up expiring (nothing applied, the thresholds drop back)
//
// The target rate is computed in SQL rather than in Go so the scanner's 10s
// tick does one query instead of one per online subscriber. The tier is
// picked by a LATERAL join taking the highest threshold at or below the
// topped-up usage — ORDER BY threshold_bytes DESC LIMIT 1 — which is the
// same "highest crossed tier wins" rule internal/fup.Policy.TierFor
// implements for the paths that work in Go.
func (s *FUPStore) SessionsNeedingThrottleChange(ctx context.Context) ([]fup.ThrottleChange, error) {
	const q = `
		WITH usage AS (` + liveSessionUsage + `),
		topups AS (` + topupBytes + `)
		SELECT s.id, s.username, COALESCE(u.nas_ip, ''),
		       COALESCE(u.bytes_used, 0),
		       COALESCE(t.topped_up, 0),
		       COALESCE(s.fup_applied_rate, ''),
		       COALESCE(tier.throttle_rate, '') AS target_rate,
		       COALESCE(tier.threshold_bytes, 0)
		FROM usage u
		JOIN subscribers s ON s.id = u.subscriber_id
		JOIN plans p       ON p.id = s.plan_id
		LEFT JOIN topups t ON t.subscriber_id = s.id
		-- The tiers this plan is evaluated against: its policy's, or a
		-- single synthetic one from its own legacy columns, so a plan that
		-- predates policies behaves exactly as it did.
		LEFT JOIN LATERAL (
			SELECT ft.throttle_rate, ft.threshold_bytes
			FROM fup_policy_tiers ft
			WHERE ft.policy_id = p.fup_policy_id
			  AND ft.threshold_bytes <= GREATEST(COALESCE(u.bytes_used, 0) - COALESCE(t.topped_up, 0), 0)
			UNION ALL
			SELECT p.fup_throttle_string, p.fup_threshold_bytes
			WHERE p.fup_policy_id IS NULL
			  AND p.fup_threshold_bytes > 0
			  AND COALESCE(p.fup_throttle_string, '') <> ''
			  AND GREATEST(COALESCE(u.bytes_used, 0) - COALESCE(t.topped_up, 0), 0) >= p.fup_threshold_bytes
			ORDER BY threshold_bytes DESC
			LIMIT 1
		) tier ON TRUE
		WHERE s.status IN ('active','grace_period')
		  -- The whole point: only where what is applied differs from what
		  -- should be. A subscriber already on the right rate is skipped,
		  -- which is what stops the 10s tick re-sending CoA forever.
		  AND COALESCE(s.fup_applied_rate, '') <> COALESCE(tier.throttle_rate, '')
		  -- A speed override is a deliberate manual act and outranks fair
		  -- use, so a subscriber under one is left alone entirely rather
		  -- than having their override overwritten on the next tick.
		  AND NOT (s.speed_override_rate_limit IS NOT NULL
		           AND (s.speed_override_expires_at IS NULL OR s.speed_override_expires_at > NOW()))`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: sessions needing a throttle change: %w", err)
	}
	defer rows.Close()

	var out []fup.ThrottleChange
	for rows.Next() {
		var c fup.ThrottleChange
		if err := rows.Scan(&c.SubscriberID, &c.Username, &c.NasIP,
			&c.BytesUsed, &c.ToppedUpBytes, &c.AppliedRate,
			&c.TargetRate, &c.TargetThreshold); err != nil {
			return nil, fmt.Errorf("db: scan throttle change: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetFUPAppliedRate records the rate now applied to a subscriber, or clears
// it with an empty rate.
//
// fup_active is written in the same statement. It is the legacy boolean the
// Access-Accept path, the portal and the console all still read, and letting
// the two disagree would mean a subscriber whose rate says throttled and
// whose flag says not — which is exactly the kind of split state that makes
// a speed complaint unanswerable.
func (s *FUPStore) SetFUPAppliedRate(ctx context.Context, subscriberID int, rate string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE subscribers
		   SET fup_applied_rate = NULLIF($2, ''),
		       fup_active = ($2 <> '')
		 WHERE id = $1`, subscriberID, rate)
	if err != nil {
		return fmt.Errorf("db: set applied rate for subscriber %d: %w", subscriberID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: subscriber %d not found", subscriberID)
	}
	return nil
}

// ActiveTopupBytes is a subscriber's unexpired top-up total, for the portal
// and console to show alongside their usage.
func (s *FUPStore) ActiveTopupBytes(ctx context.Context, subscriberID int) (int64, error) {
	var total int64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(bytes), 0) FROM data_topups
		WHERE subscriber_id = $1 AND expires_at > NOW()`, subscriberID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("db: active top-up bytes for subscriber %d: %w", subscriberID, err)
	}
	return total, nil
}
