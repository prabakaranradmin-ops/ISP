-- +goose Up
-- NAS interface traffic, polled over SNMP and graphed — FR-NET-006.
--
-- Three parts: SNMP settings on each router, the router's interfaces as last
-- seen, and a time series of per-interface rates.

-- ── SNMP settings per router ────────────────────────────────────────────────
--
-- Off by default. A router does not answer SNMP until someone configures a
-- community on it, and polling every registered NAS would fill the error
-- column for every one that has not been set up.
--
-- The community string is a shared secret — on most routers it grants read
-- access to the whole configuration tree, not just counters — so it is stored
-- encrypted exactly as radius_secret_encrypted is, and never rendered back.
--
-- SNMPv2c only: v1 has no 64-bit counters (see nas_interface_samples), and v3
-- is not supported yet. Recorded in HANDOFF.
ALTER TABLE nas_devices
    ADD COLUMN IF NOT EXISTS snmp_enabled             BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS snmp_community_encrypted TEXT,
    ADD COLUMN IF NOT EXISTS snmp_port                INTEGER     NOT NULL DEFAULT 161,
    -- The last attempt, the last success, and why the last attempt failed.
    -- Kept apart so the console can say "not answering since <last ok>"
    -- rather than only "failed", which is the difference between a router
    -- that blipped and one that has been dark for a week.
    ADD COLUMN IF NOT EXISTS snmp_last_polled_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS snmp_last_ok_at          TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS snmp_last_error          TEXT;

ALTER TABLE nas_devices
    ADD CONSTRAINT chk_nas_snmp_port CHECK (snmp_port BETWEEN 1 AND 65535),
    -- Enabled with no community would poll with an empty one, which every
    -- agent refuses; the screen would then show a failure that is really a
    -- missing setting.
    ADD CONSTRAINT chk_nas_snmp_has_community
        CHECK (NOT snmp_enabled OR snmp_community_encrypted IS NOT NULL);

-- ── Interfaces ───────────────────────────────────────────────────────────────
--
-- Keyed by name, not by ifIndex. ifIndex is the SNMP row number, and routers
-- are free to renumber it — MikroTik does after a reboot or a module change —
-- so keying on it would quietly continue ether1's graph with ether2's traffic.
-- The name is what an operator reads on the router and what they mean when
-- they ask about a port. A renamed interface starts a new history, which is
-- honest: it may well be a different link now.
--
-- The last raw counters live here rather than in the samples. A rate needs
-- the previous reading, and keeping it on the interface row is what lets a
-- poll be a single compare-and-swap: the write only lands if
-- last_counter_at is still what this poller read, so two pollers racing on
-- one router cannot both record the same interval.
--
-- NUMERIC(20,0) because the counters are unsigned 64-bit and BIGINT is
-- signed: a counter past 2^63 would not fit, however unlikely that is.
CREATE TABLE IF NOT EXISTS nas_interfaces (
    id               SERIAL        PRIMARY KEY,
    nas_id           INTEGER       NOT NULL REFERENCES nas_devices(id) ON DELETE CASCADE,
    name             VARCHAR(255)  NOT NULL,
    if_index         INTEGER       NOT NULL CHECK (if_index > 0),
    alias            VARCHAR(255)  NOT NULL DEFAULT '',
    if_type          INTEGER,
    -- ifHighSpeed, in Mbit/s; NULL when the device does not report one.
    speed_mbps       BIGINT        CHECK (speed_mbps IS NULL OR speed_mbps >= 0),
    oper_up          BOOLEAN,
    -- False when the device reported no 64-bit counters for this interface.
    -- It is listed, so the operator can see it exists, but never graphed:
    -- see nas_interface_samples.
    has_hc_counters  BOOLEAN       NOT NULL DEFAULT TRUE,
    last_in_octets   NUMERIC(20,0),
    last_out_octets  NUMERIC(20,0),
    last_counter_at  TIMESTAMPTZ,
    last_seen_at     TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_nas_interface_name UNIQUE (nas_id, name),
    CONSTRAINT chk_nas_interface_name_not_blank CHECK (btrim(name) <> ''),
    CONSTRAINT chk_nas_interface_counters_together CHECK (
        (last_in_octets IS NULL) = (last_out_octets IS NULL) AND
        (last_in_octets IS NULL) = (last_counter_at IS NULL)
    )
);

-- ── Samples ──────────────────────────────────────────────────────────────────
--
-- One row per interface per poll: the average rate over the interval since
-- the previous poll, in bits per second, with the interval it covers.
--
-- Rates rather than raw counters, so the decisions about a bad reading are
-- made once, when it is written, instead of in every graph query: a counter
-- that went backwards (the router rebooted), a gap long enough that an
-- average across it would draw a flat line through an outage, and a rate the
-- link could not physically carry are all simply not recorded. A graph then
-- shows a hole where there was no trustworthy measurement.
--
-- Only 64-bit counters (ifHCInOctets/ifHCOutOctets) are used. The 32-bit ones
-- wrap every 34 seconds at 1 Gbit/s, so at a five-minute poll they would
-- report a number unrelated to the traffic — a wrong graph is worse than none.
CREATE TABLE IF NOT EXISTS nas_interface_samples (
    interface_id      INTEGER      NOT NULL REFERENCES nas_interfaces(id) ON DELETE CASCADE,
    sampled_at        TIMESTAMPTZ  NOT NULL,
    interval_seconds  INTEGER      NOT NULL CHECK (interval_seconds > 0),
    in_bps            BIGINT       NOT NULL CHECK (in_bps >= 0),
    out_bps           BIGINT       NOT NULL CHECK (out_bps >= 0),
    PRIMARY KEY (interface_id, sampled_at)
);

-- The retention purge deletes by age across every interface; without this it
-- would scan the whole table each time.
CREATE INDEX IF NOT EXISTS idx_nas_interface_samples_age ON nas_interface_samples (sampled_at);

-- +goose Down
DROP TABLE IF EXISTS nas_interface_samples;
DROP TABLE IF EXISTS nas_interfaces;
ALTER TABLE nas_devices DROP CONSTRAINT IF EXISTS chk_nas_snmp_has_community;
ALTER TABLE nas_devices DROP CONSTRAINT IF EXISTS chk_nas_snmp_port;
ALTER TABLE nas_devices
    DROP COLUMN IF EXISTS snmp_last_error,
    DROP COLUMN IF EXISTS snmp_last_ok_at,
    DROP COLUMN IF EXISTS snmp_last_polled_at,
    DROP COLUMN IF EXISTS snmp_port,
    DROP COLUMN IF EXISTS snmp_community_encrypted,
    DROP COLUMN IF EXISTS snmp_enabled;
