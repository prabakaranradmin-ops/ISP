-- +goose Up
-- Session DB-036 | DDS §5.9 | IDD §8.4
--
-- Live RADIUS session state, replacing the Redis-backed cache.SessionStore
-- used when this stack ran as multiple Docker services. The health endpoint
-- and the subscriber portal's live-usage panel both read "is this subscriber
-- online right now, and how much have they used", which is worthless once
-- the session ends — exactly what made it a Redis cache rather than a
-- PostgreSQL table originally. On a single-machine native install there is
-- no separate cache tier to put this in, so it becomes a small table instead,
-- with updated_at doing the job a Redis TTL did: a row older than
-- live_session_ttl (30 minutes, matching the old SessionTTL) is treated as
-- offline by the reader, and swept by a periodic scanner so the table does
-- not grow one permanent stale row per subscriber that ever connected once.

CREATE TABLE IF NOT EXISTS live_sessions (
    subscriber_id INTEGER      PRIMARY KEY REFERENCES subscribers(id),
    session_id    TEXT         NOT NULL,
    nas_ip        TEXT         NOT NULL DEFAULT '',
    assigned_ip   TEXT         NOT NULL DEFAULT '',
    bytes_in      BIGINT       NOT NULL DEFAULT 0,
    bytes_out     BIGINT       NOT NULL DEFAULT 0,
    bytes_total   BIGINT       NOT NULL DEFAULT 0, -- plan quota; 0 = unlimited
    speed_profile TEXT         NOT NULL DEFAULT '',
    fup_throttled BOOLEAN      NOT NULL DEFAULT false,
    started_at    TIMESTAMPTZ  NOT NULL,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);

-- Accounting-Interim-Update and Accounting-Stop key on Acct-Session-Id, not
-- subscriber_id — the primary key doesn't serve that lookup.
CREATE INDEX IF NOT EXISTS idx_live_sessions_session_id ON live_sessions(session_id);

-- The staleness sweep scans on updated_at; without this index it is a full
-- table scan every tick.
CREATE INDEX IF NOT EXISTS idx_live_sessions_updated_at ON live_sessions(updated_at);

-- +goose Down
DROP TABLE IF EXISTS live_sessions;
