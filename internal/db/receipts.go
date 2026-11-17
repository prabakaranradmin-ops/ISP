package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Payment receipts — FR-BIL-021.
//
// A payment is what the Payments list counts (ConsoleStore.ListPayments):
// a credit leg on a subscriber's wallet that no staff member posted as an
// adjustment. Credit notes, refunds and adjustments credit the wallet too,
// but nobody paid anything, so they get no receipt.

const receiptQuery = `
	SELECT w.id, w.subscriber_id, s.username, COALESCE(s.mobile_number, ''), COALESCE(s.caf_number, ''),
	       w.created_at, w.amount::text, w.balance_after::text,
	       COALESCE(p.method, regexp_replace(COALESCE(w.description, ''), '^recharge via ', '')),
	       COALESCE(p.reference, w.transaction_token, ''),
	       COALESCE(p.cheque_number, ''), COALESCE(p.cheque_bank, ''), p.cheque_date,
	       COALESCE(p.tds_certificate, ''), COALESCE(p.status, 'cleared'),
	       p.bounced_at, COALESCE(p.bounce_reason, ''), COALESCE(p.recorded_by, '')
	  FROM wallet_ledgers w
	  JOIN subscribers s ON s.id = w.subscriber_id
	  LEFT JOIN payment_details p ON p.wallet_ledger_id = w.id
	 WHERE w.account = 'subscriber_wallet'
	   AND w.entry_type = 'credit'
	   AND w.adjusted_by_username IS NULL`

func scanReceipt(row pgx.Row) (*billing.PaymentReceipt, error) {
	var (
		r               billing.PaymentReceipt
		amount, balance string
	)
	if err := row.Scan(&r.PaymentID, &r.SubscriberID, &r.Username, &r.MobileNumber, &r.CAFNumber,
		&r.ReceivedAt, &amount, &balance, &r.Method, &r.Reference, &r.ChequeNumber, &r.ChequeBank,
		&r.ChequeDate, &r.TDSCertificate, &r.Status, &r.BouncedAt, &r.BounceReason, &r.RecordedBy); err != nil {
		return nil, err
	}
	var err error
	if r.Amount, err = parseDecimal(amount); err != nil {
		return nil, err
	}
	if r.BalanceAfter, err = parseDecimal(balance); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetPaymentReceipt loads one payment for its receipt; nil if the id is not
// a payment (no such ledger row, or a credit that was not a payment).
func (s *BillingStore) GetPaymentReceipt(ctx context.Context, paymentID int) (*billing.PaymentReceipt, error) {
	r, err := scanReceipt(s.pool.QueryRow(ctx, receiptQuery+` AND w.id = $1`, paymentID))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: payment receipt %d: %w", paymentID, err)
	}
	return r, nil
}

// ListSubscriberPayments lists one subscriber's payments, newest first, for
// their own portal.
func (s *BillingStore) ListSubscriberPayments(ctx context.Context, subscriberID, limit int) ([]billing.PaymentReceipt, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, receiptQuery+` AND w.subscriber_id = $1 ORDER BY w.created_at DESC, w.id DESC LIMIT $2`,
		subscriberID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list payments for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()
	out := []billing.PaymentReceipt{}
	for rows.Next() {
		r, err := scanReceipt(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan payment: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
