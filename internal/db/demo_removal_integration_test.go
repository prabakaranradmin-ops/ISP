//go:build integration

// Removing demo data must survive a walkthrough.
//
// "Load demo data" creates subscribers, plans, a router, a ticket and an
// invoice for a client demonstration, and "Remove demo data" is supposed to
// take all of it out again. Almost nothing that references a subscriber
// cascades — a subscriber is not meant to vanish and take its billing history
// with it — so every table a walkthrough can touch has to be named in
// demoDebris or it blocks the delete with a foreign-key violation, inside one
// transaction, which fails the whole removal.
//
// Until 2026-11-23 three tables were named and twelve were not. Loading the
// demo data and issuing a single credit note against its invoice — an obvious
// thing to show a prospect — left the demo data permanently unremovable
// through the console, which reported only "check the server log".
package db_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// TestDemoRemovalCoversEveryBlockingReference asks the live schema which
// tables can block the deletes Remove performs, and fails when one of them is
// not handled.
//
// This is the check that could not have been fooled. A behavioural test only
// covers the walkthrough steps somebody thought to write down, and the defect
// was precisely a step nobody had: the twelve unhandled tables each had
// correct code, correct tests and no mention here. A migration adding a
// thirteenth would reintroduce the bug silently, so the registry is derived
// from the schema rather than maintained by hand.
//
// Same shape as TestEveryPageTemplateIsRegistered in internal/staffui and
// TestSubscriberRoutesVerifyThePortalKey in internal/api, for the same
// reason: the list, not the behaviour, is where this goes wrong.
func TestDemoRemovalCoversEveryBlockingReference(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	// Every table that blocks a delete from one of the tables Remove empties.
	// Partitions are excluded: deleting from the parent covers them, and they
	// appear here only as children of their own partitioned table.
	const q = `
		SELECT DISTINCT c.conrelid::regclass::text
		  FROM pg_constraint c
		 WHERE c.contype = 'f'
		   AND c.confdeltype IN ('a', 'r')
		   AND c.confrelid::regclass::text IN (
		       'subscribers', 'invoices', 'tickets', 'wallet_ledgers',
		       'plans', 'nas_devices')
		   AND NOT EXISTS (
		       SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.conrelid)
		 ORDER BY 1`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		t.Fatalf("query blocking references: %v", err)
	}
	defer rows.Close()

	var blocking []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan: %v", err)
		}
		blocking = append(blocking, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	// A schema this query finds nothing in would mean it had stopped matching
	// how the constraints are written, and the test would be asserting
	// nothing at all — the failure mode that makes a registry check useless.
	if len(blocking) < 10 {
		t.Fatalf("only %d blocking references found (%v); this query no longer describes the schema "+
			"and is not checking anything", len(blocking), blocking)
	}

	handled := db.DemoRemovalTables()
	var missing []string
	for _, table := range blocking {
		// subscribers and plans block each other and are themselves removed,
		// so they are covered by Remove's own deletes.
		if table == "subscribers" || table == "plans" {
			continue
		}
		if !handled[table] {
			missing = append(missing, table)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("demoDebris does not touch %v.\n"+
			"Each of these has a foreign key into a table Remove deletes from, with ON DELETE "+
			"NO ACTION or RESTRICT, so a walkthrough that writes one row in any of them makes "+
			"\"Remove demo data\" fail as a whole and leaves the demo data in the system for good.",
			missing)
	}
}

// TestDemoRemoval_SurvivesAWalkthroughThatTouchesEveryBlockingTable is the
// behavioural half: one row in each blocking table, then a removal.
//
// Rows are written directly rather than through each feature's own API
// because the point is the foreign keys, not the features — and going through
// twenty console paths would test those paths instead, and would quietly stop
// covering a table whose path changed.
func TestDemoRemoval_SurvivesAWalkthroughThatTouchesEveryBlockingTable(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Real plan", "50M/50M", 0, "", "499.00")
	seedPlan(ctx, t, pool, 2, "Demo Fiber 100", "100M/100M", 0, "", "999.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "real@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "demo@isp", PlanID: 2})

	demo := database.Demo()
	if err := demo.MarkPlanDemo(ctx, 2); err != nil {
		t.Fatalf("MarkPlanDemo: %v", err)
	}
	if err := demo.MarkSubscriberDemo(ctx, 2); err != nil {
		t.Fatalf("MarkSubscriberDemo: %v", err)
	}

	// The walkthrough: a ticket, an invoice, a wallet entry, a notification, a
	// session — the things a demonstration actually shows.
	seedGstRate(ctx, t, pool, 1)
	seedWalletCredit(ctx, t, pool, 2, "118.00", "cash", "billing1")
	seedSession(ctx, t, pool, 2, "demo-session-1", "10.0.0.1", 1<<20, 1<<19)
	for _, stmt := range []string{
		`INSERT INTO tickets (subscriber_id, category, description)
		 VALUES (2, 'connectivity', 'Speed drops every evening around 8pm.')`,
		`INSERT INTO invoices (subscriber_id, base_amount, total_amount, gst_rate_id,
		                       gb_included, gb_used)
		 VALUES (2, 100, 118, 1, 100, 62)`,
		`INSERT INTO notification_log (subscriber_id, channel, triggered_by_event)
		 VALUES (2, 'sms', 'demo.walkthrough')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("walkthrough step failed (%s): %v", firstSQLLine(stmt), err)
		}
	}

	// The exact action that found the defect: a credit note on the demo
	// invoice, which lands as an approval_requests row referencing the
	// invoice and not only the subscriber.
	if _, err := pool.Exec(ctx, `
		INSERT INTO approval_requests (subscriber_id, invoice_id, action_type, amount, reason,
		                               requested_by_username)
		 VALUES (2, (SELECT id FROM invoices WHERE subscriber_id = 2),
		         'credit_note', 100, 'demo walkthrough: billed in error', 'billing1')`); err != nil {
		t.Fatalf("credit note request: %v", err)
	}

	// A live session, the thing a demonstration most wants to show: the demo
	// subscriber online right now.
	if _, err := pool.Exec(ctx, `
		INSERT INTO live_sessions (subscriber_id, session_id, started_at)
		 VALUES (2, 'demo-live-1', now())`); err != nil {
		t.Fatalf("live session: %v", err)
	}

	if err := demo.Remove(ctx); err != nil {
		t.Fatalf("removing demo data after a walkthrough must succeed, got: %v", err)
	}

	// The demo data is gone and the real subscriber is untouched.
	var demoSubs, demoPlans, realSubs int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM subscribers WHERE is_demo),
		       (SELECT count(*) FROM plans WHERE is_demo),
		       (SELECT count(*) FROM subscribers WHERE NOT is_demo)`).
		Scan(&demoSubs, &demoPlans, &realSubs); err != nil {
		t.Fatalf("count after removal: %v", err)
	}
	if demoSubs != 0 || demoPlans != 0 {
		t.Errorf("demo data survived removal: %d subscribers, %d plans", demoSubs, demoPlans)
	}
	if realSubs != 1 {
		t.Errorf("removal took a real subscriber with it: %d left, want 1", realSubs)
	}
}

