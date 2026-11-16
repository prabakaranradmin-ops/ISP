//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/staffui"
	"github.com/shopspring/decimal"
)

func TestFR_BIL_013_015_PaymentsAndBounce(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme_ltd", MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "ravi", MobileNumber: "+919800000002"})

	wallet := billing.NewWalletService(database.Billing())
	today := time.Now()
	pay := func(sub int, amount string, pd billing.PaymentDetail) *billing.Transaction {
		t.Helper()
		tx, err := wallet.Recharge(ctx, billing.RechargeRequest{
			SubscriberID: sub, Amount: decimal.RequireFromString(amount),
			Description: "recharge via " + pd.Method, Payment: &pd,
		})
		if err != nil {
			t.Fatalf("recharge %s %s: %v", pd.Method, amount, err)
		}
		return tx
	}

	cash := pay(1, "500.00", billing.PaymentDetail{Method: billing.MethodCash, RecordedBy: "meena"})
	cheque := pay(1, "1000.00", billing.PaymentDetail{Method: billing.MethodCheque, ChequeNumber: "000123",
		ChequeBank: "SBI", ChequeDate: &today, RecordedBy: "meena"})
	tds := pay(1, "100.00", billing.PaymentDetail{Method: billing.MethodTDS, TDSCertificate: "16A-77", RecordedBy: "meena"})
	spent := pay(2, "799.00", billing.PaymentDetail{Method: billing.MethodCheque, ChequeNumber: "000999",
		ChequeBank: "HDFC", ChequeDate: &today, RecordedBy: "meena"})

	t.Run("each payment carries its method and details", func(t *testing.T) {
		got := scanString(ctx, t, pool, `SELECT method||'/'||cheque_number||'/'||cheque_bank||'/'||status||'/'||recorded_by
		                                   FROM payment_details WHERE wallet_ledger_id = $1`, cheque.ID)
		if got != "cheque/000123/SBI/cleared/meena" {
			t.Errorf("cheque details: %s", got)
		}
		if m := scanString(ctx, t, pool, `SELECT method FROM payment_details WHERE wallet_ledger_id = $1`, cash.ID); m != "cash" {
			t.Errorf("cash method: %s", m)
		}
	})

	t.Run("TDS posts to TDS Receivable, not Cash / Bank", func(t *testing.T) {
		code := scanString(ctx, t, pool, `
			SELECT a.code FROM gl_journal_lines l
			  JOIN chart_of_accounts a ON a.id = l.account_id
			 WHERE l.journal_entry_id = (SELECT max(id) FROM gl_journal_entries
			                              WHERE source_type = 'wallet_ledger' AND source_id = $1)
			   AND l.debit > 0`, tds.ID)
		if code != "1300" {
			t.Errorf("TDS debit should land on 1300, got %s", code)
		}
	})

	store := database.Billing()

	t.Run("a bounced cheque is reversed through the ledger", func(t *testing.T) {
		rev, err := store.BounceCheque(ctx, cheque.ID, "insufficient funds", "owner")
		if err != nil {
			t.Fatalf("BounceCheque: %v", err)
		}
		// 500 + 1000 + 100 - 1000
		if got := scanString(ctx, t, pool, `SELECT wallet_balance::text FROM subscribers WHERE id = 1`); got != "600.00" {
			t.Errorf("balance after bounce: %s", got)
		}
		got := scanString(ctx, t, pool, `SELECT status||'/'||bounce_reason||'/'||bounce_ledger_id FROM payment_details WHERE wallet_ledger_id = $1`, cheque.ID)
		if got != "bounced/insufficient funds/"+itoa(rev.ID) {
			t.Errorf("bounce record: %s", got)
		}
		credit := scanString(ctx, t, pool, `
			SELECT a.code FROM gl_journal_lines l JOIN chart_of_accounts a ON a.id = l.account_id
			 WHERE l.journal_entry_id = (SELECT max(id) FROM gl_journal_entries
			                              WHERE source_type = 'wallet_ledger' AND source_id = $1)
			   AND l.credit > 0`, rev.ID)
		if credit != "1000" {
			t.Errorf("the reversal should credit Cash / Bank (1000), got %s", credit)
		}
	})

	t.Run("it cannot bounce twice, and nothing but a cheque bounces", func(t *testing.T) {
		if _, err := store.BounceCheque(ctx, cheque.ID, "again", "owner"); !errors.Is(err, billing.ErrNotBounceable) {
			t.Errorf("second bounce: want ErrNotBounceable, got %v", err)
		}
		if _, err := store.BounceCheque(ctx, cash.ID, "x", "owner"); !errors.Is(err, billing.ErrNotBounceable) {
			t.Errorf("cash bounce: want ErrNotBounceable, got %v", err)
		}
	})

	t.Run("a spent cheque is refused and nothing is written", func(t *testing.T) {
		// Ravi's 799.00 cheque paid for a renewal that already took it.
		if _, err := pool.Exec(ctx, `UPDATE subscribers SET wallet_balance = 0 WHERE id = 2`); err != nil {
			t.Fatal(err)
		}
		before := countRows(ctx, t, pool, `SELECT count(*) FROM wallet_ledgers`)
		_, err := store.BounceCheque(ctx, spent.ID, "funds insufficient", "owner")
		if !errors.Is(err, billing.ErrInsufficientBalance) {
			t.Fatalf("want ErrInsufficientBalance, got %v", err)
		}
		if after := countRows(ctx, t, pool, `SELECT count(*) FROM wallet_ledgers`); after != before {
			t.Errorf("no ledger leg may be written, %d -> %d", before, after)
		}
		if st := scanString(ctx, t, pool, `SELECT status FROM payment_details WHERE wallet_ledger_id = $1`, spent.ID); st != "cleared" {
			t.Errorf("the cheque must stay cleared until it can really be reversed, got %s", st)
		}
	})

	t.Run("the payments list splits cash, TDS and bounced", func(t *testing.T) {
		rows, tot, err := database.Console().ListPayments(ctx, staffui.MoneyFilter{
			From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		// Collected: 500 cash + 799 cleared cheque. TDS 100. Bounced 1000.
		if tot.Count != 4 || tot.Amount != "1299.00" || tot.TDS != "100.00" || tot.BouncedCount != 1 || tot.Bounced != "1000.00" {
			t.Errorf("totals: %+v", tot)
		}
		for _, r := range rows {
			if r.ID == cheque.ID && (r.Status != "bounced" || r.ChequeNumber != "000123" || r.ChequeDate == nil) {
				t.Errorf("bounced cheque row: %+v", r)
			}
		}
		byMethod, _, err := database.Console().ListPayments(ctx, staffui.MoneyFilter{
			From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Minute), Method: "tds"})
		if err != nil || len(byMethod) != 1 || byMethod[0].TDSCertificate != "16A-77" {
			t.Errorf("method filter: %+v %v", byMethod, err)
		}
	})
}
