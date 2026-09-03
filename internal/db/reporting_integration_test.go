//go:build integration

// Reporting view tests — FR-RPT-001, FR-RPT-003 | migration 032 | MDS §4.8.
//
// The views aggregate; these tests are about whether the aggregation says
// something true. A report that is merely plausible is worse than no report,
// because nobody checks it — so the assertions here target the judgement calls
// baked into the SQL (suspension is not churn, first resolution not last,
// no-billing is not 0% collection) rather than just row counts.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// refreshTicketView rebuilds the materialised view so a test can read it.
func refreshTicketView(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `REFRESH MATERIALIZED VIEW CONCURRENTLY mv_ticket_resolution`); err != nil {
		t.Fatalf("refresh mv_ticket_resolution: %v", err)
	}
}

func TestFR_RPT_001_PlanMixCountsOnlyActiveTowardMRR(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Gold", "100M/100M", 0, "", "1000.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "c@isp", PlanID: 1, Status: "hard_suspended"})

	rows, err := database.Reporting().PlanMix(ctx, nil)
	if err != nil {
		t.Fatalf("PlanMix: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one plan row, got %d", len(rows))
	}
	got := rows[0]
	if got.TotalSubscribers != 3 {
		t.Errorf("total_subscribers: want 3, got %d", got.TotalSubscribers)
	}
	if got.ActiveSubscribers != 2 || got.SuspendedSubscribers != 1 {
		t.Errorf("active/suspended: want 2/1, got %d/%d", got.ActiveSubscribers, got.SuspendedSubscribers)
	}
	// The judgement call: a suspended subscriber is not producing revenue.
	assertDecimalEqual(t, "mrr excludes suspended subscribers", got.MRR, "2000.00")
}

func TestFR_RPT_001_GrowthSeparatesChurnFromSuspension(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	// Three real signups (the INSERT trigger records each).
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "keep@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "susp@isp"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "gone@isp"})

	opCtx := middleware.WithSubject(ctx, "csr:test")
	suspended := "hard_suspended"
	if _, err := database.API().UpdateSubscriber(opCtx, 2, nil, &suspended, nil); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := database.API().TerminateSubscriber(opCtx, 3); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	rows, err := database.Reporting().GrowthMonthly(ctx, 1, nil)
	if err != nil {
		t.Fatalf("GrowthMonthly: %v", err)
	}
	var newConn, churned, susp, net int
	for _, r := range rows {
		newConn += r.NewConnections
		churned += r.Churned
		susp += r.Suspended
		net += r.NetGrowth
	}
	if newConn != 3 {
		t.Errorf("new_connections: want 3, got %d", newConn)
	}
	// The judgement call this view is built around: a suspension is a
	// collections event that usually reverses, and folding it into churn
	// makes every dunning run look like a customer exodus.
	if churned != 1 {
		t.Errorf("churned: want 1 (the termination only), got %d", churned)
	}
	if susp != 1 {
		t.Errorf("suspended: want 1, reported separately from churn, got %d", susp)
	}
	if net != 2 {
		t.Errorf("net_growth: want 3 signups - 1 churn = 2, got %d", net)
	}
}

// TestFR_RPT_001_GrowthIgnoresTheSeededBaseline is the guard on migration
// 031's snapshot rows. Counting one as a signup would draw a growth curve for
// a period nobody actually observed.
func TestFR_RPT_001_GrowthIgnoresTheSeededBaseline(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "pre@isp"})

	// Replace the real creation event with a baseline snapshot, which is the
	// state a pre-migration account is in.
	if _, err := pool.Exec(ctx, `
		UPDATE subscriber_status_history SET is_baseline = TRUE, old_status = NULL
		 WHERE subscriber_id = 1`); err != nil {
		t.Fatalf("mark baseline: %v", err)
	}

	rows, err := database.Reporting().GrowthMonthly(ctx, 1, nil)
	if err != nil {
		t.Fatalf("GrowthMonthly: %v", err)
	}
	for _, r := range rows {
		if r.NewConnections != 0 {
			t.Errorf("a baseline snapshot was counted as a new connection (%d) — "+
				"that invents a signup curve for a period with no capture", r.NewConnections)
		}
	}
}

