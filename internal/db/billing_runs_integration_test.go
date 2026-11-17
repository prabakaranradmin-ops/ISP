//go:build integration

// Recurring-billing run history — FR-BIL-020 | migration 057.
//
// Driven through the real scanner over the real store, so what the run
// row says is checked against what actually happened to the wallets and
// the invoices table, not against what the scanner believed it did.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

func TestFR_BIL_020_BillingRunHistory(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedGstRate(ctx, t, pool, 1)
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "100.00")

	expired := time.Now().AddDate(0, 0, -1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "due_and_funded", Balance: "500.00", PlanExpiry: &expired, MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "new_and_funded", Status: "pending_payment", Balance: "500.00", MobileNumber: "+919800000002"})

	scan := func() {
		t.Helper()
		sc := billing.NewRecurringBillingScanner(store, billing.NewWalletService(store))
		sc.SetRunRecorder(store)
		if err := sc.Scan(ctx); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	latest := func() *billing.BillingRun {
		t.Helper()
		var id int
		if err := pool.QueryRow(ctx, `SELECT max(id) FROM billing_runs`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		r, err := store.GetBillingRun(ctx, id)
		if err != nil || r == nil {
			t.Fatalf("get run %d: %v", id, err)
		}
		return r
	}

	t.Run("a pass that renews records what it did, and it matches the books", func(t *testing.T) {
		scan()
		r := latest()
		if r.Status != billing.RunCompleted || r.FinishedAt == nil || r.Kind != billing.RunKindRecurringBilling {
			t.Fatalf("run: %+v", r)
		}
		if r.Candidates != 2 || r.Renewed != 1 || r.Activated != 1 || r.Failed != 0 || r.InvoicesRaised != 2 {
			t.Errorf("counts: %+v", r)
		}
		// The recorded charge is what the wallets actually lost: 2 x 118.00.
		debited := scanString(ctx, t, pool, `SELECT sum(amount)::text FROM wallet_ledgers
			WHERE account = 'subscriber_wallet' AND entry_type = 'debit'`)
		if r.AmountCharged.StringFixed(2) != "236.00" || debited != "236.00" {
			t.Errorf("recorded %s, wallets debited %s", r.AmountCharged, debited)
		}
		if n := scanString(ctx, t, pool, `SELECT count(*)::text FROM invoices`); n != "2" {
			t.Errorf("invoices written: %s", n)
		}
	})

	t.Run("idle passes are recorded but left out of the default list", func(t *testing.T) {
		scan() // both now paid up: nobody to renew
		idle := latest()
		if idle.Candidates != 0 || idle.Status != billing.RunCompleted {
			t.Fatalf("idle run: %+v", idle)
		}
		def, err := store.ListBillingRuns(ctx, billing.BillingRunFilter{})
		if err != nil {
			t.Fatal(err)
		}
		all, _ := store.ListBillingRuns(ctx, billing.BillingRunFilter{IncludeIdle: true})
		if len(def) != 1 || len(all) != 2 || all[0].ID != idle.ID {
			t.Errorf("default %d runs, all %d (newest %d, want %d)", len(def), len(all), all[0].ID, idle.ID)
		}
	})

	t.Run("a subscriber the pass could not renew is named with the step", func(t *testing.T) {
		expiredToo := time.Now().AddDate(0, 0, -2)
		seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "due_no_gst_rate", Balance: "500.00", PlanExpiry: &expiredToo, MobileNumber: "+919800000003"})
		// No rate in force: the renewal stops before charging anything.
		if _, err := pool.Exec(ctx, `UPDATE gst_rates SET effective_from = now() + interval '1 day'`); err != nil {
			t.Fatal(err)
		}
		scan()
		r := latest()
		if r.Failed != 1 || len(r.Failures) != 1 || !r.AmountCharged.IsZero() {
			t.Fatalf("run: %+v", r)
		}
		f := r.Failures[0]
		if f.SubscriberID != 3 || f.Username != "due_no_gst_rate" || f.Stage != billing.StagePrice || f.Charged {
			t.Errorf("failure: %+v", f)
		}
		if bal := scanString(ctx, t, pool, `SELECT wallet_balance::text FROM subscribers WHERE id = 3`); bal != "500.00" {
			t.Errorf("nothing may be charged: %s", bal)
		}
		problems, _ := store.ListBillingRuns(ctx, billing.BillingRunFilter{ProblemsOnly: true, IncludeIdle: true})
		if len(problems) != 1 || problems[0].ID != r.ID {
			t.Errorf("problems-only list: %+v", problems)
		}
	})

	t.Run("a run left running by a stopped process is closed as abandoned", func(t *testing.T) {
		var stale int
		if err := pool.QueryRow(ctx, `INSERT INTO billing_runs (kind, started_at) VALUES ($1, now() - interval '2 hours') RETURNING id`,
			billing.RunKindRecurringBilling).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		var recent int
		if err := pool.QueryRow(ctx, `INSERT INTO billing_runs (kind, started_at) VALUES ($1, now() - interval '5 minutes') RETURNING id`,
			billing.RunKindRecurringBilling).Scan(&recent); err != nil {
			t.Fatal(err)
		}
		if _, err := store.StartRun(ctx, billing.RunKindRecurringBilling); err != nil {
			t.Fatal(err)
		}
		if st := scanString(ctx, t, pool, `SELECT status FROM billing_runs WHERE id = $1`, stale); st != billing.RunAbandoned {
			t.Errorf("two hours old: %s", st)
		}
		// Another process may genuinely be mid-pass.
		if st := scanString(ctx, t, pool, `SELECT status FROM billing_runs WHERE id = $1`, recent); st != billing.RunRunning {
			t.Errorf("five minutes old: %s", st)
		}
	})

	t.Run("the table refuses a record that cannot be true", func(t *testing.T) {
		id, err := store.StartRun(ctx, billing.RunKindRecurringBilling)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.FinishRun(ctx, id, billing.RunFailed, billing.RunSummary{AmountCharged: decimal.Zero}); err == nil {
			t.Error("a failed run with no error was accepted")
		}
		if err := store.FinishRun(ctx, id, billing.RunCompleted, billing.RunSummary{Candidates: 1, Renewed: 2, AmountCharged: decimal.Zero}); err == nil {
			t.Error("more renewals than candidates was accepted")
		}
		// Two failures: one before the charge, one after. The one after is
		// the customer who paid and did not get everything, so it leads.
		if err := store.FinishRun(ctx, id, billing.RunCompleted, billing.RunSummary{
			Candidates: 2, Failed: 2, AmountCharged: decimal.RequireFromString("118"),
			Failures: []billing.RunFailure{
				{SubscriberID: 1, Username: "due_and_funded", Stage: billing.StagePrice, Message: "no rate", Charged: false},
				{SubscriberID: 999, Username: "since_deleted", Stage: billing.StageExpiry, Message: "db down", Charged: true},
			},
		}); err != nil {
			t.Fatalf("a true record was refused: %v", err)
		}
		got, err := store.GetBillingRun(ctx, id)
		if err != nil || len(got.Failures) != 2 {
			t.Fatalf("run %d: %+v %v", id, got, err)
		}
		if !got.Failures[0].Charged || got.Failures[0].Stage != billing.StageExpiry {
			t.Errorf("charged failures must lead: %+v", got.Failures)
		}
		// A subscriber id that matches nobody is kept as a name, not refused.
		if got.Failures[0].SubscriberID != 0 || got.Failures[0].Username != "since_deleted" {
			t.Errorf("unknown subscriber: %+v", got.Failures[0])
		}
		if err := store.FinishRun(ctx, id, billing.RunCompleted, billing.RunSummary{AmountCharged: decimal.Zero}); err == nil {
			t.Error("a run cannot be finished twice")
		}
	})
}
