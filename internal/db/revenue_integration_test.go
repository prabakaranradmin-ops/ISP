//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// TestFR_REV_001_RevenueStore_UnbilledSubscribers verifies only active subscribers with no
// invoice this period are counted.
//
// FR-REV-001 | INT-REV-001
func TestFR_REV_001_RevenueStore_UnbilledSubscribers(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)

	// 1, 2: active, no invoice     -> the seeded deficit
	// 3:    active, invoiced       -> billed
	// 4:    terminated, no invoice -> not active, so not counted
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "unbilled1@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "unbilled2@isp"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "billed@isp"})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "terminated@isp", Status: "terminated"})

	if _, err := pool.Exec(ctx, `
		INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
		                      total_amount, gst_rate_id, gb_included, gb_used, created_at)
		VALUES (3, 799.00, 71.91, 71.91, 0.00, 942.82, 1, 3300, 950.25, NOW())`); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}

	count, err := database.Revenue().GetUnbilledActiveSubscribers(ctx)
	if err != nil {
		t.Fatalf("GetUnbilledActiveSubscribers: %v", err)
	}
	if count != 2 {
		t.Errorf("want 2 unbilled active subscribers, got %d", count)
	}
}

// TestFR_REV_002_RevenueStore_LedgerVariance verifies the reconciliation detects drift
// between the ledger and the denormalised wallet balance.
//
// FR-REV-002 | INT-REV-002
func TestFR_REV_002_RevenueStore_LedgerVariance(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "clean@isp"})

	store := database.Revenue()
	billingStore := database.Billing()

	t.Run("clean books report zero variance", func(t *testing.T) {
		if _, err := billingStore.RecordRecharge(ctx, posting(1, "799.00", "799.00", "pay_clean", nil)); err != nil {
			t.Fatalf("RecordRecharge: %v", err)
		}
		variance, err := store.GetLedgerVariance(ctx)
		if err != nil {
			t.Fatalf("GetLedgerVariance: %v", err)
		}
		if !variance.IsZero() {
			t.Errorf("want zero variance on clean books, got %s", variance)
		}
	})

	t.Run("a balance edited behind the ledger's back is detected", func(t *testing.T) {
		// Simulate the failure mode the check exists for: the denormalised
		// balance moving without a matching ledger entry.
		if _, err := pool.Exec(ctx, `UPDATE subscribers SET wallet_balance = 1299.00 WHERE id = 1`); err != nil {
			t.Fatalf("tamper with balance: %v", err)
		}
		variance, err := store.GetLedgerVariance(ctx)
		if err != nil {
			t.Fatalf("GetLedgerVariance: %v", err)
		}
		assertDecimalEqual(t, "variance", variance, "500.00")
		if !variance.Abs().GreaterThan(decimal.RequireFromString("0.01")) {
			t.Error("a 500.00 drift must exceed the alert threshold")
		}
	})

	t.Run("the gateway leg does not cancel the wallet leg", func(t *testing.T) {
		// Both legs carry the same subscriber_id. If the variance query summed
		// them together every recharge would net to zero and the check could
		// never detect anything, so this guards the account filter.
		var legs int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM wallet_ledgers WHERE subscriber_id = 1`).Scan(&legs); err != nil {
			t.Fatalf("count legs: %v", err)
		}
		if legs != 2 {
			t.Fatalf("precondition: want both legs present, got %d", legs)
		}
		if _, err := pool.Exec(ctx, `UPDATE subscribers SET wallet_balance = 799.00 WHERE id = 1`); err != nil {
			t.Fatalf("restore balance: %v", err)
		}
		variance, err := store.GetLedgerVariance(ctx)
		if err != nil {
			t.Fatalf("GetLedgerVariance: %v", err)
		}
		if !variance.IsZero() {
			t.Errorf("want zero variance once the balance matches the wallet leg, got %s", variance)
		}
	})
}

// TestFR_REV_001_RevenueStore_SnapshotAndForecast verifies the nightly job's writes,
// including that a re-run replaces rather than doubles.
//
// FR-REV-001, FR-REV-004 | INT-REV-003
func TestFR_REV_001_RevenueStore_SnapshotAndForecast(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")

	// Two subscribers renew in 7 days: one funded, one short of the plan price.
	in7 := time.Now().Add(7 * 24 * time.Hour).UTC()
	in21 := time.Now().Add(21 * 24 * time.Hour).UTC()
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "funded@isp", Balance: "1000.00", PlanExpiry: &in7})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "short@isp", Balance: "10.00", PlanExpiry: &in7})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "suspended@isp", Balance: "5000.00",
		DunningState: "soft_suspended", PlanExpiry: &in21})

	store := database.Revenue()

	forecasts, err := store.BuildCollectionsForecast(ctx, 30)
	if err != nil {
		t.Fatalf("BuildCollectionsForecast: %v", err)
	}
	if len(forecasts) != 2 {
		t.Fatalf("want 2 forecast days, got %d: %+v", len(forecasts), forecasts)
	}

	var totalExpected, totalAtRisk int
	for _, f := range forecasts {
		totalExpected += f.ExpectedRenewals
		totalAtRisk += f.AtRiskRenewals
	}
	if totalExpected != 1 {
		t.Errorf("want 1 subscriber expected to renew, got %d", totalExpected)
	}
	// Short balance and suspended dunning are both at-risk conditions.
	if totalAtRisk != 2 {
		t.Errorf("want 2 at-risk subscribers, got %d", totalAtRisk)
	}

	if err := store.UpsertCollectionsForecast(ctx, forecasts); err != nil {
		t.Fatalf("UpsertCollectionsForecast: %v", err)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM collections_forecast`); n != 2 {
		t.Fatalf("want 2 persisted forecast rows, got %d", n)
	}

	t.Run("re-running replaces rather than doubles", func(t *testing.T) {
		if err := store.UpsertCollectionsForecast(ctx, forecasts); err != nil {
			t.Fatalf("second UpsertCollectionsForecast: %v", err)
		}
		if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM collections_forecast`); n != 2 {
			t.Errorf("re-run must replace: want 2 rows, got %d", n)
		}
	})

	t.Run("snapshot is written and re-runs replace", func(t *testing.T) {
		snap := revenue.RevenueSnapshot{
			SnapshotDate:            time.Now().UTC().Truncate(24 * time.Hour),
			UnbilledSubscriberCount: 3,
			LedgerVariance:          decimal.RequireFromString("0.00"),
			TotalWalletBalance:      decimal.RequireFromString("6010.00"),
		}
		if err := store.UpsertRevenueSnapshot(ctx, snap); err != nil {
			t.Fatalf("UpsertRevenueSnapshot: %v", err)
		}
		if err := store.UpsertRevenueSnapshot(ctx, snap); err != nil {
			t.Fatalf("second UpsertRevenueSnapshot: %v", err)
		}
		if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM revenue_snapshots`); n != 1 {
			t.Errorf("re-run must replace the day's snapshot: want 1 row, got %d", n)
		}
		total := scanString(ctx, t, pool, `SELECT total_wallet_balance::text FROM revenue_snapshots LIMIT 1`)
		if got := mustDecimal(t, total); !got.Equal(mustDecimal(t, "6010.00")) {
			t.Errorf("total_wallet_balance: want 6010.00, got %s", got)
		}
	})

	t.Run("total wallet balance sums exactly", func(t *testing.T) {
		total, err := store.GetTotalWalletBalance(ctx)
		if err != nil {
			t.Fatalf("GetTotalWalletBalance: %v", err)
		}
		assertDecimalEqual(t, "total wallet balance", total, "6010.00")
	})
}

