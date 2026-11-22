-- +goose Up
-- A shared vendor master for inventory and procurement — FR-INV-004.
--
-- Three columns held a vendor as free text before this: cpe_device_types.vendor
-- (the model's supplier), cpe_purchases.vendor (who a batch was actually bought
-- from) and purchase_orders.vendor (who a general order is with). Nothing tied
-- them together, so "Chennai Networks", "chennai networks" and "Chennai
-- Networks " were three different suppliers to every report, and a GSTIN — the
-- thing that decides whether input tax credit can be claimed on a purchase
-- invoice — had nowhere to live at all.
--
-- name is VARCHAR(200), not 100: purchase_orders.vendor is already 200 while
-- the two cpe_* columns are 100. Sized to the widest source so the backfill
-- below cannot truncate a name on its way into the master — a narrower column
-- would fail this migration on the one deployment that has a long vendor name,
-- and a migration that fails on real data is the worst outcome available here.
CREATE TABLE IF NOT EXISTS vendors (
    id             SERIAL          PRIMARY KEY,
    name           VARCHAR(200)    NOT NULL,
    contact_person VARCHAR(200),
    -- Deliberately NOT the E.164 constraint migration 020 puts on subscribers,
    -- franchises and leads. That rule exists because those numbers are dialled
    -- and texted by the notification pipeline, and a malformed one fails at the
    -- worst moment. Nothing in this system ever contacts a vendor, so the only
    -- reader is a human with the phone in their hand — and a supplier's number
    -- is routinely a landline with an extension ("044-2812 3456 x14"), which
    -- E.164 cannot express. Constraining it would refuse correct data to serve
    -- a purpose this column does not have.
    phone          VARCHAR(30),
    email          VARCHAR(255),
    -- Same format as chk_subscriber_gstin_format (migration 038), character for
    -- character, and that is the point: a GSTIN is a GSTIN whether it belongs
    -- to a customer or a supplier, and two tables disagreeing on what one looks
    -- like is a defect waiting for whichever table is written to second. The
    -- checksum built into the last character is not checked here — it is
    -- checked in internal/vendors, where a failure can be explained to the
    -- person typing it rather than surfacing as a constraint violation.
    gstin          VARCHAR(15),
    address        TEXT,
    notes          TEXT,
    -- Vendors are retired, never deleted: see the Down section's note and
    -- internal/db/vendors.go. A deactivated vendor keeps every purchase that
    -- was made from it and simply stops appearing in the pickers.
    active         BOOLEAN         NOT NULL DEFAULT TRUE,
    -- Who entered and last changed this row, as subscriber_groups records it
    -- (migration 069). Kept here specifically because of the GSTIN: a wrong
    -- registration number is a tax exposure, and "who entered this" is the
    -- first question asked about one. Nullable because the backfill below has
    -- no honest answer — those rows were assembled from free text nobody
    -- signed.
    created_by     VARCHAR(100),
    updated_by     VARCHAR(100),
    created_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_vendor_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT chk_vendor_gstin_format CHECK (
        gstin IS NULL OR gstin ~ '^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z]Z[0-9A-Z]$'
    )
);

CREATE TRIGGER trg_vendors_updated_at
    BEFORE UPDATE ON vendors
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The whole point of a master is that one supplier is one row, so the name is
-- unique case-insensitively and with internal whitespace collapsed — the exact
-- normalisation uq_subscriber_group_name_active uses (migration 069), for the
-- same reason: btrim alone would let "Anna  Nagar Cables" and "Anna Nagar
-- Cables" both go live, which is the duplication this table exists to end.
-- Partial on active so a retired vendor's name can be reused by whoever takes
-- over that territory.
CREATE UNIQUE INDEX IF NOT EXISTS uq_vendors_name_active
    ON vendors (lower(regexp_replace(btrim(name), '\s+', ' ', 'g')))
    WHERE active;

