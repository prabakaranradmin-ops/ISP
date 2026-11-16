//go:build integration

package db_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// creditNoteFixture is one subscriber with 100.00 in their wallet and the
// 799.00 + 9% + 9% invoice seed_local.sql raises, plus an approved
// credit-note request for it.
func creditNoteFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (invoiceID, approvalID int) {
	t.Helper()
	seedPlan(ctx, t, pool, 1, "Home 100M", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "alice", Balance: "100.00", MobileNumber: "+919800000001"})
	if err := pool.QueryRow(ctx, `
		INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
		                      total_amount, gst_rate_id, gb_included, gb_used)
		VALUES (1, 799.00, 71.91, 71.91, 0, 942.82, 1, 0, 0) RETURNING id`).Scan(&invoiceID); err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO approval_requests (action_type, subscriber_id, amount, reason, requested_by_username, invoice_id)
		VALUES ('credit_note', 1, 100.00, 'overcharge', 'billing1', $1) RETURNING id`, invoiceID).Scan(&approvalID); err != nil {
		t.Fatalf("seed approval: %v", err)
	}
	return invoiceID, approvalID
}

func TestFR_BIL_012_IssueCreditNote(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	invoiceID, approvalID := creditNoteFixture(ctx, t, pool)
	store := database.Billing()

	note, err := store.IssueCreditNote(ctx, approvalID, invoiceID, decimal.RequireFromString("100"), "overcharge", "billing1", "owner")
	if err != nil {
		t.Fatalf("IssueCreditNote: %v", err)
	}

	t.Run("the document carries the invoice's own split", func(t *testing.T) {
		got := scanString(ctx, t, pool, `SELECT base_amount||'/'||cgst_amount||'/'||sgst_amount||'/'||igst_amount||'/'||total_amount
		                                   FROM credit_notes WHERE id = $1`, note.ID)
		if got != "100.00/9.00/9.00/0.00/118.00" {
			t.Errorf("credit note: %s", got)
		}
		links := scanString(ctx, t, pool, `SELECT approval_request_id||'/'||wallet_ledger_id FROM credit_notes WHERE id = $1`, note.ID)
		if want := itoa(approvalID) + "/" + itoa(note.WalletLedgerID); links != want {
			t.Errorf("links: want %s, got %s", want, links)
		}
	})

	t.Run("the money is back in the wallet, attributed, and not counted as a payment", func(t *testing.T) {
		assertDecimalEqual(t, "new balance", note.NewBalance, "218.00")
		if got := scanString(ctx, t, pool, `SELECT wallet_balance::text FROM subscribers WHERE id = 1`); got != "218.00" {
			t.Errorf("stored balance: %s", got)
		}
		by := scanString(ctx, t, pool, `SELECT adjusted_by_username FROM wallet_ledgers WHERE id = $1`, note.WalletLedgerID)
		if by != "billing1" {
			t.Errorf("wallet leg must name the requester (it is excluded from payments by that), got %q", by)
		}
	})

	t.Run("the general ledger reverses revenue and GST, and balances", func(t *testing.T) {
		rows, err := pool.Query(ctx, `
			SELECT a.code, l.debit::text, l.credit::text
			  FROM gl_journal_lines l
			  JOIN gl_journal_entries e ON e.id = l.journal_entry_id
			  JOIN chart_of_accounts a ON a.id = l.account_id
			 WHERE e.id = (SELECT max(id) FROM gl_journal_entries
			                WHERE source_type = 'wallet_ledger' AND source_id = $1)
			 ORDER BY a.code`, note.WalletLedgerID)
		// max(id): the GL tables are not truncated between tests while
		// wallet_ledgers ids restart at 1, so other tests' entries can share
		// this source_id. This test's entry is the newest one carrying it.
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var code, dr, cr string
			if err := rows.Scan(&code, &dr, &cr); err != nil {
				t.Fatal(err)
			}
			got[code] = dr + "/" + cr
		}
		want := map[string]string{"1200": "0.00/118.00", "2200": "18.00/0.00", "4000": "100.00/0.00"}
		for code, w := range want {
			if got[code] != w {
				t.Errorf("GL %s: want %s (debit/credit), got %q (all lines %v)", code, w, got[code], got)
			}
		}
	})

	t.Run("crediting more than remains writes nothing at all", func(t *testing.T) {
		before := countRows(ctx, t, pool, `SELECT count(*) FROM wallet_ledgers`)
		_, err := store.IssueCreditNote(ctx, approvalID, invoiceID, decimal.RequireFromString("700"), "too much", "billing1", "owner")
		if !errors.Is(err, billing.ErrCreditExceedsInvoice) {
			t.Fatalf("want ErrCreditExceedsInvoice, got %v", err)
		}
		if n := countRows(ctx, t, pool, `SELECT count(*) FROM credit_notes WHERE invoice_id = `+itoa(invoiceID)); n != 1 {
			t.Errorf("no second note may exist, found %d", n)
		}
		if after := countRows(ctx, t, pool, `SELECT count(*) FROM wallet_ledgers`); after != before {
			t.Errorf("no wallet leg may be written, %d -> %d", before, after)
		}
	})

	t.Run("the remainder credits to exactly zero", func(t *testing.T) {
		rest, err := store.IssueCreditNote(ctx, approvalID, invoiceID, decimal.RequireFromString("699"), "rest", "billing1", "owner")
		if err != nil {
			t.Fatalf("remainder: %v", err)
		}
		assertDecimalEqual(t, "remainder total", rest.Amounts.Total, "824.82")
		sum := scanString(ctx, t, pool, `SELECT sum(total_amount)::text FROM credit_notes WHERE invoice_id = $1`, invoiceID)
		if sum != "942.82" {
			t.Errorf("credit notes must sum to the invoice total, got %s", sum)
		}
	})
}

// TestFR_BIL_012_ConcurrentCreditNotesCannotOverCredit: two approvals of
// 500.00 each against one 799.00 invoice, at the same moment. The invoice
// lock makes the second compute from what the first wrote, so exactly one
// succeeds.
func TestFR_BIL_012_ConcurrentCreditNotesCannotOverCredit(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	invoiceID, approvalID := creditNoteFixture(ctx, t, pool)
	store := database.Billing()

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.IssueCreditNote(ctx, approvalID, invoiceID, decimal.RequireFromString("500"), "race", "billing1", "owner")
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	ok, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, billing.ErrCreditExceedsInvoice):
			refused++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != 1 {
		t.Errorf("want one success and one refusal, got %d and %d", ok, refused)
	}
	if got := scanString(ctx, t, pool, `SELECT wallet_balance::text FROM subscribers WHERE id = 1`); got != "690.00" {
		t.Errorf("balance: want 100.00 + 590.00 once, got %s", got)
	}
}

func itoa(n int) string { return decimal.NewFromInt(int64(n)).String() }