// TestFR_RPT_001_ResolutionUsesFirstNotLast is the reopen case. Taking the
// last resolution would report a reopened ticket as one slow success and hide
// the failure entirely.
func TestFR_RPT_001_ResolutionUsesFirstNotLast(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "t@isp"})
	seedStaffUser(ctx, t, pool, 1, "agent", "csr")

	store := database.Tickets()
	created, err := store.CreateTicketAdmin(ctx, 1, "connectivity", "down", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}
	// Backdate creation so resolution durations are measurable in hours.
	if _, err := pool.Exec(ctx,
		`UPDATE tickets SET created_at = NOW() - INTERVAL '10 hours' WHERE id = $1`, created.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	agentCtx := middleware.WithSubject(ctx, "csr:agent")
	resolved, reopened := "resolved", "open"
	if _, err := store.UpdateTicketAdmin(agentCtx, created.ID, &resolved, nil, nil); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// Backdate the first resolution to 2 hours after creation.
	if _, err := pool.Exec(ctx, `
		UPDATE ticket_status_history SET occurred_at = (SELECT created_at FROM tickets WHERE id = $1) + INTERVAL '2 hours'
		 WHERE ticket_id = $1 AND new_status = 'resolved'`, created.ID); err != nil {
		t.Fatalf("backdate resolution: %v", err)
	}
	if _, err := store.UpdateTicketAdmin(agentCtx, created.ID, &reopened, nil, nil); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := store.UpdateTicketAdmin(agentCtx, created.ID, &resolved, nil, nil); err != nil {
		t.Fatalf("re-resolve: %v", err)
	}

	refreshTicketView(ctx, t, pool)
	rows, err := database.Reporting().TicketResolution(ctx, 1, nil)
	if err != nil {
		t.Fatalf("TicketResolution: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one group, got %d", len(rows))
	}
	got := rows[0]
	if got.Raised != 1 || got.Resolved != 1 {
		t.Errorf("raised/resolved: want 1/1, got %d/%d", got.Raised, got.Resolved)
	}
	if got.Reopens != 1 {
		t.Errorf("reopens: want 1 — a reopen is a support failure and must be visible, got %d", got.Reopens)
	}
	if got.MedianResolutionHours == nil {
		t.Fatal("median must be present when a ticket resolved")
	}
	// ~2h from the first resolution, not ~10h from the second.
	if *got.MedianResolutionHours > 3 {
		t.Errorf("median_resolution_hours: want ~2 (first resolution), got %.2f — "+
			"the last resolution is being used, which hides the reopen", *got.MedianResolutionHours)
	}
}

// TestFR_RPT_001_UnresolvedTicketsStillCount guards the LEFT JOIN. An inner
// join would drop exactly the tickets a resolution report exists to surface,
// and the numbers would improve the worse things actually got.
func TestFR_RPT_001_UnresolvedTicketsStillCount(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "t@isp"})
	seedStaffUser(ctx, t, pool, 1, "agent", "csr")

	for i := 0; i < 3; i++ {
		if _, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "still down", nil); err != nil {
			t.Fatalf("create ticket %d: %v", i, err)
		}
	}

	refreshTicketView(ctx, t, pool)
	rows, err := database.Reporting().TicketResolution(ctx, 1, nil)
	if err != nil {
		t.Fatalf("TicketResolution: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one group, got %d", len(rows))
	}
	if rows[0].Raised != 3 {
		t.Errorf("raised: want 3 unresolved tickets counted, got %d", rows[0].Raised)
	}
	if rows[0].Resolved != 0 {
		t.Errorf("resolved: want 0, got %d", rows[0].Resolved)
	}
	if rows[0].MedianResolutionHours != nil {
		t.Errorf("median must be NULL when nothing resolved, got %.2f — reporting 0.0 would "+
			"claim the fastest possible support for a month in which nobody was helped",
			*rows[0].MedianResolutionHours)
	}
}

