-- +goose Up
-- General ledger, Phase 2 (CRD-EXP-006) — auto-posting from wallet
-- recharges/adjustments, franchise commission, and received purchase
-- orders. Two accounts the Phase 1 starter chart deliberately left out
-- because nothing posted to them yet:
--
--   - Wallet Adjustments & Refunds (expense): staff-issued wallet credits,
--     debits and refunds (AccountAdjustmentClearing in internal/billing/
--     wallet.go) get their own account rather than blending into Operating
--     Expenses, so a P&L reader can tell a customer goodwill credit from an
--     office rent payment.
--   - Commission Payable to Partners (liability): what the ISP owes its
--     franchise partners for earned-but-unpaid commission, distinct from
--     Accounts Payable's trade payables (vendors, purchase orders).
INSERT INTO chart_of_accounts (code, name, account_type, normal_balance) VALUES
    ('5200', 'Wallet Adjustments & Refunds',    'expense',   'debit'),
    ('2100', 'Commission Payable to Partners',  'liability', 'credit')
ON CONFLICT (code) DO NOTHING;

-- lco_ledger has no idempotency guard today because nothing ever wrote to it
-- outside a test — CalculateLCOCommission was dead code until this phase
-- wired it into the live recharge path (internal/revenue/franchise.go's
-- SettleCommissionForRecharge). A recharge can be retried with the same
-- transaction_token (WalletService.Recharge is itself idempotent on it via
-- idx_wallet_token), and without this index a retry would double-post both
-- the commission ledger row and its GL entry. Partial rather than a plain
-- unique index for the same reason idx_wallet_token is partial: a
-- token-less entry (no transaction ref available) must not collide with
-- another token-less entry.
CREATE UNIQUE INDEX IF NOT EXISTS idx_lco_ledger_txn_ref
    ON lco_ledger (transaction_ref) WHERE transaction_ref IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_lco_ledger_txn_ref;
DELETE FROM chart_of_accounts WHERE code IN ('5200', '2100');
