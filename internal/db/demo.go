package db

import (
	"context"
	"fmt"
	"strings"

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

// demoDebris lists every table a walkthrough can leave pointing at demo rows,
// ordered so that children go before their parents.
//
// None of these cascade from subscribers — deliberately, since a subscriber is
// not supposed to vanish and take its billing history with it — so each has to
// be named here or it blocks the delete with a foreign-key violation. Being
// is_demo is not inherited either: these rows are identified by the demo
// subscriber, invoice, wallet entry, plan or router they hang off.
//
// Found 2026-11-23 by loading demo data through the console, issuing one
// credit note against the demo invoice the way a sales walkthrough would, and
// then being unable to remove any of it. Only tickets, invoices and
// wallet_ledgers were handled, so the first walkthrough action touching
// anything else wedged "Remove demo data" permanently: the console said
// "check the server log" and the fabricated subscribers, plans and invoices
// stayed in the system with no way out through the UI. The credit note did
// not even fail on a reference to the subscriber — it failed on
// approval_requests.invoice_id — which is why this list follows every row
// Remove deletes rather than only the subscribers.
//
// TestDemoRemovalCoversEveryBlockingReference derives the same set from the
// live schema and fails when a migration adds one that is not here.
var demoDebris = []string{
	// Approvals, payments, credit notes and refunds reference the subscriber,
	// its invoices and its wallet entries, so they go before all three.
	`DELETE FROM approval_requests WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)
	    OR invoice_id IN (SELECT id FROM invoices WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))
	    OR ledger_entry_id IN (SELECT id FROM wallet_ledgers WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))`,
	`DELETE FROM payment_details WHERE wallet_ledger_id IN (SELECT id FROM wallet_ledgers WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))
	    OR bounce_ledger_id IN (SELECT id FROM wallet_ledgers WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))`,
	`DELETE FROM payment_refunds WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)
	    OR ledger_entry_id IN (SELECT id FROM wallet_ledgers WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))`,
	`DELETE FROM credit_notes WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)
	    OR invoice_id IN (SELECT id FROM invoices WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))
	    OR wallet_ledger_id IN (SELECT id FROM wallet_ledgers WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))`,
	`DELETE FROM data_topups WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)
	    OR transaction_id IN (SELECT id FROM wallet_ledgers WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))`,
	`DELETE FROM proforma_invoices WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)
	    OR invoice_id IN (SELECT id FROM invoices WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))`,
	`DELETE FROM subscriber_products WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)
	    OR invoice_id IN (SELECT id FROM invoices WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))`,
	// A scheduled plan change names the demo plan from three columns, one of
	// which (from_plan_id) is NOT NULL, so the row goes rather than the link.
	`DELETE FROM scheduled_plan_changes WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)
	    OR new_plan_id IN (SELECT id FROM plans WHERE is_demo)
	    OR from_plan_id IN (SELECT id FROM plans WHERE is_demo)
	    OR applied_invoice_id IN (SELECT id FROM invoices WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo))`,

	// The demo subscriber's own operational and billing trail.
	`DELETE FROM cgnat_allocations          WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM cpe_devices                WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM field_tasks                WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM lco_ledger                 WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM live_sessions              WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM notification_log           WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM subscriber_session_history WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM tickets                    WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM invoices                   WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,
	`DELETE FROM wallet_ledgers             WHERE subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,

	// A lead converted during the walkthrough keeps its row and loses the
	// link. It may well have been a real lead before somebody demonstrated
	// conversion on it, and destroying real CRM data to tidy up a demo is the
	// wrong trade; the column is nullable for exactly this.
	`UPDATE leads SET converted_subscriber_id = NULL
	  WHERE converted_subscriber_id IN (SELECT id FROM subscribers WHERE is_demo)`,

	`DELETE FROM subscribers WHERE is_demo`,

	// Then the demo plans. A real subscriber's status history can name one —
	// they were moved onto it during the walkthrough — and that row is audit
	// data, so it keeps its history and loses the plan link instead of being
	// deleted. Demo subscribers' own history cascaded away with them above.
	`UPDATE subscriber_status_history SET plan_id = NULL WHERE plan_id IN (SELECT id FROM plans WHERE is_demo)`,
	`DELETE FROM announcements     WHERE segment_plan_id IN (SELECT id FROM plans WHERE is_demo)`,
	`DELETE FROM hotspot_grants    WHERE plan_id IN (SELECT id FROM plans WHERE is_demo)`,
	`DELETE FROM hotspot_vouchers  WHERE plan_id IN (SELECT id FROM plans WHERE is_demo)`,
	`DELETE FROM plan_nas_profiles WHERE plan_id IN (SELECT id FROM plans WHERE is_demo)`,
	`DELETE FROM plans WHERE is_demo`,

	`DELETE FROM ip_pools    WHERE nas_id IN (SELECT id FROM nas_devices WHERE is_demo)`,
	`DELETE FROM nas_devices WHERE is_demo`,
}

// Remove deletes every row this package's Mark* methods have ever flagged,
// together with whatever a walkthrough left hanging off it (demoDebris).
//
// Everything runs in one transaction, so a failure partway through cannot
// leave demo data half-removed — which also means one unhandled dependent
// blocks the whole removal rather than part of it.
//
// It refuses with ErrDemoDataInUse when a real subscriber is on a demo plan:
// that subscriber is somebody's actual customer, deleting them to tidy up a
// demo would be absurd, and deleting the plan from under them is impossible.
// The operator moves them to a real plan and removes again.
func (s *DemoStore) Remove(ctx context.Context) error {
	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		var realOnDemoPlan int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM subscribers
			 WHERE NOT is_demo AND plan_id IN (SELECT id FROM plans WHERE is_demo)`).Scan(&realOnDemoPlan); err != nil {
			return fmt.Errorf("db: check for real subscribers on demo plans: %w", err)
		}
		if realOnDemoPlan > 0 {
			return fmt.Errorf("%w: %d real subscriber(s) are on a demo plan; move them to a real plan first",
				staffui.ErrDemoDataInUse, realOnDemoPlan)
		}

		for _, stmt := range demoDebris {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("db: remove demo data (%s): %w", firstLine(stmt), err)
			}
		}
		return nil
	})
}

// firstLine trims a multi-line statement to something a log line can carry;
// quoting the whole statement buried the error it was reporting.
func firstLine(stmt string) string {
	if i := strings.IndexByte(stmt, '\n'); i >= 0 {
		return strings.TrimSpace(stmt[:i]) + " ..."
	}
	return strings.TrimSpace(stmt)
}

// DemoRemovalTables reports which tables Remove touches, derived from the
// statements themselves rather than from a second list that could drift.
//
// Exported for TestDemoRemovalCoversEveryBlockingReference, which compares it
// against the foreign keys the live schema actually has.
func DemoRemovalTables() map[string]bool {
	out := make(map[string]bool, len(demoDebris))
	for _, stmt := range demoDebris {
		fields := strings.Fields(stmt)
		for i, f := range fields {
			switch strings.ToUpper(f) {
			case "FROM", "UPDATE":
				if i+1 < len(fields) {
					out[fields[i+1]] = true
				}
			}
			// Only the leading DELETE FROM / UPDATE names the target; the
			// subselects that follow are reads, so stop at the first WHERE.
			if strings.ToUpper(f) == "WHERE" {
				break
			}
		}
	}
	return out
}