-- A GSTIN is one legal entity, so two active vendors cannot share one. Partial
-- on active for the same reason as the name, and on NOT NULL because most
-- vendors are entered before anyone digs out their registration number.
CREATE UNIQUE INDEX IF NOT EXISTS uq_vendors_gstin_active
    ON vendors (gstin)
    WHERE active AND gstin IS NOT NULL;

-- ── The three referencing columns ────────────────────────────────────────────
--
-- Nullable, not NOT NULL. The backfill below can only link a row whose free
-- text names something; a row with a blank vendor (storable before this
-- migration — the columns were NOT NULL but never checked for emptiness) has
-- nothing to point at, and a vendor with no name is not a vendor. Making the
-- column NOT NULL would mean this migration fails on exactly the deployments
-- with the messiest data, which are the ones that need it most. The write
-- paths require a vendor instead; the schema only refuses to invent one.
--
-- ON DELETE RESTRICT, not SET NULL: a purchase whose vendor was deleted is a
-- payment to nobody, and silently blanking the link would destroy the only
-- record of who was paid. Nothing in the application deletes a vendor at all —
-- this is the backstop for a hand-written DELETE.
ALTER TABLE cpe_device_types
    ADD COLUMN IF NOT EXISTS vendor_id INTEGER REFERENCES vendors(id) ON DELETE RESTRICT;
ALTER TABLE cpe_purchases
    ADD COLUMN IF NOT EXISTS vendor_id INTEGER REFERENCES vendors(id) ON DELETE RESTRICT;
ALTER TABLE purchase_orders
    ADD COLUMN IF NOT EXISTS vendor_id INTEGER REFERENCES vendors(id) ON DELETE RESTRICT;

-- The original free text stays in the vendor columns. It is no longer read for
-- display — internal/db COALESCEs the master's current name over it, so fixing
-- a misspelling or a GSTIN fixes it everywhere, which is the requirement — but
-- it is kept because the backfill below is a *guess*. If it merges two names
-- that turn out to be different suppliers, the text is the only evidence left
-- of what each row originally said, and without it the merge is unrecoverable.
-- Going forward it holds the master's name as it stood when the row was
-- written, so the column has one meaning throughout: the vendor name of record
-- at creation time.
COMMENT ON COLUMN cpe_device_types.vendor IS
    'Vendor name of record when this row was written. Display reads vendors.name via vendor_id. Kept as the pre-FR-INV-004 evidence trail (migration 071).';
COMMENT ON COLUMN cpe_purchases.vendor IS
    'Vendor name of record when this row was written. Display reads vendors.name via vendor_id. Kept as the pre-FR-INV-004 evidence trail (migration 071).';
COMMENT ON COLUMN purchase_orders.vendor IS
    'Vendor name of record when this row was written. Display reads vendors.name via vendor_id. Kept as the pre-FR-INV-004 evidence trail (migration 071).';

-- ── Backfill ─────────────────────────────────────────────────────────────────
--
-- One master row per distinct vendor name across all three tables, matched
-- case-insensitively with internal whitespace collapsed. That is as far as the
-- merging goes, and deliberately so: punctuation-insensitive matching would
-- also fold "Sri Balaji Cables" into "Sri Balaji, Cables Division", and a
-- wrong merge is far worse than a surviving duplicate. A duplicate is a
-- cleanup task somebody can see and fix; a wrong merge silently attributes one
-- company's spend — and one company's input tax credit — to another, and
-- nobody looking at the master would know to doubt it.
--
-- The stored name is the collapsed-whitespace form of one of the source
-- spellings, and which one is chosen by sort order so the result cannot depend
-- on physical row order.
--
-- The two boolean sort keys pick the spelling a person capitalised
-- deliberately: first one that is not entirely lower case, then one that is
-- not entirely upper case. Given "Chennai Networks", "chennai networks" and
-- "CHENNAI NETWORKS", that is the first. Without them the plain name sort wins
-- and the master opens showing "chennai networks" — which is not wrong, but
-- reads as though the import mangled the data, and the first instinct on
-- seeing it would be to distrust the whole table.
INSERT INTO vendors (name)
SELECT DISTINCT ON (norm) spelling
FROM (
    SELECT lower(regexp_replace(btrim(vendor), '\s+', ' ', 'g')) AS norm,
           regexp_replace(btrim(vendor), '\s+', ' ', 'g')        AS spelling
    FROM (
        SELECT vendor FROM cpe_device_types
        UNION ALL SELECT vendor FROM cpe_purchases
        UNION ALL SELECT vendor FROM purchase_orders
    ) raw
    WHERE btrim(vendor) <> ''
) src
ORDER BY norm, (spelling <> lower(spelling)) DESC, (spelling <> upper(spelling)) DESC, spelling
ON CONFLICT DO NOTHING;

