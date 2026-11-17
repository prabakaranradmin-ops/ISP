//go:build integration

// Billing diagnostics — FR-BIL-019.
//
// One subscriber per case, each built to sit just inside or just outside a
// check, so a query that is too loose or too strict shows up as a named
// subscriber in the wrong list.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

func TestFR_BIL_019_BillingDiagnostics(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedGstRate(ctx, t, pool, 1)
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00") // 30-day validity
	seedPlan(ctx, t, pool, 2, "Free trial", "10M/10M", 0, "", "0.00")

	now := time.Now()
	day := func(n float64) time.Time { return now.Add(time.Duration(n * 24 * float64(time.Hour))) }
	at := func(n float64) *time.Time { v := day(n); return &v }
	invoice := func(sub int, base string, created time.Time, description *string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
			                      total_amount, gst_rate_id, gb_included, gb_used, created_at, description)
			VALUES ($1, $2::numeric, 0, 0, 0, $2::numeric, 1, 0, 0, $3, $4)`, sub, base, created, description); err != nil {
			t.Fatalf("seed invoice for %d: %v", sub, err)
		}
	}
	type subOpts = seedOpts
	n := 0
	sub := func(name string, o subOpts) int {
		t.Helper()
		n++
		o.Username, o.MobileNumber = name, "+9198000000"+string(rune('0'+n/10))+string(rune('0'+n%10))
		seedSubscriber(ctx, t, pool, n, o)
		return n
	}

	// ── Check 1: served this cycle without an invoice ──────────────────────
	renewed := sub("renewed_on_time", subOpts{PlanExpiry: at(20)}) // cycle began 10 days ago
	invoice(renewed, "799.00", day(-10), nil)
	// The renewal wrote its invoice a few seconds before the expiry it set:
	// the day's allowance must absorb that.
	jitter := sub("invoice_just_before_cycle", subOpts{PlanExpiry: at(30)})
	invoice(jitter, "799.00", day(-0.0001), nil)
	never := sub("never_invoiced", subOpts{PlanExpiry: at(20)})
	lapsedInvoice := sub("only_last_cycle_invoiced", subOpts{PlanExpiry: at(20)})
	invoice(lapsedInvoice, "799.00", day(-40), nil)
	// Renewed early from the portal: still in the old cycle, invoice for
	// the next one already raised. Two invoices since the old cycle began.
	early := sub("renewed_early", subOpts{PlanExpiry: at(50)})
	invoice(early, "799.00", day(-10), nil)
	invoice(early, "799.00", day(-2), nil)
	// A plan change grants the new plan's full validity plus credit for
	// unused days, and charges nothing (api.computePlanChangeExpiry).
	planChange := sub("plan_changed", subOpts{PlanExpiry: at(35)})
	invoice(planChange, "799.00", day(-25), nil)
	// Only an installation charge this cycle: a one-off invoice does not
	// pay for the cycle.
	oneOffOnly := sub("only_a_one_off_charge", subOpts{PlanExpiry: at(20)})
	install := "Installation"
	invoice(oneOffOnly, "1500.00", day(-3), &install)
	free := sub("free_plan", subOpts{PlanID: 2, PlanExpiry: at(20)})
	pending := sub("not_yet_paying", subOpts{Status: "pending_payment"})
	expired := sub("past_expiry", subOpts{Status: "grace_period", DunningState: "grace_period", PlanExpiry: at(-2)})
	_, _, _ = free, pending, expired

	// ── Check 2: wallet balance against the ledger ──────────────────────────
	wallet := billing.NewWalletService(store)
	consistent := sub("wallet_consistent", subOpts{PlanExpiry: at(20)})
	invoice(consistent, "799.00", day(-10), nil)
	if _, err := wallet.Recharge(ctx, billing.RechargeRequest{SubscriberID: consistent, Amount: decimal.RequireFromString("500.00"), Description: "top-up"}); err != nil {
		t.Fatal(err)
	}
	if _, err := wallet.Post(ctx, billing.PostRequest{SubscriberID: consistent, Amount: decimal.RequireFromString("118.00"),
		TaxAmount: decimal.RequireFromString("18.00"), Direction: "debit", CounterAccount: billing.AccountRevenueClearing,
		Description: "charge"}); err != nil {
		t.Fatal(err)
	}
	edited := sub("balance_edited_directly", subOpts{PlanExpiry: at(20)})
	invoice(edited, "799.00", day(-10), nil)
	if _, err := wallet.Recharge(ctx, billing.RechargeRequest{SubscriberID: edited, Amount: decimal.RequireFromString("500.00"), Description: "top-up"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE subscribers SET wallet_balance = 700.00 WHERE id = $1`, edited); err != nil {
		t.Fatal(err)
	}
	unledgered := sub("balance_without_ledger", subOpts{PlanExpiry: at(20), Balance: "100.00"})
	invoice(unledgered, "799.00", day(-10), nil)

	// ── Check 3: billed at a price other than the catalogue's ───────────────
	stale := sub("billed_old_price", subOpts{PlanExpiry: at(20)})
	invoice(stale, "699.00", day(-10), nil)
	// A newer one-off charge is not a plan invoice and must not mask it.
	charge := "Installation"
	invoice(stale, "1500.00", day(-1), &charge)
	corrected := sub("old_price_then_current", subOpts{PlanExpiry: at(20)})
	invoice(corrected, "699.00", day(-40), nil)
	invoice(corrected, "799.00", day(-10), nil)
	gone := sub("terminated_old_price", subOpts{Status: "terminated"})
	invoice(gone, "699.00", day(-40), nil)

	d, err := store.BillingDiagnostics(ctx, 100)
	if err != nil {
		t.Fatalf("BillingDiagnostics: %v", err)
	}
	names := func(ids []int) map[int]bool {
		m := map[int]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	var unIDs, mmIDs, drIDs []int
	for _, u := range d.Uninvoiced {
		unIDs = append(unIDs, u.SubscriberID)
	}
	for _, m := range d.Mismatches {
		mmIDs = append(mmIDs, m.SubscriberID)
	}
	for _, x := range d.Drifts {
		drIDs = append(drIDs, x.SubscriberID)
	}

	t.Run("served this cycle without an invoice", func(t *testing.T) {
		got := names(unIDs)
		for id, label := range map[int]string{never: "never invoiced", lapsedInvoice: "only last cycle invoiced", planChange: "plan change", oneOffOnly: "only a one-off charge"} {
			if !got[id] {
				t.Errorf("%s (subscriber %d) should be listed", label, id)
			}
		}
		for id, label := range map[int]string{renewed: "renewed on time", jitter: "invoice seconds before the cycle",
			early: "renewed early", free: "free plan", pending: "not yet paying", expired: "past expiry",
			consistent: "paid-up wallet subscriber"} {
			if got[id] {
				t.Errorf("%s (subscriber %d) must not be listed", label, id)
			}
		}
		if d.UninvoicedCount != len(d.Uninvoiced) {
			t.Errorf("count %d vs %d listed", d.UninvoicedCount, len(d.Uninvoiced))
		}
		for _, u := range d.Uninvoiced {
			if u.SubscriberID == never && u.LastInvoice != nil {
				t.Error("never-invoiced should carry no last invoice")
			}
			if u.SubscriberID == lapsedInvoice && (u.LastInvoice == nil || u.CycleStart.Before(day(-10.1)) || u.CycleStart.After(day(-9.9))) {
				t.Errorf("only-last-cycle: last %v, cycle start %v", u.LastInvoice, u.CycleStart)
			}
		}
	})

	t.Run("wallet balances that disagree with the ledger", func(t *testing.T) {
		got := names(mmIDs)
		if !got[edited] || !got[unledgered] {
			t.Errorf("edited %v, unledgered %v; both should be listed", got[edited], got[unledgered])
		}
		if got[consistent] {
			t.Error("postings made through the wallet service must reconcile; counter-legs must not be counted")
		}
		if len(mmIDs) != 2 {
			t.Errorf("only the two broken wallets belong here, got %v", mmIDs)
		}
		for _, m := range d.Mismatches {
			switch m.SubscriberID {
			case edited:
				if m.Stored.String() != "700" || m.LedgerSum.String() != "500" || m.Difference().String() != "200" || m.LastAfter == nil || m.Entries != 1 {
					t.Errorf("edited: %+v", m)
				}
			case unledgered:
				if m.LastAfter != nil || m.Entries != 0 || m.Difference().String() != "100" {
					t.Errorf("unledgered: %+v", m)
				}
			}
		}
		// Largest discrepancy first.
		if len(d.Mismatches) == 2 && d.Mismatches[0].SubscriberID != edited {
			t.Error("the bigger gap should lead")
		}
	})

	t.Run("billed at a price other than the catalogue's", func(t *testing.T) {
		got := names(drIDs)
		if !got[stale] {
			t.Error("a plan invoice at the old price must be listed even with a newer one-off charge")
		}
		if got[corrected] || got[gone] {
			t.Errorf("corrected %v, terminated %v must not be listed", got[corrected], got[gone])
		}
		for _, x := range d.Drifts {
			if x.SubscriberID == stale && (x.BilledBase.String() != "699" || x.Difference().String() != "-100") {
				t.Errorf("stale: %+v", x)
			}
		}
	})

	t.Run("the counts cover everything even when the list is cut short", func(t *testing.T) {
		short, err := store.BillingDiagnostics(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(short.Uninvoiced) != 1 || short.UninvoicedCount != d.UninvoicedCount || short.MismatchCount != 2 {
			t.Errorf("limit 1: %d listed, counts %d/%d", len(short.Uninvoiced), short.UninvoicedCount, short.MismatchCount)
		}
	})
}
