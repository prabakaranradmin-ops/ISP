-- +goose Up
-- Credit notes (FR-BIL-006, GSTR-1 Table 9B).
--
-- Two renewals were invoiced at the tax-inclusive total and collected at the
-- pre-tax price, before 5c06f2d fixed the charge. The supply really happened
-- at what was collected, so the invoice has to come down to meet it, and a
-- GST invoice is not edited after issue — it is credited. Without a document
-- the return would declare tax on money that never moved.
CREATE TABLE credit_notes (
    id            SERIAL PRIMARY KEY,
    invoice_id    INTEGER       NOT NULL REFERENCES invoices(id)    ON DELETE RESTRICT,
    subscriber_id INTEGER       NOT NULL REFERENCES subscribers(id) ON DELETE RESTRICT,
    base_amount   NUMERIC(12,2) NOT NULL,
    cgst_amount   NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    sgst_amount   NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    igst_amount   NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    total_amount  NUMERIC(12,2) NOT NULL,
    reason        TEXT          NOT NULL,
    created_at    TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    -- The document must add up, for the same reason a journal entry must
    -- balance: a return built from rows that do not is wrong in a way no
    -- later reconciliation can find.
    CONSTRAINT chk_credit_note_total
        CHECK (total_amount = base_amount + cgst_amount + sgst_amount + igst_amount),
    CONSTRAINT chk_credit_note_positive
        CHECK (base_amount >= 0 AND total_amount > 0),
    -- CGST+SGST or IGST, never both, matching invoices' own rule: a supply is
    -- either intrastate or interstate.
    CONSTRAINT chk_credit_note_gst_head
        CHECK ((cgst_amount = 0 AND sgst_amount = 0) OR igst_amount = 0)
);

CREATE INDEX idx_credit_notes_invoice ON credit_notes (invoice_id);
CREATE INDEX idx_credit_notes_period  ON credit_notes (created_at);

-- Credit notes against one invoice may not exceed it. Enforced as a trigger
-- because the rule spans two tables, which a CHECK cannot see. Without it a
-- typo produces a negative net supply, which the GST portal rejects at filing
-- time, long after the mistake.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION check_credit_note_within_invoice() RETURNS TRIGGER AS $$
DECLARE
    invoice_total NUMERIC(12,2);
    credited      NUMERIC(12,2);
BEGIN
    SELECT i.total_amount INTO invoice_total FROM invoices i WHERE i.id = NEW.invoice_id;
    SELECT COALESCE(SUM(c.total_amount), 0) INTO credited
      FROM credit_notes c WHERE c.invoice_id = NEW.invoice_id AND c.id <> NEW.id;
    IF credited + NEW.total_amount > invoice_total THEN
        RAISE EXCEPTION 'credit notes for invoice % would total %, exceeding the invoice value %',
            NEW.invoice_id, credited + NEW.total_amount, invoice_total;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_credit_note_within_invoice
    BEFORE INSERT OR UPDATE ON credit_notes
    FOR EACH ROW EXECUTE FUNCTION check_credit_note_within_invoice();

-- ── The correction itself ────────────────────────────────────────────────
--
-- Matched on the defect's signature rather than on invoice ids, so a fresh
-- install (where those ids mean different invoices, or none) is untouched:
-- an invoice carrying tax, whose own auto-renewal wallet debit was the
-- pre-tax base instead of the total, within a minute of the invoice.
--
-- The arithmetic, for anyone auditing it. 599.00 was collected against an
-- invoice of 599.00 + 53.91 + 53.91 = 706.82. Treating what was collected as
-- tax-inclusive at 9% + 9%, the revised supply is the base where
-- base + round(base*9%,2) + round(base*9%,2) = 599.00 exactly, which is
-- 507.62 + 45.69 + 45.69. Note 599.00/1.18 rounds to 507.63, which totals
-- 599.01 — a paisa out, because CalculateGstInvoiceFrom rounds each head
-- independently rather than dividing the total. The credit note is the
-- difference: 91.38 base, 8.22 CGST, 8.22 SGST, 107.82 total.
INSERT INTO credit_notes (invoice_id, subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount, total_amount, reason)
SELECT i.id, i.subscriber_id,
       i.base_amount  - 507.62,
       i.cgst_amount  - 45.69,
       i.sgst_amount  - 45.69,
       0.00,
       i.total_amount - 599.00,
       'Under-collection before 5c06f2d: charged the pre-tax price against a '
       || 'tax-inclusive invoice. Supply revised to the amount actually collected.'
  FROM invoices i
 WHERE i.igst_amount = 0
   AND i.base_amount = 599.00
   AND i.cgst_amount = 53.91
   AND i.sgst_amount = 53.91
   AND i.total_amount = 706.82
   AND EXISTS (
        SELECT 1 FROM wallet_ledgers w
         WHERE w.subscriber_id = i.subscriber_id
           AND w.account       = 'subscriber_wallet'
           AND w.entry_type    = 'debit'
           AND w.description LIKE 'auto-renewal:%'
           AND w.amount        = i.base_amount          -- the defect: charged base, invoiced total
           AND w.created_at BETWEEN i.created_at - INTERVAL '1 minute'
                                AND i.created_at + INTERVAL '1 minute')
   AND NOT EXISTS (SELECT 1 FROM credit_notes c WHERE c.invoice_id = i.id);

-- The matching general-ledger correction.
--
-- Those renewals posted their whole 599.00 to 4000 Subscription Revenue,
-- because 2200 did not exist yet. No cash moves here and no wallet is
-- touched: the money was already taken and is already on the books. This
-- only reclassifies the part of it that is tax, so the ledger says what the
-- revised invoice says.
-- The amount reclassified is derived from the documents, never hardcoded: it
-- is the tax remaining on the revised supply, i.e. the invoice's tax less the
-- tax credited back. Deriving it means the ledger cannot disagree with the
-- credit note even if the matched set ever differs from the two rows here.
WITH corrected AS (
    SELECT c.id AS credit_note_id, c.invoice_id,
           (i.cgst_amount - c.cgst_amount)
         + (i.sgst_amount - c.sgst_amount)
         + (i.igst_amount - c.igst_amount) AS revised_tax
      FROM credit_notes c
      JOIN invoices i ON i.id = c.invoice_id
     WHERE c.reason LIKE 'Under-collection before 5c06f2d%'
), entry AS (
    INSERT INTO gl_journal_entries (description, source_type, source_id, created_by)
    SELECT 'reclassify GST collected in renewal invoice ' || corrected.invoice_id,
           'credit_note', corrected.credit_note_id, 'migration:049'
      FROM corrected
    RETURNING id, source_id
)
INSERT INTO gl_journal_lines (journal_entry_id, account_id, debit, credit)
SELECT e.id, a.id,
       CASE WHEN l.code = '4000' THEN c.revised_tax ELSE 0.00 END,
       CASE WHEN l.code = '2200' THEN c.revised_tax ELSE 0.00 END
  FROM entry e
  JOIN corrected c ON c.credit_note_id = e.source_id
  CROSS JOIN (VALUES ('4000'), ('2200')) AS l(code)
  JOIN chart_of_accounts a ON a.code = l.code;

-- +goose Down
DELETE FROM gl_journal_lines
 WHERE journal_entry_id IN (SELECT id FROM gl_journal_entries WHERE created_by = 'migration:049');
DELETE FROM gl_journal_entries WHERE created_by = 'migration:049';
DROP TRIGGER IF EXISTS trg_credit_note_within_invoice ON credit_notes;
DROP FUNCTION IF EXISTS check_credit_note_within_invoice();
DROP TABLE IF EXISTS credit_notes;