// TestDemoRemoval_RefusesWhenARealSubscriberIsOnADemoPlan covers the case
// where failing is correct: the plan cannot go while somebody's actual
// customer is on it, and that customer must certainly not be deleted.
func TestDemoRemoval_RefusesWhenARealSubscriberIsOnADemoPlan(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Demo Fiber 100", "100M/100M", 0, "", "999.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "real-but-on-demo-plan@isp", PlanID: 1})

	demo := database.Demo()
	if err := demo.MarkPlanDemo(ctx, 1); err != nil {
		t.Fatalf("MarkPlanDemo: %v", err)
	}

	err := demo.Remove(ctx)
	if err == nil {
		t.Fatal("removal must refuse while a real subscriber is on a demo plan")
	}
	if !errors.Is(err, staffui.ErrDemoDataInUse) {
		t.Errorf("the refusal must be ErrDemoDataInUse so the console can say what to do, got %v", err)
	}
	if !strings.Contains(err.Error(), "move them to a real plan") {
		t.Errorf("the refusal must say what the operator should do, got %q", err)
	}

	// Nothing may have been removed: the refusal happens before any delete,
	// and the whole thing is one transaction.
	var subs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM subscribers`).Scan(&subs); err != nil {
		t.Fatalf("count: %v", err)
	}
	if subs != 1 {
		t.Errorf("a refused removal deleted something anyway: %d subscribers left, want 1", subs)
	}
}

func firstSQLLine(stmt string) string {
	if i := strings.IndexByte(stmt, '\n'); i >= 0 {
		return strings.TrimSpace(stmt[:i]) + " ..."
	}
	return strings.TrimSpace(stmt)
}
