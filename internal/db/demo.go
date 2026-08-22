package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// DemoStore backs the console's "Demo Data" panel (staffui.DemoStore):
// tagging rows created for a client walkthrough as is_demo, reporting how
// much of it currently exists, and removing exactly those rows again.
//
// Marking happens as a follow-up UPDATE after an ordinary create (see
// MarkSubscriberDemo etc.) rather than by threading an IsDemo field through
// CreateSubscriber/CreatePlan/CreateNASDevice themselves — those paths are
// shared with real signups and tariff/router changes, and the smallest safe
// change is one that touches none of their INSERT statements.
type DemoStore struct{ pool dbPool }

// NewDemoStore constructs a DemoStore.
func NewDemoStore(pool dbPool) *DemoStore { return &DemoStore{pool: pool} }

// MarkSubscriberDemo flags a just-created subscriber as demo data.
func (s *DemoStore) MarkSubscriberDemo(ctx context.Context, id int) error {
	if _, err := s.pool.Exec(ctx, `UPDATE subscribers SET is_demo = true WHERE id = $1`, id); err != nil {
		return fmt.Errorf("db: mark subscriber %d demo: %w", id, err)
	}
	return nil
}

// MarkPlanDemo flags a just-created plan as demo data.
func (s *DemoStore) MarkPlanDemo(ctx context.Context, id int) error {
	if _, err := s.pool.Exec(ctx, `UPDATE plans SET is_demo = true WHERE id = $1`, id); err != nil {
		return fmt.Errorf("db: mark plan %d demo: %w", id, err)
	}
	return nil
}

// MarkNASDemo flags a just-created NAS device as demo data.
func (s *DemoStore) MarkNASDemo(ctx context.Context, id int) error {
	if _, err := s.pool.Exec(ctx, `UPDATE nas_devices SET is_demo = true WHERE id = $1`, id); err != nil {
		return fmt.Errorf("db: mark nas device %d demo: %w", id, err)
	}
	return nil
}

// Status counts demo rows currently in place, for the console to decide
// whether "Load demo data" or "Remove demo data" is the sensible next click.
//
// Returns staffui.DemoStatus rather than a type of its own — the same
// direction as CatalogueStore returning []staffui.Plan: staffui owns the
// shape its templates render, this package only fills it in.
func (s *DemoStore) Status(ctx context.Context) (staffui.DemoStatus, error) {
	var st staffui.DemoStatus
	const q = `SELECT
		(SELECT count(*) FROM subscribers WHERE is_demo),
		(SELECT count(*) FROM plans WHERE is_demo),
		(SELECT count(*) FROM nas_devices WHERE is_demo)`
	if err := s.pool.QueryRow(ctx, q).Scan(&st.Subscribers, &st.Plans, &st.NASDevices); err != nil {
		return staffui.DemoStatus{}, fmt.Errorf("db: demo status: %w", err)
	}
	return st, nil
}

// Remove deletes every row this package's Mark* methods have ever flagged.
//
// Demo subscribers' tickets, invoices and wallet ledger entries are deleted
// explicitly first: none of those tables cascade from subscribers (a
// deliberate choice elsewhere in the schema — a subscriber is not supposed
// to vanish and take its billing history with it), so leaving them out here
// would fail the subscriber delete with a foreign-key violation instead of
// silently orphaning anything. Everything runs in one transaction so a
// failure partway through cannot leave demo data half-removed.
func (s *DemoStore) Remove(ctx context.Context) error {
	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		stmts := []string{
			`DELETE FROM tickets        WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
			`DELETE FROM invoices       WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
			`DELETE FROM wallet_ledgers WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
			`DELETE FROM subscribers WHERE is_demo`,
			`DELETE FROM plans       WHERE is_demo`,
			`DELETE FROM nas_devices WHERE is_demo`,
		}
		for _, stmt := range stmts {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("db: remove demo data (%s): %w", stmt, err)
			}
		}
		return nil
	})
}