-- Link each row to the master entry its text normalises to. Re-runnable: the
-- WHERE vendor_id IS NULL clause means a second pass (after a hand-inserted
-- vendor, say) only fills what is still unlinked, and never relinks a row an
-- operator has since corrected by hand.
WITH keyed AS (
    SELECT id, lower(regexp_replace(btrim(name), '\s+', ' ', 'g')) AS norm FROM vendors
)
UPDATE cpe_device_types t SET vendor_id = k.id
FROM keyed k
WHERE t.vendor_id IS NULL
  AND k.norm = lower(regexp_replace(btrim(t.vendor), '\s+', ' ', 'g'));

WITH keyed AS (
    SELECT id, lower(regexp_replace(btrim(name), '\s+', ' ', 'g')) AS norm FROM vendors
)
UPDATE cpe_purchases p SET vendor_id = k.id
FROM keyed k
WHERE p.vendor_id IS NULL
  AND k.norm = lower(regexp_replace(btrim(p.vendor), '\s+', ' ', 'g'));

WITH keyed AS (
    SELECT id, lower(regexp_replace(btrim(name), '\s+', ' ', 'g')) AS norm FROM vendors
)
UPDATE purchase_orders o SET vendor_id = k.id
FROM keyed k
WHERE o.vendor_id IS NULL
  AND k.norm = lower(regexp_replace(btrim(o.vendor), '\s+', ' ', 'g'));

-- Indexed because every vendor retirement and every usage count reads these,
-- and because an unindexed referencing column makes Postgres seq-scan all
-- three tables to check ON DELETE RESTRICT.
CREATE INDEX IF NOT EXISTS idx_cpe_device_types_vendor ON cpe_device_types (vendor_id) WHERE vendor_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_cpe_purchases_vendor    ON cpe_purchases (vendor_id)    WHERE vendor_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_purchase_orders_vendor  ON purchase_orders (vendor_id)  WHERE vendor_id IS NOT NULL;

-- +goose Down
-- The free-text columns were never dropped, so down is a clean reversal: the
-- vendor names every row displayed before this migration are still in them.
-- Master data an operator entered after the migration (contacts, GSTINs) is
-- lost, which is unavoidable — there is nowhere in the old schema to put it.
DROP INDEX IF EXISTS idx_purchase_orders_vendor;
DROP INDEX IF EXISTS idx_cpe_purchases_vendor;
DROP INDEX IF EXISTS idx_cpe_device_types_vendor;

COMMENT ON COLUMN purchase_orders.vendor IS NULL;
COMMENT ON COLUMN cpe_purchases.vendor IS NULL;
COMMENT ON COLUMN cpe_device_types.vendor IS NULL;

ALTER TABLE purchase_orders   DROP COLUMN IF EXISTS vendor_id;
ALTER TABLE cpe_purchases     DROP COLUMN IF EXISTS vendor_id;
ALTER TABLE cpe_device_types  DROP COLUMN IF EXISTS vendor_id;

DROP TRIGGER IF EXISTS trg_vendors_updated_at ON vendors;
DROP INDEX IF EXISTS uq_vendors_gstin_active;
DROP INDEX IF EXISTS uq_vendors_name_active;
DROP TABLE IF EXISTS vendors;
