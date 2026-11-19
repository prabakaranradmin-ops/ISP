-- +goose Up
-- Plan editing and retirement — FR-CAT-001.
--
-- Plans could be created and never changed. A tariff that needed its price
-- corrected, or an old one that should stop being sold, had to be edited
-- with SQL against production — and because nothing marked a plan as no
-- longer offered, every plan ever created stayed in every dropdown
-- forever, including the ones nobody may be sold any more.
--
-- Retiring a plan hides it from new assignment and leaves the subscribers
-- already on it exactly where they are: they keep renewing on it at its
-- price, because that is the tariff they bought. This is the rule the
-- product catalogue (058) and the IP pools (060) both copied, and it is
-- stated here because this is where it started.
ALTER TABLE plans ADD COLUMN active BOOLEAN NOT NULL DEFAULT TRUE;

-- Attribution for an edit. created_at was the only timestamp a plan had,
-- so a price that changed left nothing behind saying when or by whom —
-- which is the first question asked when a subscriber's bill changes.
ALTER TABLE plans ADD COLUMN updated_by TEXT;
ALTER TABLE plans ADD COLUMN updated_at TIMESTAMPTZ;

-- Plan names are how operators refer to a tariff in conversation and on an
-- invoice, so two plans called "Fibre 100" is a support call waiting to
-- happen. Case-insensitive, and only among the plans still offered: a
-- retired "Fibre 100" and its replacement may legitimately share a name,
-- which is in fact the usual way a tariff is re-priced — retire the old
-- one, create the new one, leave existing subscribers on the old.
CREATE UNIQUE INDEX uq_plan_name_active
    ON plans (lower(btrim(name))) WHERE active;

-- The dropdowns' query: the plans that may still be sold.
CREATE INDEX idx_plans_active ON plans (active, name);

-- +goose Down
DROP INDEX IF EXISTS idx_plans_active;
DROP INDEX IF EXISTS uq_plan_name_active;
ALTER TABLE plans DROP COLUMN IF EXISTS updated_at;
ALTER TABLE plans DROP COLUMN IF EXISTS updated_by;
ALTER TABLE plans DROP COLUMN IF EXISTS active;