// TestFR_FRN_001_RevenueStore_FranchiseIsolation verifies the subscriber list is confined
// to a franchise when one is supplied, and unrestricted when it is not.
//
// FR-FRN-001 | INT-FRN-001
func TestFR_FRN_001_RevenueStore_FranchiseIsolation(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	f1, f2 := 1, 2
	seedFranchise(ctx, t, pool, 1, "Chennai LCO", "10.00", "active")
	seedFranchise(ctx, t, pool, 2, "Madurai LCO", "8.00", "active")
	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 101, seedOpts{Username: "f1-sub@isp", FranchiseID: &f1})
	seedSubscriber(ctx, t, pool, 202, seedOpts{Username: "f2-sub@isp", FranchiseID: &f2})
	seedSubscriber(ctx, t, pool, 303, seedOpts{Username: "direct@isp"}) // no franchise

	store := database.Revenue()

	scoped, err := store.ListSubscribers(ctx, &f1)
	if err != nil {
		t.Fatalf("ListSubscribers scoped: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != 101 {
		t.Fatalf("LCO F1 must see exactly its own subscriber, got %+v", scoped)
	}

	all, err := store.ListSubscribers(ctx, nil)
	if err != nil {
		t.Fatalf("ListSubscribers unscoped: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("an unscoped list must return every subscriber, got %d", len(all))
	}
}

// TestFR_FRN_002_RevenueStore_LCOCommission verifies the commission is persisted with the
// exact decimal the calculator produced.
//
// FR-FRN-002 | INT-FRN-002
func TestFR_FRN_002_RevenueStore_LCOCommission(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 1, "Chennai LCO", "10.00", "active")
	seedFranchise(ctx, t, pool, 2, "Dormant LCO", "10.00", "suspended")
	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	franchiseID := 1
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "commission@isp", FranchiseID: &franchiseID})

	store := database.Revenue()

	franchise, err := store.GetFranchiseByID(ctx, 1)
	if err != nil {
		t.Fatalf("GetFranchiseByID: %v", err)
	}
	assertDecimalEqual(t, "commission_rate_pct", franchise.CommissionRatePct, "10.00")

	// Drive the real calculator so the stored figure is what it reported.
	commission, err := revenue.CalculateLCOCommission(ctx, store, revenue.LCOCommissionEntry{
		FranchiseID:    1,
		SubscriberID:   1,
		RechargeAmount: decimal.RequireFromString("799.00"),
		TransactionRef: "pay_commission_001",
	})
	if err != nil {
		t.Fatalf("CalculateLCOCommission: %v", err)
	}
	assertDecimalEqual(t, "commission", commission, "79.90")

	stored := scanString(ctx, t, pool, `SELECT commission_amount::text FROM lco_ledger WHERE transaction_ref = 'pay_commission_001'`)
	if got := mustDecimal(t, stored); !got.Equal(mustDecimal(t, "79.90")) {
		t.Errorf("lco_ledger.commission_amount: want 79.90, got %s", got)
	}
	recharge := scanString(ctx, t, pool, `SELECT recharge_amount::text FROM lco_ledger WHERE transaction_ref = 'pay_commission_001'`)
	if got := mustDecimal(t, recharge); !got.Equal(mustDecimal(t, "799.00")) {
		t.Errorf("lco_ledger.recharge_amount: want 799.00, got %s", got)
	}

	t.Run("a suspended franchise earns nothing and writes no row", func(t *testing.T) {
		before := countRows(ctx, t, pool, `SELECT COUNT(*) FROM lco_ledger`)
		_, err := revenue.CalculateLCOCommission(ctx, store, revenue.LCOCommissionEntry{
			FranchiseID:    2,
			SubscriberID:   1,
			RechargeAmount: decimal.RequireFromString("799.00"),
		})
		if err == nil {
			t.Error("want an error for a non-active franchise")
		}
		if after := countRows(ctx, t, pool, `SELECT COUNT(*) FROM lco_ledger`); after != before {
			t.Errorf("no ledger row may be written for a suspended franchise: %d -> %d", before, after)
		}
	})

	t.Run("unknown franchise reports an error", func(t *testing.T) {
		if _, err := store.GetFranchiseByID(ctx, 999999); err == nil {
			t.Error("want an error for an unknown franchise")
		}
	})
}

