//go:build integration

// Status-capture tests — FR-RPT-001 | migration 031 | MDS §4.8.
//
// These exist because churn and resolution reporting are only as trustworthy
// as the capture underneath them. A view that silently reads an incomplete
// history produces numbers nobody can tell are wrong, so the properties
// asserted here are about completeness and attribution rather than about any
// particular report.
package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// historyRow is one captured transition.
type historyRow struct {
	OldStatus  *string
	NewStatus  string
	Reason     *string
	ChangedBy  string
	IsBaseline bool
	PlanID     *int
}

func subscriberHistory(ctx context.Context, t *testing.T, pool *pgxpool.Pool, subscriberID int) []historyRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT old_status, new_status, reason, changed_by, is_baseline, plan_id
		  FROM subscriber_status_history
		 WHERE subscriber_id = $1
		 ORDER BY id`, subscriberID)
	if err != nil {
		t.Fatalf("query subscriber history: %v", err)
	}
	defer rows.Close()

	var out []historyRow
	for rows.Next() {
		var r historyRow
		if err := rows.Scan(&r.OldStatus, &r.NewStatus, &r.Reason, &r.ChangedBy, &r.IsBaseline, &r.PlanID); err != nil {
			t.Fatalf("scan history: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func ticketHistory(ctx context.Context, t *testing.T, pool *pgxpool.Pool, ticketID int) []historyRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT old_status, new_status, changed_by, is_baseline
		  FROM ticket_status_history
		 WHERE ticket_id = $1
		 ORDER BY id`, ticketID)
	if err != nil {
		t.Fatalf("query ticket history: %v", err)
	}
	defer rows.Close()

	var out []historyRow
	for rows.Next() {
		var r historyRow
		if err := rows.Scan(&r.OldStatus, &r.NewStatus, &r.ChangedBy, &r.IsBaseline); err != nil {
			t.Fatalf("scan ticket history: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestFR_RPT_001_CreationIsCapturedAsAnEvent covers the INSERT trigger: a new
// connection is a growth event and has to be recorded as one.
func TestFR_RPT_001_CreationIsCapturedAsAnEvent(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "new@isp"})

	history := subscriberHistory(ctx, t, pool, 1)
	if len(history) != 1 {
		t.Fatalf("want exactly one row for a newly created subscriber, got %d", len(history))
	}
	if history[0].OldStatus != nil {
		t.Errorf("a creation has no previous status, got %q", *history[0].OldStatus)
	}
	if history[0].NewStatus != "active" {
		t.Errorf("new_status: want active, got %q", history[0].NewStatus)
	}
	// The distinction the growth view depends on: a real creation is an
	// event, the migration's seeded snapshot is not.
	if history[0].IsBaseline {
		t.Error("a subscriber created after migration 031 is a real event, not a baseline snapshot")
	}
	if history[0].PlanID == nil || *history[0].PlanID != 1 {
		t.Error("plan_id must be captured on the row: churn analysis asks which plan was left, " +
			"and the subscriber's current plan is a different question")
	}
}

// TestFR_RPT_001_CreationIsAttributed covers a gap live verification found:
// capture was wired into the status *update* paths but not the *create* ones,
// so the first row of every subscriber's and every ticket's history read
// "unknown" — the one entry an audit of "who opened this account" needs most.
func TestFR_RPT_001_CreationIsAttributed(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedStaffUser(ctx, t, pool, 1, "agent", "csr")

	opCtx := middleware.WithSubject(ctx, "csr:arun")
	created, err := database.API().CreateSubscriber(opCtx, api.SubscriberRecord{
		CAFNumber:       "CAF-attr",
		Username:        "attr@isp",
		MobileNumber:    "+919876500011",
		PlanID:          1,
		RegisteredState: "TN",
	}, "$2a$12$seedhash")
	if err != nil {
		t.Fatalf("CreateSubscriber: %v", err)
	}

	subHistory := subscriberHistory(ctx, t, pool, created.ID)
	if len(subHistory) != 1 {
		t.Fatalf("want one creation row, got %d", len(subHistory))
	}
	if subHistory[0].ChangedBy != "csr:arun" {
		t.Errorf("subscriber creation changed_by: want csr:arun, got %q", subHistory[0].ChangedBy)
	}
	if subHistory[0].Reason == nil || *subHistory[0].Reason != "signup" {
		t.Error("a direct signup must be distinguishable from a lead conversion")
	}

	ticket, err := database.Tickets().CreateTicketAdmin(opCtx, created.ID, "connectivity", "no link", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}
	tktHistory := ticketHistory(ctx, t, pool, ticket.ID)
	if len(tktHistory) != 1 {
		t.Fatalf("want one ticket creation row, got %d", len(tktHistory))
	}
	if tktHistory[0].ChangedBy != "csr:arun" {
		t.Errorf("ticket creation changed_by: want csr:arun, got %q", tktHistory[0].ChangedBy)
	}
}

// TestFR_RPT_001_TransitionCapturedWithActor is the main path: an operator
// suspends an account and both the transition and who did it are recorded.
func TestFR_RPT_001_TransitionCapturedWithActor(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "susp@isp"})

	opCtx := middleware.WithSubject(ctx, "csr:meena")
	suspended := "soft_suspended"
	if _, err := database.API().UpdateSubscriber(opCtx, 1, nil, &suspended, nil); err != nil {
		t.Fatalf("UpdateSubscriber: %v", err)
	}

	history := subscriberHistory(ctx, t, pool, 1)
	if len(history) != 2 {
		t.Fatalf("want creation + transition, got %d rows", len(history))
	}
	got := history[1]
	if got.OldStatus == nil || *got.OldStatus != "active" {
		t.Error("the transition must record what the status was before it")
	}
	if got.NewStatus != "soft_suspended" {
		t.Errorf("new_status: want soft_suspended, got %q", got.NewStatus)
	}
	if got.ChangedBy != "csr:meena" {
		t.Errorf("changed_by: want the acting operator, got %q", got.ChangedBy)
	}
	if got.Reason == nil || *got.Reason != "operator" {
		t.Error("reason must distinguish an operator edit from an automatic one")
	}
}

