-- +goose Up
-- Product catalogue and invoice lines — FR-BIL-022.
--
-- Until now an invoice billed exactly one thing: the subscriber's plan, as a
-- single base_amount with the tax computed on it. An ISP sells more than
-- that — installation, router rental, a static IP, add-ons — and GST
-- requires each of those to appear as its own line with its own HSN/SAC
-- code, because they are different supplies taxed under different headings.
--
-- Three tables:
--   products          the catalogue: what may be sold, and for how much
--   subscriber_products  what a given subscriber has been sold
--   invoice_lines     what one invoice actually charged, line by line
--
-- The invoice header keeps its base/cgst/sgst/igst/total columns and stays
-- the single source for GSTR-1, the ledger and every existing report. What
-- changes is that those columns are now the sum of the lines rather than
-- tax computed once on one amount. trg_invoice_lines_total below enforces
-- that equality at commit time, so no code path can write a header that
-- disagrees with its own lines.

-- ── The catalogue ───────────────────────────────────────────────────────────
CREATE TABLE products (
    id          SERIAL        PRIMARY KEY,
    -- Operator-facing short code (INSTALL, ROUTER-RENT, STATIC-IP). Unique
    -- so an operator can refer to a charge by code in conversation and on
    -- an import, and stable so renaming the product does not break that.
    code        TEXT          NOT NULL UNIQUE,
    name        TEXT          NOT NULL,
    description TEXT          NOT NULL DEFAULT '',
    -- 'one_time' is charged on the next invoice and then done with;
    -- 'recurring' is charged on every renewal until it is ended.
    kind        TEXT          NOT NULL,
    -- GST-exclusive, exactly like plans.price: tax is added on top when the
    -- line is billed, at the rate in force that day.
    price       NUMERIC(12,2) NOT NULL,
    -- Required, not defaulted: a router is goods and bandwidth is a
    -- service, and they do not share a code. Defaulting this would print a
    -- plausible-looking wrong code on a tax invoice, which is worse than
    -- refusing to create the product.
    hsn_sac     TEXT          NOT NULL,
    -- Deactivated products stay here and stay attached to the subscribers
    -- who already have them; they simply cannot be attached to anyone new.
    -- Same rule as FR-CAT-001 for plans.
    active      BOOLEAN       NOT NULL DEFAULT TRUE,
    created_by  TEXT          NOT NULL,
    created_at  TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_by  TEXT,
    updated_at  TIMESTAMPTZ,

    CONSTRAINT chk_product_code CHECK (code = upper(btrim(code)) AND code ~ '^[A-Z0-9][A-Z0-9-]{1,31}$'),
    CONSTRAINT chk_product_name CHECK (length(btrim(name)) BETWEEN 1 AND 120),
    CONSTRAINT chk_product_description CHECK (length(description) <= 500),
    CONSTRAINT chk_product_kind CHECK (kind IN ('one_time', 'recurring')),
    -- Positive, not merely non-negative: a zero-price line is noise on a
    -- tax invoice. Waiving a charge means not attaching it.
    CONSTRAINT chk_product_price CHECK (price > 0 AND price <= 1000000),
    CONSTRAINT chk_product_hsn_sac CHECK (hsn_sac ~ '^[0-9]{4,8}$')
);

CREATE INDEX idx_products_active ON products (active, kind, code);

