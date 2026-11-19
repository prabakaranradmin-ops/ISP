//go:build integration

// Scheduled plan changes — FR-BIL-023 | migration 059.
//
// The properties only a real database shows: exactly one pending change per
// subscriber, the move and the new dates landing in one transaction, a
// change that two overlapping renewal passes cannot both apply, and the
// widened run-failure stage vocabulary actually accepting the new stage.
package db_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/shopspring/decimal"
)

// planChangeFixture seeds two plans and one subscriber on the first.
func planChangeFixture(ctx context.Context, t *testing.T, database *db.DB, pool *pgxpool.Pool) *db.BillingStore {
	t.Helper()
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "500.00")
	seedPlan(ctx, t, pool, 2, "Fibre 100", "100M/100M", 0, "", "1200.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "mover", Balance: "0.00", MobileNumber: "+919800000001"})
	return database.Billing()
}

func futureDay(n int) time.Time {
	return billing.DayOf(time.Now().UTC().AddDate(0, 0, n))
}

func TestFR_BIL_023_ScheduleLifecycle(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := planChangeFixture(ctx, t, database, pool)

	c, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(20), Reason: "Customer asked",
	}, "meena")
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if c.Status != billing.PlanChangeScheduled {
		t.Errorf("status %q", c.Status)
	}
	// from_plan_id is read from the subscriber, not supplied.
	if c.FromPlanID != 1 || c.FromPlanName != "Basic" {
		t.Errorf("from plan %d/%s, want 1/Basic", c.FromPlanID, c.FromPlanName)
	}
	// The new plan's price and validity come along for the renewal path.
	if !c.NewPlanPrice.Equal(decimal.RequireFromString("1200.00")) {
		t.Errorf("new plan price %s", c.NewPlanPrice)
	}
	if c.Summary() != "Basic → Fibre 100" {
		t.Errorf("summary %q", c.Summary())
	}

	t.Run("only one pending change per subscriber", func(t *testing.T) {
		_, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
			SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(25),
		}, "meena")
		if !errors.Is(err, billing.ErrPlanChangeAlreadyScheduled) {
			t.Fatalf("want ErrPlanChangeAlreadyScheduled, got %v", err)
		}
	})

	t.Run("the plan they are already on is refused", func(t *testing.T) {
		_, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
			SubscriberID: 1, NewPlanID: 1, EffectiveOn: futureDay(20),
		}, "meena")
		if !errors.Is(err, billing.ErrPlanChangeSamePlan) {
			t.Fatalf("want ErrPlanChangeSamePlan, got %v", err)
		}
	})

	t.Run("an unknown plan is a validation failure, not a crash", func(t *testing.T) {
		// Cancel the pending one first so the uniqueness rule is not what
		// refuses this.
		if err := store.CancelPlanChange(ctx, c.ID, "making room for the next check", "meena"); err != nil {
			t.Fatal(err)
		}
		_, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
			SubscriberID: 1, NewPlanID: 999999, EffectiveOn: futureDay(20),
		}, "meena")
		if !errors.Is(err, billing.ErrPlanChangeInvalid) {
			t.Fatalf("want ErrPlanChangeInvalid, got %v", err)
		}
	})

	t.Run("an unknown subscriber is not-found", func(t *testing.T) {
		_, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
			SubscriberID: 999999, NewPlanID: 2, EffectiveOn: futureDay(20),
		}, "meena")
		if !errors.Is(err, db.ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	})

	t.Run("a cancelled change keeps its reason and frees the slot", func(t *testing.T) {
		got, err := store.GetPlanChange(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != billing.PlanChangeCancelled || got.CancelReason == "" || got.CancelledBy != "meena" {
			t.Errorf("cancelled change reads %+v", got)
		}
		// The slot is free again.
		again, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
			SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(20),
		}, "ravi")
		if err != nil {
			t.Fatalf("scheduling after a cancellation: %v", err)
		}
		if again.ID == c.ID {
			t.Error("the cancelled row was reused")
		}
	})

	t.Run("cancelling needs a reason, and twice is refused", func(t *testing.T) {
		pending, err := store.ListPlanChanges(ctx, billing.PlanChangeFilter{
			SubscriberID: 1, Status: billing.PlanChangeScheduled})
		if err != nil || len(pending) != 1 {
			t.Fatalf("pending: %v %d", err, len(pending))
		}
		id := pending[0].ID
		if err := store.CancelPlanChange(ctx, id, "  ", "meena"); !errors.Is(err, billing.ErrPlanChangeInvalid) {
			t.Fatalf("blank reason: want ErrPlanChangeInvalid, got %v", err)
		}
		if err := store.CancelPlanChange(ctx, id, "done", "meena"); err != nil {
			t.Fatal(err)
		}
		if err := store.CancelPlanChange(ctx, id, "again", "meena"); !errors.Is(err, billing.ErrPlanChangeNotPending) {
			t.Fatalf("want ErrPlanChangeNotPending, got %v", err)
		}
	})

	t.Run("cancelling one that does not exist is not-found", func(t *testing.T) {
		if err := store.CancelPlanChange(ctx, 999999, "x", "meena"); !errors.Is(err, billing.ErrPlanChangeNotFound) {
			t.Fatalf("want ErrPlanChangeNotFound, got %v", err)
		}
	})
}

