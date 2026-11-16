-- +goose Up
-- How each payment was made — FR-BIL-013 (method), FR-BIL-014 (cheque
-- bounce), FR-BIL-015 (TDS).
--
-- Until now the method lived only in wallet_ledgers.description ("recharge
-- via cash"), free text nothing could filter, total or reconcile against a
-- bank statement. A detail row per payment, keyed by the wallet credit leg it
-- describes, carries it as data. wallet_ledgers itself is left alone: it is
-- the subledger every reconciliation reads, and widening it for fields only
-- a payment has would put cheque columns on every renewal debit.

CREATE TABLE payment_details (
    wallet_ledger_id INTEGER     PRIMARY KEY REFERENCES wallet_ledgers(id) ON DELETE RESTRICT,
    method           VARCHAR(20) NOT NULL
                     CHECK (method IN ('online','cash','cheque','bank_transfer','upi','tds')),
    -- A gateway payment id, bank UTR or UPI reference. Free text: each
    -- channel formats its own.
    reference        VARCHAR(100),
    cheque_number    VARCHAR(20),
    cheque_bank      VARCHAR(100),
    cheque_date      DATE,
    tds_certificate  VARCHAR(50),
    status           VARCHAR(12) NOT NULL DEFAULT 'cleared' CHECK (status IN ('cleared','bounced')),
    bounced_at       TIMESTAMPTZ,
    bounce_reason    TEXT,
    -- The wallet debit that reversed a bounced cheque.
    bounce_ledger_id INTEGER     REFERENCES wallet_ledgers(id),
    recorded_by      VARCHAR(100),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- A cheque is only traceable with its number, bank and date; a TDS
    -- credit only with its certificate (Form 16A) number.
    CONSTRAINT chk_payment_cheque_fields CHECK (
        method <> 'cheque' OR (cheque_number IS NOT NULL AND cheque_bank IS NOT NULL AND cheque_date IS NOT NULL)),
    CONSTRAINT chk_payment_tds_fields CHECK (method <> 'tds' OR tds_certificate IS NOT NULL),
    -- Only a cheque can bounce, and a bounce is only recorded with the
    -- reversal that undid it — never a status flip that left the money in.
    CONSTRAINT chk_payment_bounce_only_cheque CHECK (status = 'cleared' OR method = 'cheque'),
    CONSTRAINT chk_payment_bounce_has_reversal CHECK (
        (status = 'bounced') = (bounce_ledger_id IS NOT NULL AND bounced_at IS NOT NULL))
);

CREATE INDEX idx_payment_details_method ON payment_details (method);

-- TDS is not cash received: the customer paid less and the tax department
-- owes the difference until the certificate is claimed. It needs its own
-- asset account rather than Cash / Bank (1000), and a wallet counter
-- account that maps to it.
INSERT INTO chart_of_accounts (code, name, account_type, normal_balance)
VALUES ('1300', 'TDS Receivable', 'asset', 'debit')
ON CONFLICT (code) DO NOTHING;

ALTER TABLE wallet_ledgers DROP CONSTRAINT IF EXISTS chk_wallet_account;
ALTER TABLE wallet_ledgers ADD CONSTRAINT chk_wallet_account
    CHECK (account IN ('subscriber_wallet', 'payment_gateway_clearing',
                        'revenue_clearing', 'adjustment_clearing', 'tds_receivable'));

-- Backfill the method for payments already taken, from the description the
-- recharge paths have always written. Anything unrecognised is left without
-- a detail row rather than guessed at; the console falls back to showing the
-- description for those.
INSERT INTO payment_details (wallet_ledger_id, method, reference, created_at)
SELECT w.id,
       CASE
         WHEN w.description ILIKE 'recharge via razorpay%' THEN 'online'
         WHEN w.description ILIKE 'recharge via cash%'     THEN 'cash'
         WHEN w.description ILIKE 'recharge via upi%'      THEN 'upi'
         WHEN w.description ILIKE 'recharge via bank%'     THEN 'bank_transfer'
       END,
       w.transaction_token, w.created_at
  FROM wallet_ledgers w
 WHERE w.account = 'subscriber_wallet' AND w.entry_type = 'credit'
   AND w.adjusted_by_username IS NULL
   AND (w.description ILIKE 'recharge via razorpay%' OR w.description ILIKE 'recharge via cash%'
        OR w.description ILIKE 'recharge via upi%' OR w.description ILIKE 'recharge via bank%');

-- +goose Down
DROP TABLE IF EXISTS payment_details;
-- Nothing may remain in an account the restored constraint does not allow;
-- a TDS posting cannot be silently re-labelled, so the rollback refuses.
ALTER TABLE wallet_ledgers DROP CONSTRAINT IF EXISTS chk_wallet_account;
ALTER TABLE wallet_ledgers ADD CONSTRAINT chk_wallet_account
    CHECK (account IN ('subscriber_wallet', 'payment_gateway_clearing',
                        'revenue_clearing', 'adjustment_clearing'));
DELETE FROM chart_of_accounts WHERE code = '1300'
   AND NOT EXISTS (SELECT 1 FROM gl_journal_lines l WHERE l.account_id = chart_of_accounts.id);
