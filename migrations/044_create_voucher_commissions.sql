-- +goose Up
-- MAC-voucher reseller settlement (CRD-EXP-010, re-scoped 2026-08-24). A
-- voucher batch could already be attributed to a franchise partner
-- (hotspot_vouchers.franchise_id, migration 034) but redemption never
-- credited that partner anything, and a voucher had no price to credit a
-- commission against in the first place.
--
-- This deliberately does NOT extend lco_ledger the way a subscription
-- recharge's commission does (CalculateAndStoreLCOCommission): that table's
-- subscriber_id is NOT NULL, and a voucher grant has no subscriber by
-- design (chk_grant_has_exactly_one_source, migration 034) — relaxing that
-- constraint on a live financial ledger table already read by
-- GetFranchisePnL/ListConsolidatedPnL risked those two, already-shipped
-- reports for comparatively little benefit over a dedicated table. A
-- franchise partner's total commission is therefore the sum of two ledgers
-- (subscription recharges + voucher sales) rather than one — surfaced as
-- two figures in the console, not silently merged.

-- A voucher's own sale price. Nullable-by-default via 0, not literally
-- nullable: a pre-existing voucher (issued before this column existed) or
-- one deliberately given away free both mean "no commission is owed on
-- this", which 0 expresses without a NULL needing special-casing in the
-- commission arithmetic below.
ALTER TABLE hotspot_vouchers
    ADD COLUMN IF NOT EXISTS sale_amount NUMERIC(10,2) NOT NULL DEFAULT 0
        CHECK (sale_amount >= 0);

CREATE TABLE IF NOT EXISTS voucher_commissions (
    id                  SERIAL          PRIMARY KEY,
    franchise_id        INTEGER         NOT NULL REFERENCES franchises(id),
    voucher_id          INTEGER         NOT NULL UNIQUE REFERENCES hotspot_vouchers(id),
    sale_amount         NUMERIC(10,2)   NOT NULL,
    commission_rate_pct NUMERIC(5,2)    NOT NULL,
    commission_amount   NUMERIC(10,2)   NOT NULL,
    created_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_voucher_commissions_franchise ON voucher_commissions (franchise_id);

-- +goose Down
DROP TABLE IF EXISTS voucher_commissions;
ALTER TABLE hotspot_vouchers DROP COLUMN IF EXISTS sale_amount;