// GetDuePlanChange is what the renewal path asks on every pass, so its date
// boundary is the thing to get right.
func TestFR_BIL_023_OnlyDueChangesAreReturned(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := planChangeFixture(ctx, t, database, pool)

	c, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(10),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("not due before its date", func(t *testing.T) {
		got, err := store.GetDuePlanChange(ctx, 1, futureDay(9))
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("a change dated %v came back due on %v", c.EffectiveOn, futureDay(9))
		}
	})

	t.Run("due on its date", func(t *testing.T) {
		got, err := store.GetDuePlanChange(ctx, 1, futureDay(10))
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatal("not due on its own date")
		}
		if got.ID != c.ID || got.NewPlanID != 2 {
			t.Errorf("got %+v", got)
		}
		if got.NewPlanValidityDays == 0 {
			t.Error("the new plan's validity did not come along; the cycle could not be dated")
		}
	})

	t.Run("still due later — a pass that runs late must not skip it", func(t *testing.T) {
		got, err := store.GetDuePlanChange(ctx, 1, futureDay(45))
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Error("a change whose date has passed is no longer offered")
		}
	})

	t.Run("a cancelled change is never due", func(t *testing.T) {
		if err := store.CancelPlanChange(ctx, c.ID, "withdrawn", "meena"); err != nil {
			t.Fatal(err)
		}
		got, err := store.GetDuePlanChange(ctx, 1, futureDay(45))
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Error("a cancelled change was offered as due")
		}
	})

	t.Run("a subscriber with nothing scheduled gets nothing", func(t *testing.T) {
		got, err := store.GetDuePlanChange(ctx, 1, futureDay(100))
		if err != nil {
			t.Fatal(err)
		}
		if got != nil {
			t.Errorf("got %+v", got)
		}
	})
}

// Applying moves the plan, stamps the expiry and closes the change
// together, and can only happen once.
func TestFR_BIL_023_ApplyMovesThePlanAndClosesTheChangeAtomically(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := planChangeFixture(ctx, t, database, pool)

	c, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(0),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}

	newExpiry := time.Now().UTC().AddDate(0, 0, 60)
	if err := store.ApplyDuePlanChange(ctx, c.ID, 1, 2, newExpiry, false, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}

	gotPlan := scanString(ctx, t, pool, `SELECT plan_id::text FROM subscribers WHERE id = 1`)
	if gotPlan != "2" {
		t.Errorf("subscriber is on plan %s, want 2", gotPlan)
	}
	applied, err := store.GetPlanChange(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != billing.PlanChangeApplied || applied.AppliedAt == nil {
		t.Errorf("change reads %+v", applied)
	}

	t.Run("applying twice is refused", func(t *testing.T) {
		err := store.ApplyDuePlanChange(ctx, c.ID, 1, 2, newExpiry, false, nil)
		if !errors.Is(err, billing.ErrPlanChangeNotPending) {
			t.Fatalf("want ErrPlanChangeNotPending, got %v", err)
		}
	})

	t.Run("the invoice link is written afterwards", func(t *testing.T) {
		var invoiceID int
		if err := pool.QueryRow(ctx, `
			INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
			                      total_amount, gst_rate_id, gb_included, gb_used)
			VALUES (1, 1200.00, 108.00, 108.00, 0, 1416.00, 1, 0, 0) RETURNING id`).Scan(&invoiceID); err != nil {
			t.Fatal(err)
		}
		if err := store.SetPlanChangeInvoice(ctx, c.ID, invoiceID); err != nil {
			t.Fatalf("link invoice: %v", err)
		}
		linked, err := store.GetPlanChange(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if linked.AppliedInvoiceID == nil || *linked.AppliedInvoiceID != invoiceID {
			t.Errorf("invoice link is %v", linked.AppliedInvoiceID)
		}
		if linked.AppliedInvoiceNumber() == "" {
			t.Error("the applied invoice number is empty")
		}
	})
}

// A first cycle moves plan, expiry and status together — the window
// migration 048 exists to close.
func TestFR_BIL_023_ApplyActivatesAFirstCycleInTheSameStatement(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "500.00")
	seedPlan(ctx, t, pool, 2, "Fibre 100", "100M/100M", 0, "", "1200.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "newbie", Balance: "0.00",
		Status: "pending_payment", MobileNumber: "+919800000002"})
	store := database.Billing()

	c, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(0),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().AddDate(0, 0, 60)
	if err := store.ApplyDuePlanChange(ctx, c.ID, 1, 2, expiry, true, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got := scanString(ctx, t, pool,
		`SELECT plan_id::text || ' ' || status || ' ' || (plan_expiry IS NOT NULL)::text
		   FROM subscribers WHERE id = 1`)
	if got != "2 active true" {
		t.Errorf("subscriber reads %q, want \"2 active true\"", got)
	}
}

