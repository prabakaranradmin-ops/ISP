-- +goose Up
-- Per-plan, per-vendor RADIUS reply attributes — FR-CAT-004.
--
-- The bandwidth attribute was the only thing a plan could put in an
-- Access-Accept. Anything else an operator needed — put this plan's
-- subscribers in a MikroTik address list so firewall rules can be written
-- against them, allocate from a named pool on the router, cut a trial plan
-- after an hour, ask for interim accounting more often — meant editing Go
-- and shipping a release.
--
-- Attributes are validated against a dictionary in internal/nas rather than
-- stored as free text. RADIUS is silent about attributes a NAS does not
-- understand: a misspelled name or a wrongly typed value is ignored on
-- arrival, so the operator is left with a plan that looks configured and
-- does nothing. That silence is the failure internal/nas exists to stop
-- reproducing, and the dictionary is what turns it into a refusal at the
-- point of entry. The CHECKs here are the second gate, for a row that
-- arrives without going through Go.
CREATE TABLE IF NOT EXISTS plan_reply_attributes (
    id         SERIAL       PRIMARY KEY,
    plan_id    INTEGER      NOT NULL REFERENCES plans(id) ON DELETE CASCADE,

    -- Same vocabulary as nas_devices.vendor (migration 022) and
    -- plan_nas_profiles.vendor, because an attribute is only ever sent to a
    -- NAS of that vendor. internal/nas.KnownVendor and this list must agree;
    -- the Go side is tested against Vendors() so they cannot drift apart
    -- silently.
    vendor     VARCHAR(20)  NOT NULL
        CHECK (vendor IN ('mikrotik','huawei','zte','cisco','juniper','cisco_wlc','aruba','ruckus')),

    -- The dictionary's own spelling, canonicalised by Normalise before it
    -- gets here. Stored as text rather than an enum: the dictionary grows
    -- by a line of Go, and a CHECK listing every attribute name would mean
    -- a migration for each one.
    attribute  VARCHAR(64)  NOT NULL CHECK (btrim(attribute) <> ''),
    value      VARCHAR(253) NOT NULL CHECK (btrim(value) <> ''),

    -- Kept but not sent. An operator turning an attribute off to test
    -- whether it is the cause of something should not have to retype it,
    -- and deleting it would lose what it was set to.
    enabled    BOOLEAN      NOT NULL DEFAULT TRUE,

    created_by TEXT,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_by TEXT,
    updated_at TIMESTAMPTZ,

    -- One value per attribute per vendor per plan. Without this a plan
    -- could carry Mikrotik-Address-List twice and the NAS would receive the
    -- attribute twice, with no defined rule about which wins — the same
    -- class of ambiguity the dictionary's reserved list exists to prevent
    -- between an operator attribute and the plan's own shaping.
    --
    -- Case is already settled by Normalise, which rewrites the name to the
    -- dictionary's spelling, so a plain unique constraint is enough.
    CONSTRAINT uq_plan_reply_attribute UNIQUE (plan_id, vendor, attribute)
);

-- The authentication path's query: every enabled attribute for one plan.
-- Deliberately not filtered by vendor in the index, because the resolver
-- reads all of a plan's attributes once and filters in Go — a plan carries
-- a handful of rows, and one indexed read beats one per vendor.
CREATE INDEX idx_plan_reply_attributes_plan
    ON plan_reply_attributes (plan_id) WHERE enabled;

-- The console's listing, which shows a plan's attributes grouped by vendor.
CREATE INDEX idx_plan_reply_attributes_vendor
    ON plan_reply_attributes (plan_id, vendor, attribute);

-- ON DELETE CASCADE above, unlike the RESTRICT on plans.bandwidth_profile_id
-- (065): a reply attribute belongs to its plan and means nothing without it,
-- whereas a bandwidth profile is shared and outlives any one plan. Deleting
-- a plan that still has attributes should not be blocked by rows that exist
-- only to describe it.

-- +goose Down
DROP INDEX IF EXISTS idx_plan_reply_attributes_vendor;
DROP INDEX IF EXISTS idx_plan_reply_attributes_plan;
DROP TABLE IF EXISTS plan_reply_attributes;
