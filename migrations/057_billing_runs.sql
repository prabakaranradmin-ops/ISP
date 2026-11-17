-- +goose Up
-- Recurring-billing run history — FR-BIL-020.
--
-- One row per pass of the auto-renewal scanner, and one per subscriber a
-- pass could not finish. Written by the scanner (radiusd) at the start and
-- end of each pass; read by billing staff.
CREATE TABLE billing_runs (
    id                   SERIAL        PRIMARY KEY,
    kind                 TEXT          NOT NULL,
    started_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    finished_at          TIMESTAMPTZ,
    status               TEXT          NOT NULL DEFAULT 'running',
    candidates           INTEGER       NOT NULL DEFAULT 0,
    renewed              INTEGER       NOT NULL DEFAULT 0,
    activated            INTEGER       NOT NULL DEFAULT 0,
    insufficient_balance INTEGER       NOT NULL DEFAULT 0,
    failed               INTEGER       NOT NULL DEFAULT 0,
    invoices_raised      INTEGER       NOT NULL DEFAULT 0,
    invoice_failures     INTEGER       NOT NULL DEFAULT 0,
    amount_charged       NUMERIC(14,2) NOT NULL DEFAULT 0,
    error                TEXT,

    CONSTRAINT chk_billing_run_status CHECK (status IN ('running', 'completed', 'failed', 'abandoned')),
    -- Only a running pass has no end; a failed one says why.
    CONSTRAINT chk_billing_run_finished CHECK ((status = 'running') = (finished_at IS NULL)),
    CONSTRAINT chk_billing_run_error CHECK (status <> 'failed' OR length(btrim(COALESCE(error, ''))) > 0),
    CONSTRAINT chk_billing_run_counts CHECK (
        candidates >= 0 AND renewed >= 0 AND activated >= 0 AND insufficient_balance >= 0
        AND failed >= 0 AND invoices_raised >= 0 AND invoice_failures >= 0 AND amount_charged >= 0
        AND renewed + activated + insufficient_balance + failed <= candidates
    )
);

CREATE INDEX idx_billing_runs_started ON billing_runs (kind, started_at DESC);

CREATE TABLE billing_run_failures (
    id            SERIAL      PRIMARY KEY,
    run_id        INTEGER     NOT NULL REFERENCES billing_runs(id) ON DELETE CASCADE,
    -- Kept if the subscriber row ever goes, so the history still says who.
    subscriber_id INTEGER     REFERENCES subscribers(id) ON DELETE SET NULL,
    username      TEXT        NOT NULL,
    stage         TEXT        NOT NULL,
    message       TEXT        NOT NULL,
    -- Whether the wallet had been debited when this step failed.
    charged       BOOLEAN     NOT NULL,
    CONSTRAINT chk_billing_run_failure_stage CHECK (stage IN
        ('price', 'debit', 'expiry', 'activation', 'invoice', 'dunning'))
);

CREATE INDEX idx_billing_run_failures_run ON billing_run_failures (run_id);

-- +goose Down
DROP TABLE IF EXISTS billing_run_failures;
DROP TABLE IF EXISTS billing_runs;
