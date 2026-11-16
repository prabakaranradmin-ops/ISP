package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// BounceCheque marks a cleared cheque payment bounced and takes its value
// back out of the subscriber's wallet, in one transaction (FR-BIL-014).
//
// The reversal is the payment's mirror image through the same posting path:
// subscriber wallet (1200) debited, Cash / Bank (1000) credited, so the
// ledger stops claiming money the bank never paid.
//
// If the subscriber has already spent the credit — typically on the renewal
// the cheque paid for — the wallet cannot cover the reversal and
// chk_wallet_balance_nonneg forbids going below zero. That is refused with
// billing.ErrInsufficientBalance and nothing is written: recording the
// bounce without reversing it would leave the money in the books, and a
// partial reversal would hide the shortfall. What to do about a spent,
// bounced cheque (suspend, record dues, collect) is a business decision the
// operator makes; the console says so.
func (s *BillingStore) BounceCheque(ctx context.Context, walletLedgerID int, reason, by string) (*billing.Transaction, error) {
	var out *billing.Transaction
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			method, status, amountStr, balanceStr string
			subscriberID                          int
			franchiseID                           *int
			chequeNo                              *string
		)
		err := tx.QueryRow(ctx, `
			SELECT p.method, p.status, p.cheque_number, w.amount::text, w.subscriber_id,
			       s.franchise_id, s.wallet_balance::text
			  FROM payment_details p
			  JOIN wallet_ledgers w ON w.id = p.wallet_ledger_id
			  JOIN subscribers s ON s.id = w.subscriber_id
			 WHERE p.wallet_ledger_id = $1
			   FOR UPDATE OF p, s`, walletLedgerID).Scan(
			&method, &status, &chequeNo, &amountStr, &subscriberID, &franchiseID, &balanceStr)
		if isNoRows(err) {
			return fmt.Errorf("db: bounce cheque: payment %d: %w", walletLedgerID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("db: bounce cheque: load payment %d: %w", walletLedgerID, err)
		}
		if method != billing.MethodCheque || status != "cleared" {
			return billing.ErrNotBounceable
		}
		amount, err := parseDecimal(amountStr)
		if err != nil {
			return err
		}
		balance, err := parseDecimal(balanceStr)
		if err != nil {
			return err
		}
		newBalance := balance.Sub(amount)
		if newBalance.IsNegative() {
			return fmt.Errorf("%w: the wallet holds %s of the %s cheque; the rest has been spent",
				billing.ErrInsufficientBalance, balance.StringFixed(2), amount.StringFixed(2))
		}

		cheque := ""
		if chequeNo != nil {
			cheque = *chequeNo
		}
		desc := fmt.Sprintf("cheque %s bounced: %s", cheque, reason)
		leg, err := recordPostingTx(ctx, tx, billing.RechargePosting{
			SubscriberID: subscriberID,
			NewBalance:   newBalance,
			// Credit holds the wallet leg (here a debit), Debit the counter
			// leg (here a credit) — the same convention WalletService.Post
			// uses for a debit posting.
			Credit: billing.WalletEntry{
				SubscriberID: subscriberID, FranchiseID: franchiseID,
				Account: billing.AccountSubscriberWallet, EntryType: "debit",
				Amount: amount, BalanceAfter: newBalance, Description: desc, AdjustedBy: by,
			},
			Debit: billing.WalletEntry{
				SubscriberID: subscriberID, FranchiseID: franchiseID,
				Account: billing.AccountGatewayClearing, EntryType: "credit",
				Amount: amount, BalanceAfter: newBalance, Description: "counter-entry: " + desc, AdjustedBy: by,
			},
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE payment_details
			   SET status = 'bounced', bounced_at = now(), bounce_reason = $2, bounce_ledger_id = $3
			 WHERE wallet_ledger_id = $1`, walletLedgerID, reason, leg.ID); err != nil {
			return fmt.Errorf("db: bounce cheque: mark bounced: %w", err)
		}
		out = leg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
