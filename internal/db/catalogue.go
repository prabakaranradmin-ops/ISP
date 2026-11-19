package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// CatalogueStore serves the tariff catalogue the operations console
// manages: plans and dated GST rates.
type CatalogueStore struct {
	pool dbPool
}

// NewCatalogueStore constructs a CatalogueStore.
func NewCatalogueStore(pool dbPool) *CatalogueStore { return &CatalogueStore{pool: pool} }

// planColumns is the stored plan plus how many subscribers hold it.
const planColumns = `
	p.id, p.name, p.rate_limit_string, p.volume_gb, p.fup_threshold_bytes,
	COALESCE(p.fup_throttle_string, ''), p.price, p.validity_days, p.active,
	p.created_at, COALESCE(p.updated_by, ''), p.updated_at,
	(SELECT COUNT(*) FROM subscribers s WHERE s.plan_id = p.id AND s.status <> 'terminated')`

func scanPlan(row interface{ Scan(...any) error }, p *staffui.Plan) error {
	return row.Scan(&p.ID, &p.Name, &p.RateLimitString, &p.VolumeGB,
		&p.FUPThresholdBytes, &p.FUPThrottleString, &p.Price,
		&p.ValidityDays, &p.Active, &p.CreatedAt, &p.UpdatedBy, &p.UpdatedAt,
		&p.Subscribers)
}

// ListPlans returns every plan, offered ones first then newest.
//
// Retired plans are included: the screen that manages them has to show
// them, and a subscriber on one needs their tariff to be explicable.
func (s *CatalogueStore) ListPlans(ctx context.Context) ([]staffui.Plan, error) {
	return s.listPlans(ctx, `
		SELECT `+planColumns+`
		  FROM plans p
		 ORDER BY p.active DESC, p.id DESC`)
}

// ListActivePlans returns only the plans that may still be assigned.
//
// This is what every dropdown that puts a subscriber on a plan reads, so
// a retired tariff cannot be sold to anybody new (FR-CAT-001).
func (s *CatalogueStore) ListActivePlans(ctx context.Context) ([]staffui.Plan, error) {
	return s.listPlans(ctx, `
		SELECT `+planColumns+`
		  FROM plans p
		 WHERE p.active
		 ORDER BY p.name`)
}