// ── FR-REV-003: collections ─────────────────────────────────────────────────

// TestFR_REV_003_CollectionsByDunningStage pins the grouping, the exclusions
// and the ordering, all of which are in SQL and none of which a stub could
// exercise.
func TestFR_REV_003_CollectionsByDunningStage(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedPlan(ctx, t, pool, 2, "Pro", "100M/100M", 0, "", "999.00")

	// Two in one stage on different plans, so the sum is not just a count
	// multiplied by a single price.
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a", PlanID: 1, DunningState: "grace_period"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b", PlanID: 2, DunningState: "grace_period"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "c", PlanID: 1, DunningState: "hard_suspended"})
	// Current: owes nothing, must not appear at all.
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "d", PlanID: 2, DunningState: "active"})
	// Terminated: no longer a collections prospect, however they left the
	// dunning ladder.
	seedSubscriber(ctx, t, pool, 5, seedOpts{
		Username: "e", PlanID: 2, DunningState: "soft_suspended", Status: "terminated"})

	rows, err := database.Revenue().GetCollectionsByDunningStage(ctx)
	if err != nil {
		t.Fatalf("GetCollectionsByDunningStage: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("want 2 stages (active and terminated excluded), got %d: %+v", len(rows), rows)
	}
	// Ladder order, not alphabetical: grace_period precedes hard_suspended.
	if rows[0].DunningState != "grace_period" || rows[1].DunningState != "hard_suspended" {
		t.Errorf("stages out of ladder order: %s then %s", rows[0].DunningState, rows[1].DunningState)
	}
	if rows[0].Subscribers != 2 {
		t.Errorf("grace_period subscribers: want 2, got %d", rows[0].Subscribers)
	}
	if got := rows[0].Outstanding.StringFixed(2); got != "1498.00" {
		t.Errorf("grace_period outstanding: want 1498.00 (499 + 999), got %s", got)
	}
	// Service state is derived, not stored: grace_period leaves the
	// subscriber online, hard_suspended does not.
	if rows[0].ServiceStopped {
		t.Error("grace_period must not be reported as service-stopped")
	}
	if !rows[1].ServiceStopped {
		t.Error("hard_suspended must be reported as service-stopped")
	}
}