// TestFR_RPT_003_CollectionRateIsNullWithoutBilling covers the decision to
// report NULL rather than 0%: a franchise that raised no invoices has no
// collection rate, and 0% would rank a new territory bottom of a league table
// it has not joined.
func TestFR_RPT_003_CollectionRateIsNullWithoutBilling(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 1, "Madurai LCO", "10.00", "active")
	seedFranchise(ctx, t, pool, 2, "New Territory", "10.00", "active")
	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "1000.00")
	f1 := 1
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "m@isp", FranchiseID: &f1})
	seedGstRate(ctx, t, pool, 1)

	// Franchise 1 billed 1000 and collected 800; franchise 2 did neither.
	if _, err := pool.Exec(ctx, `
		INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
		                      total_amount, gst_rate_id, gb_included, gb_used)
		VALUES (1, 1000.00, 0, 0, 0, 1000.00, 1, 100, 0)`); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO lco_ledger (franchise_id, subscriber_id, recharge_amount, commission_amount, transaction_ref)
		VALUES (1, 1, 800.00, 80.00, 'TXN-1')`); err != nil {
		t.Fatalf("seed lco_ledger: %v", err)
	}

	rows, err := database.Reporting().FranchiseCollection(ctx, 1, nil)
	if err != nil {
		t.Fatalf("FranchiseCollection: %v", err)
	}

	var billed, unbilled bool
	for _, r := range rows {
		switch r.FranchiseID {
		case 1:
			billed = true
			assertDecimalEqual(t, "billed", r.Billed, "1000.00")
			assertDecimalEqual(t, "collected", r.Collected, "800.00")
			assertDecimalEqual(t, "commission", r.Commission, "80.00")
			if r.CollectionRatePct == nil || *r.CollectionRatePct != 80 {
				t.Errorf("collection_rate_pct: want 80, got %v", r.CollectionRatePct)
			}
		case 2:
			unbilled = true
			if r.CollectionRatePct != nil {
				t.Errorf("a franchise with nothing billed has no collection rate; got %.2f%% — "+
					"that ranks a new territory bottom of a table it has not joined", *r.CollectionRatePct)
			}
		}
	}
	if !billed || !unbilled {
		t.Fatalf("both franchises must appear (billed=%v unbilled=%v)", billed, unbilled)
	}
}

// TestFR_RPT_001_ConcurrentRefreshDoesNotBlockReaders is the reason the unique
// index exists. Without it REFRESH takes an ACCESS EXCLUSIVE lock and every
// dashboard reading the view blocks for the duration — which surfaces as the
// reporting page hanging exactly when somebody is looking at it.
func TestFR_RPT_001_ConcurrentRefreshDoesNotBlockReaders(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "t@isp"})
	seedStaffUser(ctx, t, pool, 1, "agent", "csr")
	if _, err := database.Tickets().CreateTicketAdmin(ctx, 1, "connectivity", "down", nil); err != nil {
		t.Fatalf("create ticket: %v", err)
	}

	// The store's own refresh path must succeed, which it only can if the
	// unique index CONCURRENTLY requires is present.
	if err := database.Reporting().RefreshTicketResolution(ctx); err != nil {
		t.Fatalf("RefreshTicketResolution: %v — CONCURRENTLY needs the unique index "+
			"migration 032 creates", err)
	}

	// A read taken while a refresh runs must return, not block.
	done := make(chan error, 1)
	go func() {
		done <- database.Reporting().RefreshTicketResolution(ctx)
	}()
	readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := database.Reporting().TicketResolution(readCtx, 1, nil); err != nil {
		t.Fatalf("read during refresh blocked or failed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("concurrent refresh: %v", err)
	}
}
