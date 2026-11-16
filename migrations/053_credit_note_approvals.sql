-- +goose Up
-- Credit-note issuance through the approval workflow (FR-BIL-012).
--
-- A credit note moves money: it reverses part of an invoice, returns that
-- amount to the subscriber's wallet, and reverses the revenue and output GST
-- the original charge posted. That makes it a staff money credit, and every
-- staff money credit already needs a second approver (FR-WFL-001) — without
-- this, issuing a credit note would be a way round the maker-checker rule
-- that a plain wallet credit cannot take. So it is an approval request kind,
-- not a direct action.
--
-- amount on a credit_note request is the taxable value to credit; the tax is
-- derived from the invoice's own heads and rates when it executes, so the
-- document always matches the invoice it corrects.

ALTER TABLE approval_requests
    ADD COLUMN IF NOT EXISTS invoice_id INTEGER REFERENCES invoices(id) ON DELETE RESTRICT;

ALTER TABLE approval_requests DROP CONSTRAINT IF EXISTS approval_requests_action_type_check;
ALTER TABLE approval_requests ADD CONSTRAINT approval_requests_action_type_check
    CHECK (action_type IN ('wallet_credit','refund','terminate','credit_note'));

-- An invoice id belongs on a credit-note request and nowhere else: a wallet
-- credit "against an invoice" would read like a credit note on screen while
-- issuing no GST document at all.
ALTER TABLE approval_requests ADD CONSTRAINT chk_approval_invoice_by_type CHECK (
    (action_type = 'credit_note' AND invoice_id IS NOT NULL) OR
    (action_type <> 'credit_note' AND invoice_id IS NULL)
);

-- Credit notes link back to the request that authorised them and the wallet
-- leg that returned the money, so an auditor can walk from the GST document
-- to both people involved and to the ledger.
ALTER TABLE credit_notes
    ADD COLUMN IF NOT EXISTS approval_request_id INTEGER REFERENCES approval_requests(id),
    ADD COLUMN IF NOT EXISTS wallet_ledger_id    INTEGER REFERENCES wallet_ledgers(id);

-- +goose Down
ALTER TABLE credit_notes DROP COLUMN IF EXISTS wallet_ledger_id;
ALTER TABLE credit_notes DROP COLUMN IF EXISTS approval_request_id;
ALTER TABLE approval_requests DROP CONSTRAINT IF EXISTS chk_approval_invoice_by_type;
-- Nothing may remain in a kind the restored constraint does not allow.
DELETE FROM approval_requests WHERE action_type = 'credit_note';
ALTER TABLE approval_requests DROP CONSTRAINT IF EXISTS approval_requests_action_type_check;
ALTER TABLE approval_requests ADD CONSTRAINT approval_requests_action_type_check
    CHECK (action_type IN ('wallet_credit','refund','terminate'));
ALTER TABLE approval_requests DROP COLUMN IF EXISTS invoice_id;
