package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// Reusable bandwidth profiles — FR-CAT-002 | migration 065. The domain rules
// (rendering, validation) live in internal/nas/bandwidth.go; this is storage.

// The sentinels live in internal/nas so the console can tell these outcomes
// apart from a generic failure; see internal/nas/bandwidth.go for why they
// cannot live here.

const bwColumns = `
	b.id, b.name,
	b.max_down_bps, b.max_up_bps, b.min_down_bps, b.min_up_bps,
	b.burst_down_bps, b.burst_up_bps,
	b.burst_threshold_down, b.burst_threshold_up, b.burst_time_seconds,
	b.priority, b.active,
	COALESCE(b.created_by, ''), b.created_at, COALESCE(b.updated_by, ''), b.updated_at`

// The same columns without the alias, for RETURNING clauses, which have
// none. Written out rather than derived from bwColumns because a const is
// needed to build the queries as constants.
const bwReturning = `
	id, name,
	max_down_bps, max_up_bps, min_down_bps, min_up_bps,
	burst_down_bps, burst_up_bps,
	burst_threshold_down, burst_threshold_up, burst_time_seconds,
	priority, active,
	COALESCE(created_by, ''), created_at, COALESCE(updated_by, ''), updated_at`

func scanBandwidthProfile(row pgx.Row, p *nas.BandwidthProfile, inUse *int) error {
	dest := []any{
		&p.ID, &p.Name,
		&p.MaxDownBPS, &p.MaxUpBPS, &p.MinDownBPS, &p.MinUpBPS,
		&p.BurstDownBPS, &p.BurstUpBPS,
		&p.BurstThresholdDown, &p.BurstThresholdUp, &p.BurstTimeSeconds,
		&p.Priority, &p.Active,
		&p.CreatedBy, &p.CreatedAt, &p.UpdatedBy, &p.UpdatedAt,
	}
	if inUse != nil {
		dest = append(dest, inUse)
	}
	return row.Scan(dest...)
}

// nameTaken maps the partial unique index to a readable error. 23505 is
// unique_violation.
func nameTaken(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "uq_bandwidth_profile_name_active" {
		return nas.ErrProfileNameTaken
	}
	return err
}

// CreateBandwidthProfile stores a new profile.
func (s *NASStore) CreateBandwidthProfile(ctx context.Context, in nas.BandwidthProfile, by string) (*nas.BandwidthProfile, error) {
	in = in.Normalise()
	if err := in.Validate(); err != nil {
		return nil, err
	}
	const q = `
		INSERT INTO bandwidth_profiles (
			name, max_down_bps, max_up_bps, min_down_bps, min_up_bps,
			burst_down_bps, burst_up_bps, burst_threshold_down, burst_threshold_up,
			burst_time_seconds, priority, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING ` + bwReturning
	var out nas.BandwidthProfile
	err := scanBandwidthProfile(s.pool.QueryRow(ctx, q,
		in.Name, in.MaxDownBPS, in.MaxUpBPS, in.MinDownBPS, in.MinUpBPS,
		in.BurstDownBPS, in.BurstUpBPS, in.BurstThresholdDown, in.BurstThresholdUp,
		in.BurstTimeSeconds, in.Priority, by), &out, nil)
	if err != nil {
		return nil, fmt.Errorf("db: create bandwidth profile: %w", nameTaken(err))
	}
	return &out, nil
}