-- ── What a subscriber has been sold ─────────────────────────────────────────
CREATE TABLE subscriber_products (
    id            SERIAL      PRIMARY KEY,
    subscriber_id INTEGER     NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    product_id    INTEGER     NOT NULL REFERENCES products(id),
    quantity      INTEGER     NOT NULL DEFAULT 1,
    -- The price this subscriber pays, copied from the catalogue when the
    -- product was attached (or overridden then by staff who may).
    --
    -- A snapshot, deliberately, not a join to products.price: raising the
    -- catalogue price of router rental must not silently reprice every
    -- subscriber who already rents one, and an invoice issued last month
    -- has to stay explicable from the rows that produced it.
    unit_price    NUMERIC(12,2) NOT NULL,
    -- 'active'  — recurring: billed every renewal; one_time: awaiting its
    --             first and only invoice.
    -- 'charged' — a one_time charge that has been invoiced. Terminal.
    -- 'ended'   — a recurring charge stopped by staff. Terminal.
    status        TEXT        NOT NULL DEFAULT 'active',
    attached_by   TEXT        NOT NULL,
    attached_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at      TIMESTAMPTZ,
    ended_by      TEXT,
    end_reason    TEXT,
    -- The invoice a one_time charge landed on, so "have we billed this
    -- yet?" is answerable from the row itself rather than inferred.
    invoice_id    INTEGER     REFERENCES invoices(id),
    charged_at    TIMESTAMPTZ,

    CONSTRAINT chk_sub_product_quantity CHECK (quantity BETWEEN 1 AND 999),
    CONSTRAINT chk_sub_product_price CHECK (unit_price > 0 AND unit_price <= 1000000),
    -- Each status carries exactly the fields that describe it, so a charge
    -- marked 'charged' with no invoice behind it, or an 'ended' attachment
    -- with no reason, cannot be written by any path. Same shape as
    -- chk_proforma_status in migration 056.
    CONSTRAINT chk_sub_product_status CHECK (
        (status = 'active'
            AND invoice_id IS NULL AND charged_at IS NULL AND ended_at IS NULL)
     OR (status = 'charged'
            AND invoice_id IS NOT NULL AND charged_at IS NOT NULL AND ended_at IS NULL)
     OR (status = 'ended'
            AND charged_at IS NULL AND invoice_id IS NULL
            AND ended_at IS NOT NULL AND ended_by IS NOT NULL
            AND length(btrim(COALESCE(end_reason, ''))) > 0)
    )
);

CREATE INDEX idx_sub_products_subscriber ON subscriber_products (subscriber_id, status);
-- The renewal path's lookup: every charge still owed by one subscriber.
CREATE INDEX idx_sub_products_billable ON subscriber_products (subscriber_id) WHERE status = 'active';
CREATE INDEX idx_sub_products_product ON subscriber_products (product_id);

-- ── What an invoice charged, line by line ───────────────────────────────────
CREATE TABLE invoice_lines (
    id          SERIAL        PRIMARY KEY,
    invoice_id  INTEGER       NOT NULL REFERENCES invoices(id) ON DELETE CASCADE,
    -- 1-based position as printed. Unique per invoice so the document has a
    -- stable order independent of insertion or of id allocation.
    line_no     INTEGER       NOT NULL,
    -- 'plan' is the subscriber's own plan for the cycle; 'product' is a
    -- catalogue item; 'one_off' is an ad-hoc charge (a converted proforma).
    kind        TEXT          NOT NULL,
    product_id  INTEGER       REFERENCES products(id),
    description TEXT          NOT NULL,
    hsn_sac     TEXT          NOT NULL,
    quantity    INTEGER       NOT NULL DEFAULT 1,
    unit_price  NUMERIC(12,2) NOT NULL,
    base_amount NUMERIC(12,2) NOT NULL,
    cgst_amount NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    sgst_amount NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    igst_amount NUMERIC(12,2) NOT NULL DEFAULT 0.00,
    total_amount NUMERIC(12,2) NOT NULL,

    CONSTRAINT uq_invoice_line_no UNIQUE (invoice_id, line_no),
    CONSTRAINT chk_invoice_line_no CHECK (line_no >= 1),
    CONSTRAINT chk_invoice_line_kind CHECK (kind IN ('plan', 'product', 'one_off')),
    -- A product line names its product; a plan or ad-hoc line does not.
    CONSTRAINT chk_invoice_line_product CHECK (
        (kind = 'product' AND product_id IS NOT NULL) OR
        (kind <> 'product' AND product_id IS NULL)
    ),
    CONSTRAINT chk_invoice_line_description CHECK (length(btrim(description)) BETWEEN 1 AND 200),
    CONSTRAINT chk_invoice_line_hsn_sac CHECK (hsn_sac ~ '^[0-9]{4,8}$'),
    CONSTRAINT chk_invoice_line_quantity CHECK (quantity BETWEEN 1 AND 999),
    CONSTRAINT chk_invoice_line_amounts CHECK (
        unit_price > 0
        AND base_amount = round(unit_price * quantity, 2)
        AND cgst_amount >= 0 AND sgst_amount >= 0 AND igst_amount >= 0
        AND total_amount = base_amount + cgst_amount + sgst_amount + igst_amount
    ),
    -- Same rule as invoices.chk_gst_logic, applied per line: one tax
    -- regime or the other, never both.
    CONSTRAINT chk_invoice_line_gst_logic CHECK (
        (cgst_amount > 0 AND igst_amount = 0) OR
        (igst_amount > 0 AND cgst_amount = 0) OR
        (cgst_amount = 0 AND igst_amount = 0)
    )
);