// Two overlapping renewal passes must not both apply one change.
func TestFR_BIL_023_ChangeIsAppliedOnceUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := planChangeFixture(ctx, t, database, pool)

	c, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(0),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().AddDate(0, 0, 60)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := store.ApplyDuePlanChange(ctx, c.ID, 1, 2, expiry, false, nil); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d of 2 concurrent passes applied the change, want exactly 1", succeeded)
	}
	if n := scanString(ctx, t, pool,
		`SELECT count(*)::text FROM scheduled_plan_changes WHERE status = 'applied'`); n != "1" {
		t.Errorf("%s changes are marked applied, want 1", n)
	}
}

// The run record has to accept the new failure stage.
//
// Migration 057 constrained stage to a fixed list, so without 059's widened
// CHECK this insert is rejected — and FR-BIL-020 deliberately logs a
// recording failure and carries on, so the one failure that most needs
// reporting (charged at the new plan's price, still on the old plan) would
// be the one the Billing Runs screen never shows. Nothing in Go catches
// that: the stage is a string either side of the wire.
func TestFR_BIL_023_RunRecordAcceptsThePlanChangeStage(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := planChangeFixture(ctx, t, database, pool)

	runID, err := store.StartRun(ctx, billing.RunKindRecurringBilling)
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	sum := billing.RunSummary{
		Candidates: 1, Failed: 1, AmountCharged: decimal.RequireFromString("1416.00"),
		Failures: []billing.RunFailure{{
			SubscriberID: 1, Username: "mover", Stage: billing.StagePlanChange,
			Message: "db down", Charged: true,
		}},
	}
	if err := store.FinishRun(ctx, runID, billing.RunCompleted, sum); err != nil {
		t.Fatalf("finish run with a plan_change failure: %v", err)
	}

	run, err := store.GetBillingRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Failures) != 1 {
		t.Fatalf("want 1 recorded failure, got %d", len(run.Failures))
	}
	if run.Failures[0].Stage != billing.StagePlanChange {
		t.Errorf("stage stored as %q", run.Failures[0].Stage)
	}
	if !run.Failures[0].Charged {
		t.Error("the failure lost its charged flag")
	}
}

// ListPlanChanges puts pending first, which is what an operator scanning
// the list needs.
func TestFR_BIL_023_ListPutsPendingFirst(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := planChangeFixture(ctx, t, database, pool)

	first, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(5),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CancelPlanChange(ctx, first.ID, "withdrawn", "meena"); err != nil {
		t.Fatal(err)
	}
	pending, err := store.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: futureDay(30),
	}, "ravi")
	if err != nil {
		t.Fatal(err)
	}

	list, err := store.ListPlanChanges(ctx, billing.PlanChangeFilter{SubscriberID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	if list[0].ID != pending.ID {
		t.Errorf("the pending change is not first: %+v", list)
	}
	if list[0].Username != "mover" {
		t.Errorf("the subscriber's name did not come along: %q", list[0].Username)
	}

	filtered, err := store.ListPlanChanges(ctx, billing.PlanChangeFilter{Status: billing.PlanChangeCancelled})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != first.ID {
		t.Errorf("status filter returned %+v", filtered)
	}
}
