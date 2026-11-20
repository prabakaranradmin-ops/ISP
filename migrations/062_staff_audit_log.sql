-- +goose Up
-- Append-only staff audit log — FR-OBS-006.
--
-- middleware.Audit has always written a structured log line for every
-- staff action: who, what, which target, with detail. That line goes to
-- the process log, which rotates, is not queryable, and on the native
-- install is a file nobody reads. "Who changed this subscriber's plan
-- last March" was therefore answerable only by grepping whatever log
-- happened to survive.
--
-- This persists the same entries. The log line is still emitted, and
-- remains the authority if a row cannot be written: persistence is
-- best-effort by design, because an audit write must never be able to
-- fail the action it is recording.
CREATE TABLE staff_audit_log (
    id         BIGSERIAL   PRIMARY KEY,
    -- The same correlation id the log line carries, so a row here and a
    -- line in the log can be tied together.
    correlation_id TEXT    NOT NULL,
    -- Who acted. The console session's username, or "system:<scanner>"
    -- for the background passes that set a subject (dunning, auto-renewal)
    -- — those are audited too, and attributing them to "unknown" is what
    -- migration 031 exists to avoid.
    actor_id   TEXT        NOT NULL,
    actor_role TEXT        NOT NULL DEFAULT '',
    -- What was done, in middleware.Audit's dotted form: subscriber.plan_change,
    -- product.attached, static_ip.released.
    action     TEXT        NOT NULL,
    -- What it was done to, as the caller names it: a subscriber id, an
    -- invoice number, a product code. Free text because the callers'
    -- targets are not one kind of thing.
    target     TEXT        NOT NULL DEFAULT '',
    -- The caller's detail map. JSONB rather than text so a question like
    -- "which plan changes moved somebody to plan 7" is answerable.
    detail     JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_audit_action CHECK (length(btrim(action)) BETWEEN 1 AND 100),
    CONSTRAINT chk_audit_actor CHECK (length(btrim(actor_id)) BETWEEN 1 AND 100),
    CONSTRAINT chk_audit_target CHECK (length(target) <= 200)
);

-- The three filters FR-OBS-006 asks for. created_at leads each of them
-- because every view of this table is "recently, by ...".
CREATE INDEX idx_audit_recent ON staff_audit_log (created_at DESC);
CREATE INDEX idx_audit_actor  ON staff_audit_log (actor_id, created_at DESC);
CREATE INDEX idx_audit_action ON staff_audit_log (action, created_at DESC);

-- Append-only, enforced the way migration 019 established for
-- lea_audit_log rather than by convention.
--
-- The difference from lea_audit_log is SELECT: FR-OBS-006 requires the
-- owner to browse this, so the application must read it. What it must
-- never do is change or remove a row — an audit log the application can
-- rewrite is not evidence of anything.
--
-- REVOKE is what actually bites, and it bites before RLS is evaluated:
-- bss_app is neither the table owner nor a superuser, so an UPDATE or
-- DELETE fails with "permission denied for table". The RLS policy below
-- is defence-in-depth, so that a future accidental re-grant is still
-- refused by RLS's default-deny.
--
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'bss_app') THEN
        EXECUTE 'GRANT INSERT, SELECT ON staff_audit_log TO bss_app';
        EXECUTE 'REVOKE UPDATE, DELETE ON staff_audit_log FROM bss_app';
        EXECUTE 'GRANT USAGE, SELECT ON SEQUENCE staff_audit_log_id_seq TO bss_app';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE staff_audit_log ENABLE ROW LEVEL SECURITY;

-- INSERT and SELECT policies only. No UPDATE or DELETE policy exists, and
-- RLS denies by default what it has no policy for.
CREATE POLICY staff_audit_insert_only ON staff_audit_log FOR INSERT WITH CHECK (true);
CREATE POLICY staff_audit_read ON staff_audit_log FOR SELECT USING (true);

-- +goose Down
DROP POLICY IF EXISTS staff_audit_read ON staff_audit_log;
DROP POLICY IF EXISTS staff_audit_insert_only ON staff_audit_log;
DROP TABLE IF EXISTS staff_audit_log;
