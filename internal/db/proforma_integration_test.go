//go:build integration

// Proforma invoices — FR-BIL-018 | migration 056.
//
// The properties that matter are the ones only the database can show: a
// proforma never takes a tax invoice number, conversion writes the wallet
// debit, the GL split and the invoice together or not at all, tax is
// charged at the rate in force on conversion, and a proforma converts once.
package db_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/shopspring/decimal"
)

func TestFR_BIL_018_ProformaLifecycle(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acme_tn", Balance: "100.00", MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "acme_ka", Balance: "5000.00", RegisteredSt: "KA", MobileNumber: "+919800000002"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "gone", Status: "terminated", MobileNumber: "+919800000003"})

	// One renewal invoice already exists, so the next tax invoice is INV-2.
	var firstInvoice int
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
		                      total_amount, gst_rate_id, gb_included, gb_used)
		VALUES (1, 799.00, 71.91, 71.91, 0, 942.82, 1, 3300, 0) RETURNING id`).Scan(&firstInvoice); err != nil {
		t.Fatal(err)
	}

	issue := func(sub int, desc, base string) *billing.Proforma {
		t.Helper()
		p, err := store.CreateProforma(ctx, billing.ProformaRequest{SubscriberID: sub, Description: desc,
			Base: decimal.RequireFromString(base)}, "TN", "meena")
		if err != nil {
			t.Fatalf("issue %s: %v", desc, err)
		}
		return p
	}
	balance := func(sub int) string {
		t.Helper()
		return scanString(ctx, t, pool, `SELECT wallet_balance::text FROM subscribers WHERE id = $1`, sub)
	}
	invoiceCount := func() string {
		t.Helper()
		return scanString(ctx, t, pool, `SELECT count(*)::text FROM invoices`)
	}

	tn := issue(1, "Installation and router", "1000.00")
	ka := issue(2, "Static IP, 12 months", "1000.00")
	issue(2, "Relocation", "500.00")

	t.Run("GST is estimated by the subscriber's state against the supplier's", func(t *testing.T) {
		if tn.CGST.String() != "90" || tn.SGST.String() != "90" || !tn.IGST.IsZero() || tn.Total.String() != "1180" {
			t.Errorf("intrastate: %s/%s/%s/%s", tn.CGST, tn.SGST, tn.IGST, tn.Total)
		}
		if ka.IGST.String() != "180" || !ka.CGST.IsZero() || ka.Total.String() != "1180" {
			t.Errorf("interstate: %s/%s/%s", ka.CGST, ka.IGST, ka.Total)
		}
		want := billing.ProformaValidUntil(time.Now(), billing.ProformaDefaultValid).Format("2006-01-02")
		if got := tn.ValidUntil.Format("2006-01-02"); got != want || tn.Status != billing.ProformaOpen || tn.CreatedBy != "meena" {
			t.Errorf("valid until %s (want %s), status %s, by %s", got, want, tn.Status, tn.CreatedBy)
		}
	})

	t.Run("issuing takes no tax invoice number and nothing reaches GSTR-1", func(t *testing.T) {
		if invoiceCount() != "1" {
			t.Errorf("proformas wrote %s invoices rows", invoiceCount())
		}
		now := time.Now().UTC()
		rows, err := store.ListInvoicesForGSTR1(ctx, now.Year(), now.Month())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Errorf("GSTR-1 saw %d rows; only the one tax invoice belongs there", len(rows))
		}
	})

	t.Run("a terminated or unknown subscriber cannot be quoted", func(t *testing.T) {
		_, err := store.CreateProforma(ctx, billing.ProformaRequest{SubscriberID: 3, Description: "x", Base: decimal.NewFromInt(1)}, "TN", "meena")
		if !errors.Is(err, billing.ErrProformaInvalid) {
			t.Errorf("terminated: %v", err)
		}
		_, err = store.CreateProforma(ctx, billing.ProformaRequest{SubscriberID: 99, Description: "x", Base: decimal.NewFromInt(1)}, "TN", "meena")
		if !errors.Is(err, db.ErrNotFound) {
			t.Errorf("unknown: %v", err)
		}
	})

	t.Run("conversion waits for payment and writes nothing until it can", func(t *testing.T) {
		_, err := store.ConvertProforma(ctx, tn.ID, "TN", "meena")
		if !errors.Is(err, billing.ErrInsufficientBalance) {
			t.Fatalf("want ErrInsufficientBalance, got %v", err)
		}
		p, _ := store.GetProforma(ctx, tn.ID)
		if balance(1) != "100.00" || invoiceCount() != "1" || p.Status != billing.ProformaOpen {
			t.Errorf("after a refused conversion: balance %s, invoices %s, status %s", balance(1), invoiceCount(), p.Status)
		}
	})

	t.Run("converting debits the wallet, splits the GL and issues the next invoice number", func(t *testing.T) {
		// The customer pays; then the tax invoice follows.
		if _, err := pool.Exec(ctx, `UPDATE subscribers SET wallet_balance = 1200.00 WHERE id = 1`); err != nil {
			t.Fatal(err)
		}
		c, err := store.ConvertProforma(ctx, tn.ID, "TN", "owner")
		if err != nil {
			t.Fatalf("convert: %v", err)
		}
		if c.InvoiceID != firstInvoice+1 {
			t.Errorf("tax invoice numbers must run on without gaps: want %d, got %d", firstInvoice+1, c.InvoiceID)
		}
		if balance(1) != "20.00" || c.NewBalance.String() != "20" {
			t.Errorf("balance %s / %s", balance(1), c.NewBalance)
		}
		inv := scanString(ctx, t, pool, `SELECT base_amount||'/'||cgst_amount||'/'||sgst_amount||'/'||igst_amount||'/'||total_amount||'/'||description
		                                  FROM invoices WHERE id = $1`, c.InvoiceID)
		if inv != "1000.00/90.00/90.00/0.00/1180.00/Installation and router" {
			t.Errorf("invoice: %s", inv)
		}
		pf := scanString(ctx, t, pool, `SELECT status||'/'||invoice_id||'/'||converted_by FROM proforma_invoices WHERE id = $1`, tn.ID)
		if want := "converted/" + itoa(c.InvoiceID) + "/owner"; pf != want {
			t.Errorf("proforma: want %s, got %s", want, pf)
		}
		leg := scanString(ctx, t, pool, `SELECT entry_type||'/'||amount||'/'||adjusted_by_username FROM wallet_ledgers WHERE id = $1`, c.WalletLedgerID)
		if leg != "debit/1180.00/owner" {
			t.Errorf("wallet leg: %s", leg)
		}
		gl := scanString(ctx, t, pool, `
			SELECT string_agg(a.code||':'||l.debit||':'||l.credit, ' ' ORDER BY a.code)
			  FROM gl_journal_lines l JOIN chart_of_accounts a ON a.id = l.account_id
			 WHERE l.journal_entry_id = (SELECT max(id) FROM gl_journal_entries
			                              WHERE source_type = 'wallet_ledger' AND source_id = $1)`, c.WalletLedgerID)
		if gl != "1200:1180.00:0.00 2200:0.00:180.00 4000:0.00:1000.00" {
			t.Errorf("GL: wallet debited, revenue and GST payable credited separately; got %s", gl)
		}
		d, err := store.GetInvoiceDetail(ctx, c.InvoiceID)
		if err != nil || d.LineDescription != "Installation and router" {
			t.Errorf("invoice detail description: %+v %v", d, err)
		}
	})

	t.Run("a proforma converts once", func(t *testing.T) {
		if _, err := store.ConvertProforma(ctx, tn.ID, "TN", "owner"); !errors.Is(err, billing.ErrProformaNotOpen) {
			t.Errorf("second conversion: %v", err)
		}
		if balance(1) != "20.00" {
			t.Errorf("charged twice: %s", balance(1))
		}
	})

	t.Run("two people converting at the same moment produce one invoice", func(t *testing.T) {
		before := invoiceCount()
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = store.ConvertProforma(ctx, ka.ID, "TN", "meena")
			}(i)
		}
		wg.Wait()
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if !errors.Is(err, billing.ErrProformaNotOpen) {
				t.Errorf("loser should see NotOpen, got %v", err)
			}
		}
		if ok != 1 || balance(2) != "3820.00" {
			t.Errorf("successes %d, balance %s (want one charge of 1180.00)", ok, balance(2))
		}
		if after := invoiceCount(); after != itoa(atoiT(t, before)+1) {
			t.Errorf("invoices went from %s to %s", before, after)
		}
		// Interstate stays IGST on the tax invoice too.
		if igst := scanString(ctx, t, pool, `SELECT i.igst_amount::text FROM invoices i JOIN proforma_invoices p ON p.invoice_id = i.id WHERE p.id = $1`, ka.ID); igst != "180.00" {
			t.Errorf("interstate invoice IGST: %s", igst)
		}
	})

	t.Run("tax is charged at the rate in force on conversion, not the estimate", func(t *testing.T) {
		p := issue(2, "Equipment", "1000.00") // estimated at 18%
		if _, err := pool.Exec(ctx, `INSERT INTO gst_rates (id, cgst_rate, sgst_rate, igst_rate, effective_from)
		                              VALUES (2, 6.00, 6.00, 12.00, NOW() - INTERVAL '1 minute')`); err != nil {
			t.Fatal(err)
		}
		c, err := store.ConvertProforma(ctx, p.ID, "TN", "meena")
		if err != nil {
			t.Fatal(err)
		}
		if c.Invoice.IgstAmount.String() != "120" || c.Invoice.TotalAmount.String() != "1120" || c.Invoice.GstRateID != 2 {
			t.Errorf("want 12%% IGST at rate 2, got %s total %s rate %d", c.Invoice.IgstAmount, c.Invoice.TotalAmount, c.Invoice.GstRateID)
		}
		again, _ := store.GetProforma(ctx, p.ID)
		if again.IGST.String() != "180" {
			t.Errorf("the proforma's own estimate must stand as issued: %s", again.IGST)
		}
	})

	t.Run("an expired proforma is refused", func(t *testing.T) {
		p := issue(2, "Old quote", "10.00")
		if _, err := pool.Exec(ctx, `UPDATE proforma_invoices SET valid_until = CURRENT_DATE - 1 WHERE id = $1`, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ConvertProforma(ctx, p.ID, "TN", "meena"); !errors.Is(err, billing.ErrProformaExpired) {
			t.Errorf("want ErrProformaExpired, got %v", err)
		}
		// Valid through today.
		if _, err := pool.Exec(ctx, `UPDATE proforma_invoices SET valid_until = CURRENT_DATE WHERE id = $1`, p.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ConvertProforma(ctx, p.ID, "TN", "meena"); err != nil {
			t.Errorf("a proforma valid until today must convert today: %v", err)
		}
	})

	t.Run("cancelling needs a reason and closes it", func(t *testing.T) {
		p := issue(2, "Declined", "10.00")
		if err := store.CancelProforma(ctx, p.ID, " ", "meena"); !errors.Is(err, billing.ErrProformaInvalid) {
			t.Errorf("no reason: %v", err)
		}
		if err := store.CancelProforma(ctx, p.ID, "customer chose another ISP", "meena"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ConvertProforma(ctx, p.ID, "TN", "meena"); !errors.Is(err, billing.ErrProformaNotOpen) {
			t.Errorf("converting a cancelled proforma: %v", err)
		}
		if err := store.CancelProforma(ctx, p.ID, "again", "meena"); !errors.Is(err, billing.ErrProformaNotOpen) {
			t.Errorf("cancelling twice: %v", err)
		}
		if err := store.CancelProforma(ctx, 9999, "x", "meena"); !errors.Is(err, billing.ErrProformaNotFound) {
			t.Errorf("cancelling nothing: %v", err)
		}
	})

	t.Run("the table refuses a status without the fields that describe it", func(t *testing.T) {
		p := issue(2, "Guarded", "10.00")
		for name, q := range map[string]string{
			"converted with no invoice":  `UPDATE proforma_invoices SET status = 'converted', converted_at = now(), converted_by = 'x' WHERE id = $1`,
			"cancelled with no reason":   `UPDATE proforma_invoices SET status = 'cancelled', cancelled_at = now(), cancelled_by = 'x' WHERE id = $1`,
			"total that does not add up": `UPDATE proforma_invoices SET total_amount = total_amount + 1 WHERE id = $1`,
		} {
			if _, err := pool.Exec(ctx, q, p.ID); err == nil {
				t.Errorf("%s: accepted", name)
			}
		}
	})

	t.Run("lists filter by status and subscriber", func(t *testing.T) {
		open, err := store.ListProformas(ctx, billing.ProformaFilter{Status: billing.ProformaOpen})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range open {
			if p.Status != billing.ProformaOpen {
				t.Errorf("status filter leaked %s", p.Status)
			}
		}
		mine, _ := store.ListProformas(ctx, billing.ProformaFilter{SubscriberID: 1})
		if len(mine) != 1 || mine[0].ID != tn.ID || mine[0].InvoiceID == nil {
			t.Errorf("subscriber filter: %+v", mine)
		}
	})
}

func atoiT(t *testing.T, s string) int {
	t.Helper()
	n, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatal(err)
	}
	return int(n.IntPart())
}
