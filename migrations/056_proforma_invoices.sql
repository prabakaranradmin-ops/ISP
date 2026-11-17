-- +goose Up
-- Proforma invoices — FR-BIL-018.
--
-- A quotation with GST estimated, in its own table and number series
-- (PF-000001...). Tax invoice numbers are derived from invoices.id
-- (billing.InvoiceNumber) and GST requires that series to run without gaps,
-- so a proforma must never take a row there; keeping it out of invoices
-- also keeps it out of GSTR-1, which reads only that table.
CREATE TABLE proforma_invoices (
    id            SERIAL        PRIMARY KEY,
    subscriber_id INTEGER       NOT NULL REFERENCES subscribers(id),
    description   TEXT          NOT NULL,
    base_amount   NUMERIC(12,2) NOT NULL,
    cgst_amount   NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    sgst_amount   NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    igst_amount   NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    total_amount  NUMERIC(12,2) NOT NULL,
    gst_rate_id   INTEGER       NOT NULL REFERENCES gst_rates(id),
    valid_until   DATE          NOT NULL,
    status        TEXT          NOT NULL DEFAULT 'open',
    created_by    TEXT          NOT NULL,
    created_at    TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    -- The tax invoice it became. UNIQUE: one proforma, one tax invoice.
    invoice_id    INTEGER       UNIQUE REFERENCES invoices(id),
    converted_at  TIMESTAMPTZ,
    converted_by  TEXT,
    cancelled_at  TIMESTAMPTZ,
    cancelled_by  TEXT,
    cancel_reason TEXT,

    CONSTRAINT chk_proforma_description CHECK (length(btrim(description)) BETWEEN 1 AND 200),
    CONSTRAINT chk_proforma_amounts CHECK (
        base_amount > 0 AND cgst_amount >= 0 AND sgst_amount >= 0 AND igst_amount >= 0
        AND total_amount = base_amount + cgst_amount + sgst_amount + igst_amount
    ),
    -- Same rule as invoices.chk_gst_logic: one tax regime or the other.
    CONSTRAINT chk_proforma_gst_logic CHECK (
        (cgst_amount > 0 AND igst_amount = 0) OR
        (igst_amount > 0 AND cgst_amount = 0) OR
        (cgst_amount = 0 AND igst_amount = 0)
    ),
    -- Each status carries exactly the fields that describe it, so a
    -- "converted" proforma with no invoice, or a cancellation with no
    -- reason, cannot be written by any path.
    CONSTRAINT chk_proforma_status CHECK (
        (status = 'open'
            AND invoice_id IS NULL AND converted_at IS NULL AND cancelled_at IS NULL)
     OR (status = 'converted'
            AND invoice_id IS NOT NULL AND converted_at IS NOT NULL AND converted_by IS NOT NULL
            AND cancelled_at IS NULL)
     OR (status = 'cancelled'
            AND invoice_id IS NULL AND converted_at IS NULL
            AND cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL
            AND length(btrim(COALESCE(cancel_reason, ''))) > 0)
    )
);

CREATE INDEX idx_proforma_subscriber ON proforma_invoices (subscriber_id, created_at DESC);
CREATE INDEX idx_proforma_status ON proforma_invoices (status, created_at DESC);

-- The line a one-off charge's tax invoice prints instead of the plan name.
-- NULL for every renewal invoice, which is every invoice before this.
ALTER TABLE invoices ADD COLUMN description TEXT;

-- +goose Down
ALTER TABLE invoices DROP COLUMN IF EXISTS description;
DROP TABLE IF EXISTS proforma_invoices;
