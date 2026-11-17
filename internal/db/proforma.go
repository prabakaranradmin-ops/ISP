package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// Proforma invoices — FR-BIL-018 | migration 056. See billing/proforma.go
// for what a proforma is and why converting one charges the wallet.

const proformaColumns = `
	p.id, p.subscriber_id, s.username, COALESCE(s.mobile_number, ''), s.registered_state, p.description,
	p.base_amount::text, p.cgst_amount::text, p.sgst_amount::text, p.igst_amount::text,
	p.total_amount::text, g.cgst_rate::text, g.sgst_rate::text, g.igst_rate::text,
	p.valid_until, p.status, p.created_by, p.created_at, p.invoice_id,
	p.converted_at, COALESCE(p.converted_by, ''), p.cancelled_at,
	COALESCE(p.cancelled_by, ''), COALESCE(p.cancel_reason, '')`

const proformaFrom = `
	  FROM proforma_invoices p
	  JOIN subscribers s ON s.id = p.subscriber_id
	  JOIN gst_rates g   ON g.id = p.gst_rate_id`

func scanProforma(row pgx.Row) (*billing.Proforma, error) {
	var (
		p    billing.Proforma
		amts [8]string
	)
	if err := row.Scan(&p.ID, &p.SubscriberID, &p.Username, &p.MobileNumber, &p.RegisteredState, &p.Description,
		&amts[0], &amts[1], &amts[2], &amts[3], &amts[4], &amts[5], &amts[6], &amts[7],
		&p.ValidUntil, &p.Status, &p.CreatedBy, &p.CreatedAt, &p.InvoiceID,
		&p.ConvertedAt, &p.ConvertedBy, &p.CancelledAt, &p.CancelledBy, &p.CancelReason); err != nil {
		return nil, err
	}
	dst := []*decimal.Decimal{&p.Base, &p.CGST, &p.SGST, &p.IGST, &p.Total, &p.CGSTRate, &p.SGSTRate, &p.IGSTRate}
	for i, v := range amts {
		d, err := parseDecimal(v)
		if err != nil {
			return nil, err
		}
		*dst[i] = d
	}
	return &p, nil
}