func (s *CatalogueStore) listPlans(ctx context.Context, q string) ([]staffui.Plan, error) {
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list plans: %w", err)
	}
	defer rows.Close()

	var out []staffui.Plan
	for rows.Next() {
		var p staffui.Plan
		if err := scanPlan(rows, &p); err != nil {
			return nil, fmt.Errorf("db: scan plan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePlan changes a tariff in place.
//
// Editing the price re-prices every subscriber on the plan from their next
// renewal, because the renewal path reads plans.price at the time it bills
// (unlike a product attachment, which snapshots its price — see migration
// 058). That is what a tariff change means, and it is why this is
// attributed and audited, and why the console shows how many subscribers
// an edit affects before it is made.
//
// A rate-limit change reaches the NAS as radiusd's subscriber auth cache
// expires, within its 60-second TTL — that cache is process-local and this
// process cannot reach it (see subCacheInvalidator in cmd/api).
func (s *CatalogueStore) UpdatePlan(ctx context.Context, p staffui.Plan, by string) (*staffui.Plan, error) {
	var throttle *string
	if p.FUPThrottleString != "" {
		throttle = &p.FUPThrottleString
	}
	const q = `
		UPDATE plans
		   SET name = $2, rate_limit_string = $3, volume_gb = $4,
		       fup_threshold_bytes = $5, fup_throttle_string = $6,
		       price = $7, validity_days = $8,
		       updated_by = $9, updated_at = now()
		 WHERE id = $1
		RETURNING id`

	var id int
	err := s.pool.QueryRow(ctx, q, p.ID, p.Name, p.RateLimitString, p.VolumeGB,
		p.FUPThresholdBytes, throttle, p.Price, p.ValidityDays, by).Scan(&id)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: plan %d: %w", p.ID, ErrNotFound)
	}
	if err != nil {
		if uniqueViolation(err, "uq_plan_name_active") {
			return nil, fmt.Errorf("%w: %s", staffui.ErrPlanNameTaken, p.Name)
		}
		return nil, fmt.Errorf("db: update plan %d: %w", p.ID, err)
	}
	return s.GetPlan(ctx, id)
}

// GetPlan loads one plan.
func (s *CatalogueStore) GetPlan(ctx context.Context, id int) (*staffui.Plan, error) {
	var out staffui.Plan
	err := scanPlan(s.pool.QueryRow(ctx,
		`SELECT `+planColumns+` FROM plans p WHERE p.id = $1`, id), &out)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: plan %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get plan %d: %w", id, err)
	}
	return &out, nil
}

// SetPlanActive retires or reinstates a plan.
//
// Retiring hides it from the dropdowns that assign one; it does not move
// anybody. The subscribers on it keep renewing at its price, which is the
// tariff they bought — the same rule products (058) and IP pools (060)
// follow, stated first here.
func (s *CatalogueStore) SetPlanActive(ctx context.Context, id int, active bool, by string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE plans SET active = $2, updated_by = $3, updated_at = now()
		 WHERE id = $1`, id, active, by)
	if err != nil {
		if uniqueViolation(err, "uq_plan_name_active") {
			// Reinstating a plan whose name a live one has taken since.
			return fmt.Errorf("%w: another offered plan already uses that name", staffui.ErrPlanNameTaken)
		}
		return fmt.Errorf("db: set plan %d active=%v: %w", id, active, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: plan %d: %w", id, ErrNotFound)
	}
	return nil
}

// CreatePlan inserts a plan and returns it as stored.
func (s *CatalogueStore) CreatePlan(ctx context.Context, p staffui.Plan) (*staffui.Plan, error) {
	// An empty throttle is stored as NULL rather than "": the column's
	// documented meaning for "no throttle" is NULL, and internal/fup reads
	// it that way.
	var throttle *string
	if p.FUPThrottleString != "" {
		throttle = &p.FUPThrottleString
	}

	const q = `
		INSERT INTO plans (name, rate_limit_string, volume_gb, fup_threshold_bytes,
		                   fup_throttle_string, price, validity_days)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`

	var id int
	if err := s.pool.QueryRow(ctx, q,
		p.Name, p.RateLimitString, p.VolumeGB, p.FUPThresholdBytes,
		throttle, p.Price, p.ValidityDays,
	).Scan(&id); err != nil {
		if uniqueViolation(err, "uq_plan_name_active") {
			return nil, fmt.Errorf("%w: %s", staffui.ErrPlanNameTaken, p.Name)
		}
		return nil, fmt.Errorf("db: create plan: %w", err)
	}
	return s.GetPlan(ctx, id)
}

// ListGSTRates returns every GST slab, most recently effective first.
func (s *CatalogueStore) ListGSTRates(ctx context.Context) ([]staffui.GSTRate, error) {
	const q = `
		SELECT id, cgst_rate, sgst_rate, igst_rate, effective_from
		  FROM gst_rates
		 ORDER BY effective_from DESC, id DESC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list gst rates: %w", err)
	}
	defer rows.Close()

	var out []staffui.GSTRate
	for rows.Next() {
		var g staffui.GSTRate
		if err := rows.Scan(&g.ID, &g.CGSTRate, &g.SGSTRate, &g.IGSTRate, &g.EffectiveFrom); err != nil {
			return nil, fmt.Errorf("db: scan gst rate: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateGSTRate inserts a dated GST slab and returns it as stored.
//
// Insert-only, never update: billing resolves the rate in force on an
// invoice's date, so editing a row in place would silently restate the tax
// on invoices already issued under it.
func (s *CatalogueStore) CreateGSTRate(ctx context.Context, g staffui.GSTRate) (*staffui.GSTRate, error) {
	const q = `
		INSERT INTO gst_rates (cgst_rate, sgst_rate, igst_rate, effective_from)
		VALUES ($1, $2, $3, $4)
		RETURNING id, cgst_rate, sgst_rate, igst_rate, effective_from`

	var out staffui.GSTRate
	if err := s.pool.QueryRow(ctx, q,
		g.CGSTRate, g.SGSTRate, g.IGSTRate, g.EffectiveFrom,
	).Scan(&out.ID, &out.CGSTRate, &out.SGSTRate, &out.IGSTRate, &out.EffectiveFrom); err != nil {
		return nil, fmt.Errorf("db: create gst rate: %w", err)
	}
	return &out, nil
}

// Catalogue returns the catalogue store for this database.
func (d *DB) Catalogue() *CatalogueStore { return NewCatalogueStore(d.pool) }