// TestFR_RPT_001_UnattributedChangeIsStillCaptured is the property that makes
// trigger-based capture worth the complexity over application-level writes.
//
// A write path that never learned to set the actor — a future endpoint, a
// migration, a DBA at a psql prompt — must still produce a history row. The
// attribution degrades to 'unknown'; the event itself is never lost, because
// a missing event cannot be reconstructed later and a missing name can.
func TestFR_RPT_001_UnattributedChangeIsStillCaptured(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "raw@isp"})

	// Deliberately bypassing every application code path.
	if _, err := pool.Exec(ctx,
		`UPDATE subscribers SET status = 'terminated' WHERE id = 1`); err != nil {
		t.Fatalf("raw update: %v", err)
	}

	history := subscriberHistory(ctx, t, pool, 1)
	if len(history) != 2 {
		t.Fatalf("a status change made outside the application must still be captured; got %d rows", len(history))
	}
	if history[1].NewStatus != "terminated" {
		t.Errorf("new_status: want terminated, got %q", history[1].NewStatus)
	}
	if history[1].ChangedBy != "unknown" {
		t.Errorf("changed_by: want unknown for an unattributed write, got %q", history[1].ChangedBy)
	}
}

// TestFR_RPT_001_NonStatusWritesCaptureNothing guards both correctness and
// cost. A wallet top-up is not a lifecycle event, and every spurious row is
// one a churn count has to be taught to ignore.
func TestFR_RPT_001_NonStatusWritesCaptureNothing(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "quiet@isp"})

	for _, q := range []string{
		`UPDATE subscribers SET wallet_balance = 500.00 WHERE id = 1`,
		`UPDATE subscribers SET fup_active = TRUE WHERE id = 1`,
		// A save that rewrites the same status is not a transition.
		`UPDATE subscribers SET status = 'active' WHERE id = 1`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	if history := subscriberHistory(ctx, t, pool, 1); len(history) != 1 {
		t.Fatalf("only the creation row should exist; got %d — a no-op save is inflating every count", len(history))
	}
}

// TestFR_RPT_001_DunningSuspensionIsAttributedToTheScanner keeps automatic
// suspensions separable from operator decisions, which is the difference
// between "collections is working" and "staff are suspending people".
func TestFR_RPT_001_DunningSuspensionIsAttributedToTheScanner(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "dunned@isp"})

	scannerCtx := middleware.WithSubject(ctx, "system:dunning-scanner")
	err := database.Billing().SetSubscriberDunningState(
		scannerCtx, 1, billing.DunningState("hard_suspended"), "hard_suspended")
	if err != nil {
		t.Fatalf("SetSubscriberDunningState: %v", err)
	}

	history := subscriberHistory(ctx, t, pool, 1)
	if len(history) != 2 {
		t.Fatalf("want creation + suspension, got %d rows", len(history))
	}
	if history[1].ChangedBy != "system:dunning-scanner" {
		t.Errorf("changed_by: want system:dunning-scanner, got %q", history[1].ChangedBy)
	}
	if history[1].Reason == nil || *history[1].Reason != "dunning" {
		t.Error("reason must mark this as a dunning action, not an operator decision")
	}
}