// CreateProforma issues a proforma with GST estimated at the rate in force
// and the subscriber's registered state.
func (s *BillingStore) CreateProforma(ctx context.Context, req billing.ProformaRequest, homeState, by string) (*billing.Proforma, error) {
	req, err := req.Normalise()
	if err != nil {
		return nil, err
	}
	var state, status string
	err = s.pool.QueryRow(ctx, `SELECT registered_state, status FROM subscribers WHERE id = $1`,
		req.SubscriberID).Scan(&state, &status)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: proforma for subscriber %d: %w", req.SubscriberID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: proforma: load subscriber %d: %w", req.SubscriberID, err)
	}
	if status == "terminated" {
		return nil, fmt.Errorf("%w: this subscriber's account is terminated", billing.ErrProformaInvalid)
	}
	rate, err := s.GetActiveGstRate(ctx)
	if err != nil {
		return nil, err
	}
	inv := billing.CalculateGstInvoiceFrom(req.Base, state, homeState, rate)
	validUntil := billing.ProformaValidUntil(time.Now(), req.ValidDays)

	var id int
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO proforma_invoices (subscriber_id, description, base_amount, cgst_amount, sgst_amount,
		                               igst_amount, total_amount, gst_rate_id, valid_until, created_by)
		VALUES ($1, $2, $3::numeric, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $8, $9::date, $10)
		RETURNING id`,
		req.SubscriberID, req.Description, inv.BaseAmount.String(), inv.CgstAmount.String(),
		inv.SgstAmount.String(), inv.IgstAmount.String(), inv.TotalAmount.String(), rate.ID,
		validUntil.Format("2006-01-02"), by).Scan(&id); err != nil {
		return nil, fmt.Errorf("db: insert proforma: %w", err)
	}
	return s.GetProforma(ctx, id)
}

// GetProforma loads one proforma; billing.ErrProformaNotFound if none.
func (s *BillingStore) GetProforma(ctx context.Context, id int) (*billing.Proforma, error) {
	p, err := scanProforma(s.pool.QueryRow(ctx, `SELECT `+proformaColumns+proformaFrom+` WHERE p.id = $1`, id))
	if isNoRows(err) {
		return nil, billing.ErrProformaNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("db: get proforma %d: %w", id, err)
	}
	return p, nil
}

// ListProformas lists proformas newest first.
func (s *BillingStore) ListProformas(ctx context.Context, f billing.ProformaFilter) ([]billing.Proforma, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	rows, err := s.pool.Query(ctx, `SELECT `+proformaColumns+proformaFrom+`
		 WHERE ($1 = '' OR p.status = $1)
		   AND ($2 = 0 OR p.subscriber_id = $2)
		 ORDER BY p.created_at DESC, p.id DESC
		 LIMIT $3`, f.Status, f.SubscriberID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: list proformas: %w", err)
	}
	defer rows.Close()
	out := []billing.Proforma{}
	for rows.Next() {
		p, err := scanProforma(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan proforma: %w", err)
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ConvertProforma issues the tax invoice for an open proforma: the wallet
// is debited the invoice total — the tax part posted to GST Payable, as a
// renewal's is — and the invoice written with the proforma's description,
// all in one transaction.
//
// GST is recomputed at conversion, not copied from the proforma: the tax
// invoice is dated today and owes the rate in force today. The proforma's
// figures were an estimate and the document said so.
//
// The proforma and the subscriber are both locked, so a second conversion
// of the same proforma waits and then finds it converted, and a concurrent
// wallet debit cannot take the balance below what this one checked.
func (s *BillingStore) ConvertProforma(ctx context.Context, id int, homeState, by string) (*billing.ConvertedProforma, error) {
	var out *billing.ConvertedProforma
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			subscriberID        int
			franchiseID         *int
			status, desc, state string
			baseText, walletTxt string
			validUntil          time.Time
		)
		err := tx.QueryRow(ctx, `
			SELECT p.subscriber_id, s.franchise_id, p.status, p.description, s.registered_state,
			       p.base_amount::text, s.wallet_balance::text, p.valid_until
			  FROM proforma_invoices p
			  JOIN subscribers s ON s.id = p.subscriber_id
			 WHERE p.id = $1
			   FOR UPDATE OF p, s`, id).Scan(
			&subscriberID, &franchiseID, &status, &desc, &state, &baseText, &walletTxt, &validUntil)
		if isNoRows(err) {
			return billing.ErrProformaNotFound
		}
		if err != nil {
			return fmt.Errorf("db: convert proforma %d: load: %w", id, err)
		}
		if status != billing.ProformaOpen {
			return fmt.Errorf("%w: it is %s", billing.ErrProformaNotOpen, status)
		}
		if billing.ProformaExpired(validUntil, time.Now()) {
			return billing.ErrProformaExpired
		}
		base, err := parseDecimal(baseText)
		if err != nil {
			return err
		}
		balance, err := parseDecimal(walletTxt)
		if err != nil {
			return err
		}

		rate, err := activeGstRateTx(ctx, tx)
		if err != nil {
			return err
		}
		inv := billing.CalculateGstInvoiceFrom(base, state, homeState, rate)
		inv.SubscriberID = subscriberID
		if balance.LessThan(inv.TotalAmount) {
			return fmt.Errorf("%w: the wallet holds ₹%s and the invoice is ₹%s",
				billing.ErrInsufficientBalance, balance.StringFixed(2), inv.TotalAmount.StringFixed(2))
		}

		newBalance := balance.Sub(inv.TotalAmount)
		memo := fmt.Sprintf("proforma %s converted: %s", billing.ProformaNumber(id), desc)
		posting := billing.RechargePosting{
			SubscriberID: subscriberID,
			NewBalance:   newBalance,
			// The tax the invoice states, not re-derived: the GL's GST
			// Payable line must equal the document filed.
			TaxAmount: inv.CgstAmount.Add(inv.SgstAmount).Add(inv.IgstAmount),
			// The wallet leg goes in Credit with its real direction, as
			// WalletService.Post lays out every posting.
			Credit: billing.WalletEntry{
				SubscriberID: subscriberID, FranchiseID: franchiseID,
				Account: billing.AccountSubscriberWallet, EntryType: "debit",
				Amount: inv.TotalAmount, BalanceAfter: newBalance,
				Description: memo, AdjustedBy: by,
			},
			Debit: billing.WalletEntry{
				SubscriberID: subscriberID, FranchiseID: franchiseID,
				Account: billing.AccountRevenueClearing, EntryType: "credit",
				Amount: inv.TotalAmount, BalanceAfter: newBalance,
				Description: "counter-entry: " + memo, AdjustedBy: by,
			},
		}
		leg, err := recordPostingTx(ctx, tx, posting)
		if err != nil {
			return err
		}

		var invoiceID int
		if err := tx.QueryRow(ctx, `
			INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
			                      total_amount, gst_rate_id, gb_included, gb_used, description)
			VALUES ($1, $2::numeric, $3::numeric, $4::numeric, $5::numeric, $6::numeric, $7, 0, 0, $8)
			RETURNING id`,
			subscriberID, inv.BaseAmount.String(), inv.CgstAmount.String(), inv.SgstAmount.String(),
			inv.IgstAmount.String(), inv.TotalAmount.String(), rate.ID, desc).Scan(&invoiceID); err != nil {
			return fmt.Errorf("db: convert proforma %d: insert invoice: %w", id, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE proforma_invoices
			   SET status = 'converted', invoice_id = $2, converted_at = now(), converted_by = $3
			 WHERE id = $1`, id, invoiceID, by); err != nil {
			return fmt.Errorf("db: convert proforma %d: mark converted: %w", id, err)
		}
		out = &billing.ConvertedProforma{ProformaID: id, InvoiceID: invoiceID, Invoice: inv,
			WalletLedgerID: leg.ID, NewBalance: newBalance}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CancelProforma withdraws an open proforma, with a reason.
