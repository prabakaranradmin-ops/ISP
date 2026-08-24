-- +goose Up
-- General procurement (CRD-EXP-007) — a purchase order for anything, not
-- just CPE restocking. internal/inventory.Purchase already records "N units
-- of a device type bought from a vendor" as an immediate, unapproved write;
-- this is deliberately a different shape (a request with its own approval
-- step and a status lifecycle) rather than that table widened, because a
-- purchase order can name goods or services with no device_type_id at all
-- (office rent, a contractor invoice) and must not execute until approved.
--
-- Not wired to any ledger: CRD-EXP-007 depends on CRD-EXP-006 (general
-- ledger/accounts management) for a received order to post an
-- accounts-payable entry automatically, and that does not exist yet. This
-- table only tracks the request/approval/fulfilment lifecycle on its own —
-- sequenced deliberately ahead of the ledger work, not in place of it.
--
-- A bespoke status/approval pair rather than reusing approval_requests
-- (migration 026): that table's subscriber_id is NOT NULL and its
-- action_type CHECK only permits wallet_credit/refund/terminate — both are
-- correct for subscriber-affecting actions and wrong for a purchase order,
-- which has no subscriber at all.
CREATE TABLE IF NOT EXISTS purchase_orders (
    id                SERIAL          PRIMARY KEY,
    description       TEXT            NOT NULL,
    vendor            VARCHAR(200)    NOT NULL,
    category          VARCHAR(20)     NOT NULL DEFAULT 'other'
                          CHECK (category IN ('hardware', 'services', 'other')),
    amount            NUMERIC(12,2)   NOT NULL CHECK (amount >= 0),
    status            VARCHAR(20)     NOT NULL DEFAULT 'requested'
                          CHECK (status IN ('requested', 'approved', 'rejected', 'ordered', 'received', 'cancelled')),
    requested_by      VARCHAR(100)    NOT NULL,
    decided_by        VARCHAR(100),
    decision_reason   TEXT,
    created_at        TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    decided_at        TIMESTAMPTZ,
    received_at       TIMESTAMPTZ,

    -- Same guarantee as chk_approval_distinct_approver (migration 026): the
    -- person who asked for the spend can never be the one who signs off on
    -- it, enforced at the schema regardless of which code path got here.
    CONSTRAINT chk_po_distinct_approver
        CHECK (decided_by IS NULL OR decided_by <> requested_by)
);

CREATE INDEX IF NOT EXISTS idx_purchase_orders_status ON purchase_orders (status);

-- +goose Down
DROP TABLE IF EXISTS purchase_orders;