CREATE INDEX idx_invoice_lines_invoice ON invoice_lines (invoice_id, line_no);

-- The header must equal its own lines, to the paisa.
--
-- This is the whole correctness claim of per-line GST: the document the
-- customer reads adds up to the header that GSTR-1, the general ledger and
-- the wallet debit all use. Rounding per line and summing is what GST
-- expects when lines carry different HSN/SAC codes, but it means the header
-- can no longer be re-derived by taxing one total — so it is checked
-- instead.
--
-- DEFERRABLE INITIALLY DEFERRED because the lines are inserted one at a
-- time after the header: the sums only agree once the transaction is
-- complete, which is exactly when a deferred constraint trigger runs.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION check_invoice_lines_total() RETURNS TRIGGER AS $$
DECLARE
    hdr  invoices;
    base NUMERIC(12,2);
    cgst NUMERIC(12,2);
    sgst NUMERIC(12,2);
    igst NUMERIC(12,2);
    tot  NUMERIC(12,2);
    n    INTEGER;
BEGIN
    SELECT * INTO hdr FROM invoices WHERE id = NEW.invoice_id;
    IF NOT FOUND THEN
        -- The invoice went away inside this transaction (ON DELETE CASCADE
        -- took the lines with it); there is nothing left to reconcile.
        RETURN NULL;
    END IF;

    SELECT COUNT(*),
           COALESCE(SUM(l.base_amount), 0), COALESCE(SUM(l.cgst_amount), 0),
           COALESCE(SUM(l.sgst_amount), 0), COALESCE(SUM(l.igst_amount), 0),
           COALESCE(SUM(l.total_amount), 0)
      INTO n, base, cgst, sgst, igst, tot
      FROM invoice_lines l WHERE l.invoice_id = NEW.invoice_id;

    IF n = 0 THEN
        RETURN NULL;
    END IF;

    IF base <> hdr.base_amount OR cgst <> hdr.cgst_amount
       OR sgst <> hdr.sgst_amount OR igst <> hdr.igst_amount
       OR tot <> hdr.total_amount THEN
        RAISE EXCEPTION 'invoice % lines total (base %, cgst %, sgst %, igst %, total %) do not match the invoice header (base %, cgst %, sgst %, igst %, total %)',
            NEW.invoice_id, base, cgst, sgst, igst, tot,
            hdr.base_amount, hdr.cgst_amount, hdr.sgst_amount, hdr.igst_amount, hdr.total_amount;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER trg_invoice_lines_total
    AFTER INSERT OR UPDATE ON invoice_lines
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION check_invoice_lines_total();

-- +goose Down
DROP TRIGGER IF EXISTS trg_invoice_lines_total ON invoice_lines;
DROP FUNCTION IF EXISTS check_invoice_lines_total();
DROP TABLE IF EXISTS invoice_lines;
DROP TABLE IF EXISTS subscriber_products;
DROP TABLE IF EXISTS products;
