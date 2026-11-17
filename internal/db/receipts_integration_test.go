//go:build integration

// Payment receipts — FR-BIL-021.
//
// Every payment gets a receipt and nothing else does: postings are made
// through the real wallet service, so the counter-legs, adjustments and
// bounce reversals that share the ledger with payments are all present.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

func TestFR_BIL_021_PaymentReceipts(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme_ltd", MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "ravi", MobileNumber: "+919800000002"})

	wallet := billing.NewWalletService(store)
	today := time.Now()
	pay := func(sub int, amount string, pd billing.PaymentDetail) int {
		t.Helper()
		tx, err := wallet.Recharge(ctx, billing.RechargeRequest{SubscriberID: sub, Amount: decimal.RequireFromString(amount),
			Description: "recharge via " + pd.Method, Payment: &pd})
		if err != nil {
			t.Fatalf("pay: %v", err)
		}
		return tx.ID
	}
	cash := pay(1, "1180.00", billing.PaymentDetail{Method: billing.MethodCash, RecordedBy: "meena"})
	cheque := pay(1, "500.00", billing.PaymentDetail{Method: billing.MethodCheque, ChequeNumber: "000123",
		ChequeBank: "SBI", ChequeDate: &today, RecordedBy: "meena"})
	tds := pay(1, "100.00", billing.PaymentDetail{Method: billing.MethodTDS, TDSCertificate: "16A-77", RecordedBy: "meena"})
	theirs := pay(2, "799.00", billing.PaymentDetail{Method: billing.MethodUPI, Reference: "UPI-9", RecordedBy: "meena"})

	// A staff adjustment credits the wallet too, but nobody paid.
	adj, err := wallet.Post(ctx, billing.PostRequest{SubscriberID: 1, Amount: decimal.RequireFromString("50.00"),
		Direction: "credit", CounterAccount: billing.AccountAdjustmentClearing, AdjustedBy: "owner", Description: "goodwill"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BounceCheque(ctx, cheque, "insufficient funds", "owner"); err != nil {
		t.Fatalf("bounce: %v", err)
	}
	// A renewal charge, posted as the auto-renewal scanner posts it: a
	// wallet debit with no staff name on it.
	renewal, err := wallet.Post(ctx, billing.PostRequest{SubscriberID: 1, Amount: decimal.RequireFromString("118.00"),
		TaxAmount: decimal.RequireFromString("18.00"), Direction: "debit", CounterAccount: billing.AccountRevenueClearing,
		Description: "auto-renewal: Home 100M"})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a cash payment's receipt carries the amount, method, balance and who took it", func(t *testing.T) {
		r, err := store.GetPaymentReceipt(ctx, cash)
		if err != nil || r == nil {
			t.Fatalf("receipt: %v %v", r, err)
		}
		if r.SubscriberID != 1 || r.Username != "acme_ltd" || r.CAFNumber != "CAF-acme_ltd" ||
			r.Amount.String() != "1180" || r.BalanceAfter.String() != "1180" ||
			r.Method != billing.MethodCash || r.RecordedBy != "meena" || r.Bounced() {
			t.Errorf("cash: %+v", r)
		}
	})

	t.Run("a bounced cheque's receipt shows the bounce", func(t *testing.T) {
		r, _ := store.GetPaymentReceipt(ctx, cheque)
		if r == nil || !r.Bounced() || r.BouncedAt == nil || r.BounceReason != "insufficient funds" ||
			r.ChequeNumber != "000123" || r.ChequeDate == nil {
			t.Errorf("cheque: %+v", r)
		}
	})

	t.Run("TDS keeps its certificate", func(t *testing.T) {
		if r, _ := store.GetPaymentReceipt(ctx, tds); r == nil || r.TDSCertificate != "16A-77" || r.Method != billing.MethodTDS {
			t.Errorf("tds: %+v", r)
		}
	})

	t.Run("nothing that is not a payment has a receipt", func(t *testing.T) {
		var counterLeg, bounceLeg int
		if err := pool.QueryRow(ctx, `SELECT id FROM wallet_ledgers WHERE id < $1 AND account <> 'subscriber_wallet' ORDER BY id DESC LIMIT 1`, cash).Scan(&counterLeg); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT bounce_ledger_id FROM payment_details WHERE wallet_ledger_id = $1`, cheque).Scan(&bounceLeg); err != nil {
			t.Fatal(err)
		}
		for name, id := range map[string]int{"staff adjustment": adj.ID, "counter-leg": counterLeg,
			"bounce reversal": bounceLeg, "renewal charge": renewal.ID, "no such row": 99999} {
			if r, err := store.GetPaymentReceipt(ctx, id); err != nil || r != nil {
				t.Errorf("%s (%d) must have no receipt: %+v %v", name, id, r, err)
			}
		}
	})

	t.Run("a subscriber's list holds their payments only, newest first", func(t *testing.T) {
		mine, err := store.ListSubscriberPayments(ctx, 1, 50)
		if err != nil {
			t.Fatal(err)
		}
		var ids []int
		for _, p := range mine {
			if p.SubscriberID != 1 {
				t.Errorf("another subscriber's payment leaked: %+v", p)
			}
			ids = append(ids, p.PaymentID)
		}
		if len(ids) != 3 || ids[0] != tds || ids[2] != cash {
			t.Errorf("want [tds cheque cash] = [%d %d %d], got %v", tds, cheque, cash, ids)
		}
		other, _ := store.ListSubscriberPayments(ctx, 2, 50)
		if len(other) != 1 || other[0].PaymentID != theirs || other[0].Reference != "UPI-9" {
			t.Errorf("subscriber 2: %+v", other)
		}
	})
}
