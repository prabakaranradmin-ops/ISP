package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// IssueCreditNote writes a credit note against an invoice and returns its
// value to the subscriber's wallet, in one transaction (FR-BIL-012).
//
// Three things happen or none do: the GST document (credit_notes, bounded by
// migration 049's trigger), the wallet credit (wallet_ledgers + balance),
// and the general-ledger reversal of the original charge — revenue (4000)
// and output GST (2200) debited back, subscriber wallet liability (1200)
// credited — through the same posting path a renewal charge used, with the
// counter leg's direction flipped.
//
// The invoice row is locked first, so two credit notes approved at the same
// moment against one invoice are computed one after the other from what the
// first actually wrote, rather than both from the same "already credited".
func (s *BillingStore) IssueCreditNote(ctx context.Context, approvalID, invoiceID int, base decimal.Decimal,
	reason, requestedBy, approvedBy string,
) (*billing.IssuedCreditNote, error) {
	var out *billing.IssuedCreditNote
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			inv                                    billing.InvoiceAmounts
			subscriberID                           int
			franchiseID                            *int
			b, cg, sg, ig, tot, rc, rs, ri, wallet string
		)
		err := tx.QueryRow(ctx, `
			SELECT i.subscriber_id, s.franchise_id,
			       i.base_amount::text, i.cgst_amount::text, i.sgst_amount::text,
			       i.igst_amount::text, i.total_amount::text,
			       g.cgst_rate::text, g.sgst_rate::text, g.igst_rate::text,
			       s.wallet_balance::text
			  FROM invoices i
			  JOIN gst_rates g ON g.id = i.gst_rate_id
			  JOIN subscribers s ON s.id = i.subscriber_id
			 WHERE i.id = $1
			   FOR UPDATE OF i, s`, invoiceID).Scan(
			&subscriberID, &franchiseID, &b, &cg, &sg, &ig, &tot, &rc, &rs, &ri, &wallet)
		if isNoRows(err) {
			return fmt.Errorf("db: issue credit note: invoice %d: %w", invoiceID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("db: issue credit note: load invoice %d: %w", invoiceID, err)
		}
		vals := []*decimal.Decimal{&inv.Base, &inv.CGST, &inv.SGST, &inv.IGST, &inv.Total, &inv.CGSTRate, &inv.SGSTRate, &inv.IGSTRate}
		for i, sv := range []string{b, cg, sg, ig, tot, rc, rs, ri} {
			if *vals[i], err = parseDecimal(sv); err != nil {
				return err
			}
		}
		balance, err := parseDecimal(wallet)
		if err != nil {
			return err
		}

		var credited billing.CreditNoteAmounts
		var cb, ccg, csg, cig string
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(sum(base_amount),0)::text, COALESCE(sum(cgst_amount),0)::text,
			       COALESCE(sum(sgst_amount),0)::text, COALESCE(sum(igst_amount),0)::text
			  FROM credit_notes WHERE invoice_id = $1`, invoiceID).Scan(&cb, &ccg, &csg, &cig); err != nil {
			return fmt.Errorf("db: issue credit note: credited so far: %w", err)
		}
		for i, sv := range []string{cb, ccg, csg, cig} {
			v, err := parseDecimal(sv)
			if err != nil {
				return err
			}
			*[]*decimal.Decimal{&credited.Base, &credited.CGST, &credited.SGST, &credited.IGST}[i] = v
		}

		amt, err := billing.ComputeCreditNote(inv, credited, base)
		if err != nil {
			return err
		}

		var noteID int
		if err := tx.QueryRow(ctx, `
			INSERT INTO credit_notes (invoice_id, subscriber_id, base_amount, cgst_amount, sgst_amount,
			                          igst_amount, total_amount, reason, approval_request_id)
			VALUES ($1, $2, $3::numeric, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $8, $9)
			RETURNING id`,
			invoiceID, subscriberID, amt.Base.String(), amt.CGST.String(), amt.SGST.String(),
			amt.IGST.String(), amt.Total.String(), reason, approvalID).Scan(&noteID); err != nil {
			return fmt.Errorf("db: issue credit note: insert: %w", err)
		}

		newBalance := balance.Add(amt.Total)
		desc := fmt.Sprintf("credit note %s against invoice INV-%06d: %s (approved by %s)",
			billing.CreditNoteNumber(noteID), invoiceID, reason, approvedBy)
		posting := billing.RechargePosting{
			SubscriberID: subscriberID,
			NewBalance:   newBalance,
			// The GL reverses revenue and output GST separately, exactly as
			// much tax as the document credits — not re-derived from a rate.
			TaxAmount: amt.Tax(),
			Credit: billing.WalletEntry{
				SubscriberID: subscriberID, FranchiseID: franchiseID,
				Account: billing.AccountSubscriberWallet, EntryType: "credit",
				Amount: amt.Total, BalanceAfter: newBalance,
				Description: desc, AdjustedBy: requestedBy,
			},
			Debit: billing.WalletEntry{
				SubscriberID: subscriberID, FranchiseID: franchiseID,
				Account: billing.AccountRevenueClearing, EntryType: "debit",
				Amount: amt.Total, BalanceAfter: newBalance,
				Description: "counter-entry: " + desc, AdjustedBy: requestedBy,
			},
		}
		leg, err := recordPostingTx(ctx, tx, posting)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE credit_notes SET wallet_ledger_id = $2 WHERE id = $1`, noteID, leg.ID); err != nil {
			return fmt.Errorf("db: issue credit note: link wallet leg: %w", err)
		}
		out = &billing.IssuedCreditNote{
			ID: noteID, InvoiceID: invoiceID, SubscriberID: subscriberID,
			Amounts: amt, WalletLedgerID: leg.ID, NewBalance: newBalance,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
