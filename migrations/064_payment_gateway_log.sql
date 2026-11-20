-- +goose Up
-- Payment-gateway order and callback log — FR-PAY-001.
--
-- Money arriving from the gateway left almost no trace of its own. A
-- successful capture produced a wallet ledger entry, which is the money
-- but not the conversation: nothing recorded the order that was created,
-- the callbacks that were ignored, the ones rejected for a bad signature,
-- or the captured payment that arrived with no subscriber_id and credited
-- nobody. Those are exactly the cases a reconciliation asks about —
-- "Razorpay says they paid, we have nothing" — and the answer was a log
-- line that had rotated away.
--
-- The requirement is explicit that this is logged **whether or not it
-- credits a wallet**, so every exit path of the webhook writes a row,
-- including the ones that deliberately do nothing.
CREATE TABLE payment_gateway_log (
    id          BIGSERIAL   PRIMARY KEY,
    -- Which gateway. One today; a column rather than an assumption,
    -- because the second one is where a single-gateway schema hurts.
    gateway     TEXT        NOT NULL DEFAULT 'razorpay',
    -- 'order' for a payment link we asked the gateway to create,
    -- 'callback' for anything the gateway sent us.
    kind        TEXT        NOT NULL,
    -- The gateway's own event name for a callback ('payment.captured',
    -- 'payment.failed', ...), or 'order.create' for our own request.
    event       TEXT        NOT NULL DEFAULT '',
    -- The gateway's identifiers: the payment link / order id and the
    -- payment id. Both are what support quotes back at you.
    gateway_order_id   TEXT NOT NULL DEFAULT '',
    gateway_payment_id TEXT NOT NULL DEFAULT '',
    -- Our side of the pairing. Nullable because a callback that could not
    -- be matched to a subscriber is precisely the row worth keeping.
    subscriber_id INTEGER   REFERENCES subscribers(id) ON DELETE SET NULL,
    -- In rupees, as every other money column here. Nullable: a rejected
    -- callback may not have had a readable amount.
    amount      NUMERIC(12,2),
    -- What this system did about it, from a fixed vocabulary so the
    -- column can be grouped: created, credited, ignored, rejected,
    -- unmatched, failed, unconfigured. No 'duplicate': Recharge is
    -- idempotent and gives the webhook no way to tell a retried callback
    -- from a first one, so a repeat is recorded as credited (which it is)
    -- and shows as a second row sharing one payment id.
    status      TEXT        NOT NULL,
    -- A sentence for a human, when the status alone does not explain it.
    detail      TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_pgw_kind CHECK (kind IN ('order', 'callback')),
    CONSTRAINT chk_pgw_status CHECK (status IN (
        'created', 'credited', 'ignored',
        'rejected', 'unmatched', 'failed', 'unconfigured'
    )),
    CONSTRAINT chk_pgw_amount CHECK (amount IS NULL OR amount >= 0),
    CONSTRAINT chk_pgw_detail CHECK (length(detail) <= 500),
    CONSTRAINT chk_pgw_ids CHECK (
        length(gateway_order_id) <= 100 AND length(gateway_payment_id) <= 100
    )
);

-- The questions asked of this table: what happened recently, what
-- happened to this payment id (the one support quotes), and what
-- happened to this subscriber.
CREATE INDEX idx_pgw_recent ON payment_gateway_log (created_at DESC);
CREATE INDEX idx_pgw_payment ON payment_gateway_log (gateway_payment_id)
    WHERE gateway_payment_id <> '';
CREATE INDEX idx_pgw_order ON payment_gateway_log (gateway_order_id)
    WHERE gateway_order_id <> '';
CREATE INDEX idx_pgw_subscriber ON payment_gateway_log (subscriber_id, created_at DESC);
-- The reconciliation view: everything that did not end in a credit.
CREATE INDEX idx_pgw_problems ON payment_gateway_log (created_at DESC)
    WHERE status <> 'credited';

-- No signature, no secret, no raw body.
--
-- The webhook's authenticity is already decided before anything is
-- written, so keeping the signature would preserve a credential-shaped
-- value for no operational gain. The raw body is likewise omitted: it
-- carries whatever the gateway chose to send, which is not ours to
-- retain. There are no columns for either, so no later code path can add
-- one by accident.
COMMENT ON TABLE payment_gateway_log IS
    'FR-PAY-001: gateway orders and callbacks, credited or not; no signature or raw body stored';

-- Append-only against the application role, like the audit trail (062).
-- Rows here are evidence in a payment dispute.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'bss_app') THEN
        EXECUTE 'GRANT INSERT, SELECT ON payment_gateway_log TO bss_app';
        EXECUTE 'REVOKE UPDATE, DELETE ON payment_gateway_log FROM bss_app';
        EXECUTE 'GRANT USAGE, SELECT ON SEQUENCE payment_gateway_log_id_seq TO bss_app';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS payment_gateway_log;
