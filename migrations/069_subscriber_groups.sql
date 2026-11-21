-- +goose Up
-- Named subscriber groups — FR-GRP-001.
--
-- Subscribers could be filtered by plan, by status and by franchise, which
-- are all facts the system already held about them. What an operator could
-- not do was name a set of their own: a locality, an apartment block, a
-- promotion cohort, the twenty customers on one feeder cable. Those sets are
-- how an ISP actually talks about its base, and without them every such
-- question ended in a spreadsheet beside the system.
--
-- Many-to-many rather than a group_id column on subscribers. A customer is
-- usually in one locality but may also be in a campaign cohort, and the two
-- are not alternatives. Retrofitting one-to-many into many-to-many later
-- means a migration plus rewriting every query that filters by it.
CREATE TABLE IF NOT EXISTS subscriber_groups (
    id          SERIAL        PRIMARY KEY,
    name        VARCHAR(100)  NOT NULL CHECK (btrim(name) <> ''),
    description TEXT,

    -- Retired rather than deleted, the rule plans (061), products (058), IP
    -- pools (060), bandwidth profiles (065) and fair-use policies (067) all
    -- follow. It matters more here than for most: a group can be the
    -- segment of an announcement that has already been sent, and see the
    -- note on segment_group_id below.
    active      BOOLEAN       NOT NULL DEFAULT TRUE,

    created_by  TEXT,
    created_at  TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_by  TEXT,
    updated_at  TIMESTAMPTZ
);

-- Operators refer to a group by name in conversation, so two live ones
-- sharing a name is a mistake waiting to happen. Only among the live ones: a
-- retired "Anna Nagar" and its replacement may legitimately share a name,
-- which is how a group gets rebuilt after a survey.
CREATE UNIQUE INDEX uq_subscriber_group_name_active
    ON subscriber_groups (lower(btrim(name))) WHERE active;

CREATE INDEX idx_subscriber_groups_active ON subscriber_groups (active, name);

CREATE TABLE IF NOT EXISTS subscriber_group_members (
    group_id      INTEGER     NOT NULL REFERENCES subscriber_groups(id) ON DELETE CASCADE,
    subscriber_id INTEGER     NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    added_by      TEXT,
    added_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The pair is the identity: adding a subscriber twice is the same
    -- membership, not two, and a second row would double them in every
    -- count and every fan-out.
    PRIMARY KEY (group_id, subscriber_id)
);

-- The primary key serves "who is in this group". This index serves the other
-- direction — "which groups is this subscriber in" — which the subscriber's
-- own page asks for every time it loads.
CREATE INDEX idx_group_members_subscriber ON subscriber_group_members (subscriber_id);

-- A fourth segment dimension for announcements, beside franchise, plan and
-- status (migration 028).
--
-- ON DELETE RESTRICT, deliberately unlike the SET NULL the other three
-- carry. A NULL segment means "no restriction on this dimension", so if a
-- targeted group were deleted and its reference nulled, an announcement
-- already sent to twenty people would silently start showing in every
-- subscriber's portal. Retiring a group leaves its memberships and its
-- references intact, which is the supported way to take one out of use.
--
-- The same hazard exists today for segment_plan_id and segment_franchise_id
-- and is not introduced here; it is recorded in HANDOFF.md rather than
-- changed, because altering those constraints would affect existing rows.
ALTER TABLE announcements ADD COLUMN segment_group_id INTEGER
    REFERENCES subscriber_groups(id) ON DELETE RESTRICT;

-- +goose Down
ALTER TABLE announcements DROP COLUMN IF EXISTS segment_group_id;
DROP INDEX IF EXISTS idx_group_members_subscriber;
DROP TABLE IF EXISTS subscriber_group_members;
DROP INDEX IF EXISTS idx_subscriber_groups_active;
DROP INDEX IF EXISTS uq_subscriber_group_name_active;
DROP TABLE IF EXISTS subscriber_groups;