// UpdateBandwidthProfile changes a profile in place.
//
// This re-shapes every plan on the profile, and therefore every subscriber on
// those plans, from their next Access-Accept or CoA. That is the point of a
// reusable profile, but it is also why the console shows the plan count
// before confirming.
func (s *NASStore) UpdateBandwidthProfile(ctx context.Context, id int, in nas.BandwidthProfile, by string) (*nas.BandwidthProfile, error) {
	in = in.Normalise()
	if err := in.Validate(); err != nil {
		return nil, err
	}
	const q = `
		UPDATE bandwidth_profiles SET
			name = $2, max_down_bps = $3, max_up_bps = $4,
			min_down_bps = $5, min_up_bps = $6,
			burst_down_bps = $7, burst_up_bps = $8,
			burst_threshold_down = $9, burst_threshold_up = $10,
			burst_time_seconds = $11, priority = $12,
			updated_by = $13, updated_at = NOW()
		WHERE id = $1
		RETURNING ` + bwReturning
	var out nas.BandwidthProfile
	err := scanBandwidthProfile(s.pool.QueryRow(ctx, q, id,
		in.Name, in.MaxDownBPS, in.MaxUpBPS, in.MinDownBPS, in.MinUpBPS,
		in.BurstDownBPS, in.BurstUpBPS, in.BurstThresholdDown, in.BurstThresholdUp,
		in.BurstTimeSeconds, in.Priority, by), &out, nil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("db: bandwidth profile %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("db: update bandwidth profile %d: %w", id, nameTaken(err))
	}
	return &out, nil
}

// SetBandwidthProfileActive retires or restores a profile.
//
// Retiring takes it out of the dropdowns and leaves every plan already on it
// exactly as it is — the same rule plans (061), products (058) and IP pools
// (060) follow, and the reason there is no delete for a profile in use.
func (s *NASStore) SetBandwidthProfileActive(ctx context.Context, id int, active bool, by string) error {
	const q = `UPDATE bandwidth_profiles SET active = $2, updated_by = $3, updated_at = NOW() WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, id, active, by)
	if err != nil {
		return fmt.Errorf("db: set bandwidth profile %d active=%v: %w", id, active, nameTaken(err))
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: bandwidth profile %d not found", id)
	}
	return nil
}

// DeleteBandwidthProfile removes a profile no plan references.
//
// The foreign key is ON DELETE RESTRICT, so this is checked by the database
// either way; the count is read first only so the caller can be told to
// retire it instead of being handed a constraint violation.
func (s *NASStore) DeleteBandwidthProfile(ctx context.Context, id int) error {
	var inUse int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM plans WHERE bandwidth_profile_id = $1`, id).Scan(&inUse); err != nil {
		return fmt.Errorf("db: count plans on bandwidth profile %d: %w", id, err)
	}
	if inUse > 0 {
		return fmt.Errorf("%w: %d plan(s)", nas.ErrProfileInUse, inUse)
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM bandwidth_profiles WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("db: delete bandwidth profile %d: %w", id, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: bandwidth profile %d not found", id)
	}
	return nil
}

// ListBandwidthProfiles returns the profiles with their plan counts. Retired
// profiles are included only when includeRetired is set, so the dropdowns
// get the live ones without having to filter.
func (s *NASStore) ListBandwidthProfiles(ctx context.Context, includeRetired bool) ([]nas.BandwidthProfile, error) {
	q := `
		SELECT ` + bwColumns + `,
		       (SELECT count(*) FROM plans p WHERE p.bandwidth_profile_id = b.id)
		FROM bandwidth_profiles b`
	if !includeRetired {
		q += ` WHERE b.active`
	}
	q += ` ORDER BY b.active DESC, lower(btrim(b.name))`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list bandwidth profiles: %w", err)
	}
	defer rows.Close()

	var out []nas.BandwidthProfile
	for rows.Next() {
		var p nas.BandwidthProfile
		if err := scanBandwidthProfile(rows, &p, &p.InUseByPlans); err != nil {
			return nil, fmt.Errorf("db: scan bandwidth profile: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// BandwidthProfileByID reads one profile.
func (s *NASStore) BandwidthProfileByID(ctx context.Context, id int) (*nas.BandwidthProfile, error) {
	const q = `
		SELECT ` + bwColumns + `,
		       (SELECT count(*) FROM plans p WHERE p.bandwidth_profile_id = b.id)
		FROM bandwidth_profiles b WHERE b.id = $1`
	var p nas.BandwidthProfile
	err := scanBandwidthProfile(s.pool.QueryRow(ctx, q, id), &p, &p.InUseByPlans)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("db: bandwidth profile %d not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("db: read bandwidth profile %d: %w", id, err)
	}
	return &p, nil
}

// SetPlanBandwidthProfile attaches a profile to a plan, or clears it with a
// nil profileID so the plan goes back to shaping from rate_limit_string.
func (s *NASStore) SetPlanBandwidthProfile(ctx context.Context, planID int, profileID *int, by string) error {
	const q = `
		UPDATE plans SET bandwidth_profile_id = $2, updated_by = $3, updated_at = NOW()
		WHERE id = $1`
	tag, err := s.pool.Exec(ctx, q, planID, profileID, by)
	if err != nil {
		return fmt.Errorf("db: set plan %d bandwidth profile: %w", planID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: plan %d not found", planID)
	}
	return nil
}