// TestFR_RPT_001_TicketTransitionsCaptureEveryStep covers the reopen case
// specifically: resolution time is the FIRST arrival at resolved, and taking
// the last one would report a reopened ticket as a single slow success.
func TestFR_RPT_001_TicketTransitionsCaptureEveryStep(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "tkt@isp"})
	seedStaffUser(ctx, t, pool, 1, "agent", "csr")

	store := database.Tickets()
	created, err := store.CreateTicketAdmin(ctx, 1, "connectivity", "no link", nil)
	if err != nil {
		t.Fatalf("CreateTicketAdmin: %v", err)
	}

	agentCtx := middleware.WithSubject(ctx, "csr:agent")
	for _, status := range []string{"in_progress", "resolved", "open", "resolved"} {
		s := status
		if _, err := store.UpdateTicketAdmin(agentCtx, created.ID, &s, nil, nil); err != nil {
			t.Fatalf("UpdateTicketAdmin(%s): %v", status, err)
		}
	}

	history := ticketHistory(ctx, t, pool, created.ID)
	// creation + four transitions
	if len(history) != 5 {
		t.Fatalf("want creation plus four transitions, got %d", len(history))
	}
	if history[0].OldStatus != nil || history[0].NewStatus != "open" {
		t.Error("the creation row must record the opening status with no predecessor")
	}
	if history[3].NewStatus != "open" || history[3].OldStatus == nil || *history[3].OldStatus != "resolved" {
		t.Error("the reopen must be visible as resolved -> open, or reopen rate is not derivable")
	}
	for _, r := range history[1:] {
		if r.ChangedBy != "csr:agent" {
			t.Errorf("changed_by: want csr:agent, got %q", r.ChangedBy)
		}
	}

	// The query every resolution metric is built on.
	var firstResolved, lastResolved *string
	if err := pool.QueryRow(ctx, `
		SELECT min(occurred_at)::text, max(occurred_at)::text
		  FROM ticket_status_history
		 WHERE ticket_id = $1 AND new_status = 'resolved'`, created.ID).
		Scan(&firstResolved, &lastResolved); err != nil {
		t.Fatalf("resolution query: %v", err)
	}
	if firstResolved == nil || lastResolved == nil {
		t.Fatal("both resolutions must be present")
	}
	if *firstResolved == *lastResolved {
		t.Error("the two resolutions must be distinguishable, or a reopen is invisible to reporting")
	}
}

// TestFR_RPT_001_BaselineIsNotAnEvent covers the seeding decision: accounts
// that predate the migration get a starting position, not an invented signup.
func TestFR_RPT_001_BaselineIsNotAnEvent(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "existing@isp"})

	// Simulate the pre-migration world: the account exists with no history.
	if _, err := pool.Exec(ctx, `DELETE FROM subscriber_status_history WHERE subscriber_id = 1`); err != nil {
		t.Fatalf("clear history: %v", err)
	}
	// Re-run migration 031's seeding statement.
	if _, err := pool.Exec(ctx, `
		INSERT INTO subscriber_status_history (
			subscriber_id, old_status, new_status, reason, changed_by,
			plan_id, franchise_id, is_baseline, occurred_at)
		SELECT s.id, NULL, s.status, 'baseline', 'system:migration-031',
		       s.plan_id, s.franchise_id, TRUE, s.created_at
		  FROM subscribers s
		 WHERE s.status <> 'terminated'
		   AND NOT EXISTS (SELECT 1 FROM subscriber_status_history h WHERE h.subscriber_id = s.id)`); err != nil {
		t.Fatalf("seed baseline: %v", err)
	}

	history := subscriberHistory(ctx, t, pool, 1)
	if len(history) != 1 {
		t.Fatalf("want one baseline row, got %d", len(history))
	}
	if !history[0].IsBaseline {
		t.Error("the seeded row must be flagged is_baseline, or reporting counts it as a signup " +
			"that never happened")
	}

	// The constraint that keeps a baseline from ever masquerading as a
	// transition, whatever future code inserts one.
	_, err := pool.Exec(ctx, `
		INSERT INTO subscriber_status_history (subscriber_id, old_status, new_status, is_baseline)
		VALUES (1, 'active', 'terminated', TRUE)`)
	if err == nil {
		t.Error("a baseline row with a predecessor status must be rejected: it is a transition " +
			"claiming to be a snapshot")
	}
}