// TestFR_REV_003_MonthlyRecoveryCountsOnlyRealPayments is the one that
// matters for trust in the number: a staff-issued goodwill credit has no
// transaction token, and counting it would let anyone inflate collections
// by crediting a wallet.
func TestFR_REV_003_MonthlyRecoveryCountsOnlyRealPayments(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "payer", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "payer2", PlanID: 1})

	ins := func(subID int, entry, amount string, token *string, at time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO wallet_ledgers (subscriber_id, entry_type, amount, balance_after, transaction_token, created_at)
			 VALUES ($1,$2,$3,0,$4,$5)`, subID, entry, amount, token, at); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}
	tok := func(s string) *string { return &s }
	now := time.Now()

	ins(1, "credit", "500.00", tok("pay-1"), now) // counted
	ins(2, "credit", "300.00", tok("pay-2"), now) // counted, second payer
	ins(1, "credit", "250.00", nil, now)          // staff credit: excluded
	ins(1, "debit", "100.00", tok("chg-1"), now)  // a charge, not a collection

	months, err := database.Revenue().GetMonthlyRecovery(ctx, 2)
	if err != nil {
		t.Fatalf("GetMonthlyRecovery: %v", err)
	}
	if len(months) == 0 {
		t.Fatal("want at least the current month, got none")
	}
	if got := months[0].Collected.StringFixed(2); got != "800.00" {
		t.Errorf("collected: want 800.00 (gateway credits only), got %s", got)
	}
	if months[0].Payers != 2 {
		t.Errorf("payers: want 2 distinct subscribers, got %d", months[0].Payers)
	}
}
