-- +goose Up
-- Scheduled plan changes — FR-BIL-023.
--
-- The existing plan change (FR-LC-001) takes effect the moment staff press
-- it, and prorates: the subscriber gets the new plan's full validity from
-- today plus bonus days converted from whatever value was left on the old
-- one. That is the right behaviour for "upgrade me now", and the wrong one
-- for "move me to the cheaper plan when this month runs out" — which is
-- most of what customers actually ask for. Prorating an end-of-cycle
-- downgrade hands out bonus days for value the customer has already
-- consumed, and it changes the rate limit mid-cycle on a plan they paid
-- for in full.
--
-- A scheduled change does neither. It waits, and the renewal path applies
-- it as part of buying the next cycle, so the cycle is priced, billed and
-- rate-limited as the new plan throughout. There is no proration because
-- there is nothing to prorate: the old cycle was used to the end.
CREATE TABLE scheduled_plan_changes (
    id            SERIAL      PRIMARY KEY,
    subscriber_id INTEGER     NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    -- The plan the subscriber is to end up on.
    new_plan_id   INTEGER     NOT NULL REFERENCES plans(id),
    -- The plan they were on when this was scheduled. Kept for the audit
    -- trail and so a list can read "Basic → Fibre 100" without guessing.
    --
    -- Deliberately NOT a precondition for applying: if an immediate change
    -- moves them somewhere else in the meantime, the intent recorded here
    -- is still "put them on new_plan_id", and refusing to honour that
    -- because the starting point moved would silently drop the
    -- instruction. See db.ApplyDuePlanChange.
    from_plan_id  INTEGER     NOT NULL REFERENCES plans(id),
    -- The date from which the change may be applied. The renewal path
    -- picks it up at the first renewal on or after this date — so a date
    -- set mid-cycle does not fire mid-cycle; it waits for the renewal,
    -- which is the whole point of scheduling it.
    effective_on  DATE        NOT NULL,
    -- 'scheduled' — waiting for a renewal on or after effective_on.
    -- 'applied'   — the renewal moved them. Terminal.
    -- 'cancelled' — withdrawn by staff before it fired. Terminal.
    status        TEXT        NOT NULL DEFAULT 'scheduled',
    reason        TEXT,
    scheduled_by  TEXT        NOT NULL,
    scheduled_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    applied_at    TIMESTAMPTZ,
    -- The invoice the first cycle on the new plan was billed on, so "what
    -- did this change cost them" is answerable from the row.
    applied_invoice_id INTEGER REFERENCES invoices(id),
    cancelled_at  TIMESTAMPTZ,
    cancelled_by  TEXT,
    cancel_reason TEXT,

    CONSTRAINT chk_spc_reason CHECK (reason IS NULL OR length(btrim(reason)) <= 200),
    CONSTRAINT chk_spc_different_plan CHECK (new_plan_id <> from_plan_id),
    -- Each status carries exactly the fields that describe it, the same
    -- shape as chk_proforma_status (056) and chk_sub_product_status (058).
    CONSTRAINT chk_spc_status CHECK (
        (status = 'scheduled'
            AND applied_at IS NULL AND cancelled_at IS NULL)
     OR (status = 'applied'
            AND applied_at IS NOT NULL AND cancelled_at IS NULL)
     OR (status = 'cancelled'
            AND applied_at IS NULL AND applied_invoice_id IS NULL
            AND cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL
            AND length(btrim(COALESCE(cancel_reason, ''))) > 0)
    )
);

-- One pending change per subscriber.
--
-- A queue of them would be ambiguous about which wins at the next renewal,
-- and an operator correcting a mistake would be stacking a second
-- instruction on top of the first rather than replacing it. Scheduling
-- again requires cancelling what is there, which is visible in the
-- history; the partial index makes that a database rule rather than a
-- convention the API happens to follow.
CREATE UNIQUE INDEX uq_spc_one_pending
    ON scheduled_plan_changes (subscriber_id) WHERE status = 'scheduled';

-- The renewal path's lookup: is anything due for this subscriber today.
CREATE INDEX idx_spc_due
    ON scheduled_plan_changes (subscriber_id, effective_on) WHERE status = 'scheduled';
CREATE INDEX idx_spc_history
    ON scheduled_plan_changes (subscriber_id, scheduled_at DESC);

-- The renewal path gains a step that can fail, so the run record's stage
-- vocabulary has to admit it (migration 057, FR-BIL-020).
--
-- Without this the CHECK rejects the row and the pass's own failure
-- recording fails — silently, since FR-BIL-020 deliberately logs a
-- recording failure and carries on rather than holding up renewals. The
-- result would be the one failure that most needs reporting (charged at
-- the new plan's price, still on the old plan) being the one the Billing
-- Runs screen never shows. Nothing in Go would have caught it: the stage
-- is a string either side of the wire.
ALTER TABLE billing_run_failures DROP CONSTRAINT IF EXISTS chk_billing_run_failure_stage;
ALTER TABLE billing_run_failures ADD CONSTRAINT chk_billing_run_failure_stage CHECK (stage IN
    ('price', 'debit', 'plan_change', 'expiry', 'activation', 'invoice', 'dunning'));

-- +goose Down
ALTER TABLE billing_run_failures DROP CONSTRAINT IF EXISTS chk_billing_run_failure_stage;
ALTER TABLE billing_run_failures ADD CONSTRAINT chk_billing_run_failure_stage CHECK (stage IN
    ('price', 'debit', 'expiry', 'activation', 'invoice', 'dunning'));
DROP TABLE IF EXISTS scheduled_plan_changes;
