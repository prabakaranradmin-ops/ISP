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

// ListPlans returns every plan, newest first.
func (s *CatalogueStore) ListPlans(ctx context.Context) ([]staffui.Plan, error) {
	const q = `
		SELECT id, name, rate_limit_string, volume_gb, fup_threshold_bytes,
		       COALESCE(fup_throttle_string, ''), price, validity_days, created_at
		  FROM plans
		 ORDER BY id DESC`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list plans: %w", err)
	}
	defer rows.Close()

	var out []staffui.Plan
	for rows.Next() {
		var p staffui.Plan
		if err := rows.Scan(&p.ID, &p.Name, &p.RateLimitString, &p.VolumeGB,
			&p.FUPThresholdBytes, &p.FUPThrottleString, &p.Price,
			&p.ValidityDays, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan plan: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
		RETURNING id, name, rate_limit_string, volume_gb, fup_threshold_bytes,
		          COALESCE(fup_throttle_string, ''), price, validity_days, created_at`

	var out staffui.Plan
	if err := s.pool.QueryRow(ctx, q,
		p.Name, p.RateLimitString, p.VolumeGB, p.FUPThresholdBytes,
		throttle, p.Price, p.ValidityDays,
	).Scan(&out.ID, &out.Name, &out.RateLimitString, &out.VolumeGB,
		&out.FUPThresholdBytes, &out.FUPThrottleString, &out.Price,
		&out.ValidityDays, &out.CreatedAt); err != nil {
		return nil, fmt.Errorf("db: create plan: %w", err)
	}
	return &out, nil
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
