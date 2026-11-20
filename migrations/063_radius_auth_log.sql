-- +goose Up
-- RADIUS authentication attempt log — FR-NET-004.
--
-- Nothing recorded why an authentication failed. A subscriber who could
-- not get online produced a rejected packet, a counter increment and a log
-- line that rotated away; "it was working yesterday" was answerable only
-- by watching it happen again. This is the table that answers it: when,
-- who, from which NAS and MAC, accepted or rejected, and if rejected, why.
--
-- **The password is never stored, in any form.** The reference product's
-- equivalent log keeps the supplied password so staff can "see what the
-- customer typed", which turns an operational log into a credential dump
-- — readable by anyone with report access, and a breach of every
-- expectation a subscriber has. There is no column for it here, so no
-- future code path can add one by accident.
CREATE TABLE radius_auth_log (
    id         BIGSERIAL   PRIMARY KEY,
    -- As supplied in User-Name. Not a foreign key to subscribers: the
    -- most interesting rows are the ones naming a username that does not
    -- exist, which is what a typo and a credential-stuffing attempt both
    -- look like.
    username   TEXT        NOT NULL,
    nas_ip     INET,
    -- Calling-Station-Id, normally the subscriber's MAC. Free text
    -- because vendors format it differently and this records what
    -- arrived, not a tidied version of it.
    mac        TEXT        NOT NULL DEFAULT '',
    -- 'accept' or 'reject'.
    result     TEXT        NOT NULL,
    -- Why it was rejected, from a fixed vocabulary so the column can be
    -- grouped and counted. Empty on an accept.
    reject_reason TEXT     NOT NULL DEFAULT '',
    -- Which method answered: pap, eap or mab. A subscriber failing on one
    -- and succeeding on another is a specific and common misconfiguration.
    auth_method TEXT       NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_auth_log_result CHECK (result IN ('accept', 'reject')),
    CONSTRAINT chk_auth_log_reason CHECK (
        (result = 'accept' AND reject_reason = '')
     OR (result = 'reject' AND length(btrim(reject_reason)) > 0)
    ),
    CONSTRAINT chk_auth_log_reason_vocab CHECK (reject_reason IN (
        '', 'unknown_user', 'bad_password', 'not_authorised', 'locked_out',
        'no_mab_device', 'eap_failed', 'internal_error'
    )),
    CONSTRAINT chk_auth_log_method CHECK (auth_method IN ('', 'pap', 'eap', 'mab')),
    CONSTRAINT chk_auth_log_username CHECK (length(username) <= 253)
);

-- The three questions asked of this table: what happened recently, what
-- happened to this subscriber, and what is failing and why.
CREATE INDEX idx_auth_log_recent ON radius_auth_log (created_at DESC);
CREATE INDEX idx_auth_log_username ON radius_auth_log (username, created_at DESC);
-- Partial, because the rejects are what anybody actually reads and they
-- are the minority of rows on a healthy network.
CREATE INDEX idx_auth_log_rejects ON radius_auth_log (created_at DESC)
    WHERE result = 'reject';

-- Bounded retention, as FR-NET-004 requires.
--
-- A row per authentication is high-volume by nature — NFR-PERF-001 sizes
-- the daemon at 5,000 requests a second — so this table cannot be allowed
-- to grow without limit. 30 days is long enough to investigate a
-- complaint ("it stopped working last week") and short enough that the
-- table stays an operational tool rather than an archive. The purge runs
-- in cmd/radiusd; this default is where the number lives.
--
-- Deliberately not partitioned, unlike cgnat_allocations: that table is
-- kept for years for law-enforcement lookups, where dropping a partition
-- is the only sane way to expire data. Thirty days of a bounded log is a
-- DELETE on an indexed timestamp.
COMMENT ON TABLE radius_auth_log IS
    'FR-NET-004: authentication attempts, 30-day retention, password never stored';

-- Append-only against the application role, and purgeable by nobody but
-- the purge itself — which runs as the same role, so DELETE is granted
-- here unlike staff_audit_log (062). UPDATE is not: an attempt as it was
-- recorded is not something to revise.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'bss_app') THEN
        EXECUTE 'GRANT INSERT, SELECT, DELETE ON radius_auth_log TO bss_app';
        EXECUTE 'REVOKE UPDATE ON radius_auth_log FROM bss_app';
        EXECUTE 'GRANT USAGE, SELECT ON SEQUENCE radius_auth_log_id_seq TO bss_app';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS radius_auth_log;