func (s *BillingStore) CancelProforma(ctx context.Context, id int, reason, by string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: give a reason for cancelling", billing.ErrProformaInvalid)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE proforma_invoices
		   SET status = 'cancelled', cancelled_at = now(), cancelled_by = $3, cancel_reason = $2
		 WHERE id = $1 AND status = 'open'`, id, reason, by)
	if err != nil {
		return fmt.Errorf("db: cancel proforma %d: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	// Nothing changed: either it does not exist or it is no longer open.
	if _, err := s.GetProforma(ctx, id); errors.Is(err, billing.ErrProformaNotFound) {
		return err
	}
	return billing.ErrProformaNotOpen
}

// activeGstRateTx is GetActiveGstRate inside a transaction.
func activeGstRateTx(ctx context.Context, tx pgx.Tx) (billing.GstRate, error) {
	var (
		rate             billing.GstRate
		cgst, sgst, igst string
	)
	err := tx.QueryRow(ctx, `
		SELECT id, cgst_rate::text, sgst_rate::text, igst_rate::text
		  FROM gst_rates WHERE effective_from <= NOW()
		 ORDER BY effective_from DESC LIMIT 1`).Scan(&rate.ID, &cgst, &sgst, &igst)
	if isNoRows(err) {
		return billing.GstRate{}, fmt.Errorf("db: no effective GST rate: %w", ErrNotFound)
	}
	if err != nil {
		return billing.GstRate{}, fmt.Errorf("db: get active GST rate: %w", err)
	}
	for _, p := range []struct {
		dst *decimal.Decimal
		v   string
	}{{&rate.CgstRate, cgst}, {&rate.SgstRate, sgst}, {&rate.IgstRate, igst}} {
		if *p.dst, err = parseDecimal(p.v); err != nil {
			return billing.GstRate{}, err
		}
	}
	return rate, nil
}
