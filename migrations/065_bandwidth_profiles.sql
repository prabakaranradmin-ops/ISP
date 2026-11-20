-- +goose Up
-- Reusable bandwidth profiles — FR-CAT-002.
--
-- A plan's speed lived in one varchar(50), plans.rate_limit_string, holding
-- MikroTik's "50M/50M". That expresses a ceiling and nothing else, so three
-- things an ISP sells were simply not representable: a committed floor (the
-- min-rate a queue is guaranteed under contention), a burst (a higher rate
-- allowed for a short window, which is what makes a speed test look good
-- and is how most Indian retail broadband is actually marketed), and a
-- queue priority (which customer's traffic yields when the uplink is full).
--
-- They are a separate table rather than more columns on plans because the
-- same shaping is reused across many tariffs: an operator typically runs a
-- handful of profiles ("Retail burst", "Business committed") against dozens
-- of plans that differ only in price, quota and validity. Editing the
-- profile then re-shapes every plan on it, which is the point — and is also
-- why retiring one is a separate act from deleting it (see below).
CREATE TABLE IF NOT EXISTS bandwidth_profiles (
    id          SERIAL        PRIMARY KEY,
    name        VARCHAR(100)  NOT NULL,

    -- The ceiling, in bits per second, stored as a number rather than
    -- MikroTik's "50M" text. The text form is a vendor dialect: Huawei and
    -- ZTE want a 32-bit integer VSA, Cisco wants the name of a policy, and
    -- re-parsing "50M" at every send (which is what internal/nas's
    -- parseRateLimitString does today) puts a string parse on the
    -- authentication hot path and makes "50 M" or "50MB" a runtime failure
    -- rather than something the console refuses on entry.
    max_down_bps  BIGINT NOT NULL CHECK (max_down_bps  > 0),
    max_up_bps    BIGINT NOT NULL CHECK (max_up_bps    > 0),

    -- The committed floor. 0 means "no guarantee", which is the honest
    -- default: a plan sold without a committed rate should not claim one.
    min_down_bps  BIGINT NOT NULL DEFAULT 0 CHECK (min_down_bps >= 0),
    min_up_bps    BIGINT NOT NULL DEFAULT 0 CHECK (min_up_bps   >= 0),

    -- Burst. All four of these are one feature and are meaningless apart:
    -- a burst rate with no threshold bursts forever (so it is just a higher
    -- ceiling), and a threshold with no time never stops. The table-level
    -- CHECK below enforces that they are set together or not at all, rather
    -- than trusting the console to.
    burst_down_bps       BIGINT NOT NULL DEFAULT 0 CHECK (burst_down_bps      >= 0),
    burst_up_bps         BIGINT NOT NULL DEFAULT 0 CHECK (burst_up_bps        >= 0),
    burst_threshold_down BIGINT NOT NULL DEFAULT 0 CHECK (burst_threshold_down >= 0),
    burst_threshold_up   BIGINT NOT NULL DEFAULT 0 CHECK (burst_threshold_up   >= 0),
    burst_time_seconds   INTEGER NOT NULL DEFAULT 0 CHECK (burst_time_seconds  >= 0),

    -- MikroTik queue priority: 1 is served first, 8 last. The range is the
    -- vendor's, not ours; a value outside it is silently ignored by the NAS,
    -- which is the failure mode this package exists to stop reproducing.
    priority    SMALLINT NOT NULL DEFAULT 8 CHECK (priority BETWEEN 1 AND 8),

    -- Retired rather than deleted, the same rule plans (061), products (058)
    -- and IP pools (060) follow: a profile in use is shaping live sessions,
    -- so removing it would change what subscribers get mid-cycle. Retiring
    -- takes it out of the dropdowns and leaves every plan already on it
    -- exactly as it is.
    active      BOOLEAN       NOT NULL DEFAULT TRUE,

    created_by  TEXT,
    created_at  TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_by  TEXT,
    updated_at  TIMESTAMPTZ,

    -- A burst must be faster than the ceiling it bursts above, or it is not
    -- a burst. Equal is allowed and means "no burst" expressed the long way.
    CONSTRAINT chk_bw_burst_above_max CHECK (
        burst_down_bps = 0 OR burst_down_bps >= max_down_bps),
    CONSTRAINT chk_bw_burst_above_max_up CHECK (
        burst_up_bps = 0 OR burst_up_bps >= max_up_bps),

    -- The floor cannot exceed the ceiling.
    CONSTRAINT chk_bw_min_below_max CHECK (min_down_bps <= max_down_bps),
    CONSTRAINT chk_bw_min_below_max_up CHECK (min_up_bps <= max_up_bps),

    -- Burst is all-or-nothing, in each direction independently: an operator
    -- may legitimately burst the download and not the upload.
    CONSTRAINT chk_bw_burst_complete_down CHECK (
        (burst_down_bps = 0 AND burst_threshold_down = 0)
        OR (burst_down_bps > 0 AND burst_threshold_down > 0 AND burst_time_seconds > 0)),
    CONSTRAINT chk_bw_burst_complete_up CHECK (
        (burst_up_bps = 0 AND burst_threshold_up = 0)
        OR (burst_up_bps > 0 AND burst_threshold_up > 0 AND burst_time_seconds > 0)),

    -- A burst threshold above the ceiling can never be crossed, so the
    -- burst would never engage — a profile that looks configured and does
    -- nothing.
    CONSTRAINT chk_bw_threshold_reachable CHECK (
        burst_threshold_down <= max_down_bps AND burst_threshold_up <= max_up_bps)
);

-- Same rule and same reason as uq_plan_name_active (061): operators refer
-- to a profile by name, two live ones sharing a name is a support call, and
-- a retired profile may share its name with the replacement that supersedes
-- it.
CREATE UNIQUE INDEX uq_bandwidth_profile_name_active
    ON bandwidth_profiles (lower(btrim(name))) WHERE active;

-- The dropdown's query.
CREATE INDEX idx_bandwidth_profiles_active ON bandwidth_profiles (active, name);

-- Nullable, and deliberately so. Every existing plan keeps shaping traffic
-- from rate_limit_string exactly as before, and nothing about this migration
-- changes a single live session. A plan with a profile resolves its shaping
-- from the profile; a plan without one falls back to rate_limit_string. That
-- fallback is not a transitional nicety to be removed later — hotspot
-- vouchers and the demo seed both create plans without going near a
-- profile.
--
-- ON DELETE RESTRICT rather than SET NULL: silently dropping a plan back to
-- its legacy rate_limit_string because somebody deleted a profile would
-- re-shape live subscribers with nothing in the logs. Retiring is the
-- supported path, and a profile still referenced cannot be deleted at all.
ALTER TABLE plans ADD COLUMN bandwidth_profile_id INTEGER
    REFERENCES bandwidth_profiles(id) ON DELETE RESTRICT;

CREATE INDEX idx_plans_bandwidth_profile ON plans (bandwidth_profile_id)
    WHERE bandwidth_profile_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_plans_bandwidth_profile;
ALTER TABLE plans DROP COLUMN IF EXISTS bandwidth_profile_id;
DROP INDEX IF EXISTS idx_bandwidth_profiles_active;
DROP INDEX IF EXISTS uq_bandwidth_profile_name_active;
DROP TABLE IF EXISTS bandwidth_profiles;
