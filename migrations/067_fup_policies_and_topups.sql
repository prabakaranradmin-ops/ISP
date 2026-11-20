-- +goose Up
-- Multi-tier fair-use policies and purchasable data top-ups — FR-CAT-003.
--
-- A plan had one threshold and one throttle: cross 500GB, drop to 10M/10M
-- for the rest of the cycle. Real tariffs are rarely that flat — the common
-- Indian retail shape is a gentle first step and a harder one for the
-- heaviest users, so that somebody 10% over quota is not treated the same as
-- somebody at triple it. And there was no way to sell a subscriber out of a
-- throttle at all.
--
-- Policies are reusable, for the same reason bandwidth profiles are
-- (migration 065): an operator runs a handful of fair-use shapes across many
-- tariffs that differ only in price, quota and validity.
CREATE TABLE IF NOT EXISTS fup_policies (
    id          SERIAL        PRIMARY KEY,
    name        VARCHAR(100)  NOT NULL,
    -- Retired rather than deleted, the rule plans (061), products (058),
    -- IP pools (060) and bandwidth profiles (065) all follow: a policy in
    -- use is shaping live sessions.
    active      BOOLEAN       NOT NULL DEFAULT TRUE,
    created_by  TEXT,
    created_at  TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_by  TEXT,
    updated_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX uq_fup_policy_name_active
    ON fup_policies (lower(btrim(name))) WHERE active;

CREATE TABLE IF NOT EXISTS fup_policy_tiers (
    id              SERIAL  PRIMARY KEY,
    policy_id       INTEGER NOT NULL REFERENCES fup_policies(id) ON DELETE CASCADE,

    -- Cumulative usage in the cycle, not an increment above the previous
    -- tier. Cumulative because it is what the scanner compares against and
    -- what an operator reads off a tariff sheet ("throttle at 500GB, harder
    -- at 1TB"); increments would have to be summed before either could
    -- happen.
    threshold_bytes BIGINT  NOT NULL CHECK (threshold_bytes > 0),

    -- A MikroTik-format "down/up" rate, the same form
    -- plans.fup_throttle_string holds and the CoA path already sends.
    -- Deliberately not a bandwidth profile (065): a throttle is a flat
    -- ceiling, and giving a throttled subscriber a burst would hand back
    -- the speed the throttle exists to remove.
    throttle_rate   VARCHAR(50) NOT NULL CHECK (btrim(throttle_rate) <> ''),

    -- Two tiers at one threshold is ambiguous: both apply, and which
    -- throttle a subscriber received would depend on the order the rows
    -- came back in.
    CONSTRAINT uq_fup_tier_threshold UNIQUE (policy_id, threshold_bytes)
);

-- Read ascending so internal/fup.Policy.TierFor can rely on the order
-- without sorting on every scanner pass.
CREATE INDEX idx_fup_policy_tiers_policy
    ON fup_policy_tiers (policy_id, threshold_bytes);

-- Nullable, and deliberately so: a plan without a policy keeps throttling
-- from its own fup_threshold_bytes and fup_throttle_string exactly as
-- before. That is a supported steady state, not a migration window — the
-- demo seed and hotspot plans both set those columns directly and never
-- touch the catalogue.
--
-- ON DELETE RESTRICT, as for bandwidth_profile_id (065): silently dropping a
-- plan back to its legacy single threshold because somebody deleted a policy
-- would change what every subscriber on it experiences, with nothing in the
-- logs. Retiring is the supported path.
ALTER TABLE plans ADD COLUMN fup_policy_id INTEGER
    REFERENCES fup_policies(id) ON DELETE RESTRICT;

CREATE INDEX idx_plans_fup_policy ON plans (fup_policy_id)
    WHERE fup_policy_id IS NOT NULL;

-- Which rate is currently applied to this subscriber, or NULL for none.
--
-- subscribers.fup_active is a boolean: throttled or not. That was enough for
-- one threshold, and cannot express "throttled, but to the gentler of two
-- tiers" — so a subscriber who crossed the second threshold would already
-- have fup_active set and the scanner, which skips anyone already throttled,
-- would never send the harder CoA. Recording the rate rather than the tier id
-- makes the scanner's question a single comparison that covers every case at
-- once: escalating a tier, easing back after a top-up, and releasing the
-- throttle entirely.
--
-- fup_active is kept and still maintained, because the Access-Accept path,
-- the portal and the console all read it. The two are written together.
ALTER TABLE subscribers ADD COLUMN fup_applied_rate VARCHAR(50);

-- Backfill, so the new column is truthful for subscribers who are already
-- throttled on the old single-threshold path.
--
-- Without this, every currently-throttled subscriber has fup_active set and
-- no applied rate, so the scanner's first tick after deploy sees "applied
-- nothing, should be throttled" for all of them at once and issues a CoA
-- re-applying the rate they already have. Harmless per subscriber and
-- invisible in testing, but on a live estate it is a burst of pointless CoA
-- traffic at the exact moment an operator is watching a release.
UPDATE subscribers s
   SET fup_applied_rate = p.fup_throttle_string
  FROM plans p
 WHERE p.id = s.plan_id
   AND s.fup_active
   AND COALESCE(p.fup_throttle_string, '') <> '';

-- A purchased data allowance that raises a subscriber's thresholds for the
-- rest of the billing cycle.
--
-- Added allowance rather than "clear the throttle", which is the tempting
-- shortcut. Clearing a flag would un-throttle a subscriber who then carries
-- on using data and never throttles again that cycle however much they use,
-- because nothing moved the thresholds and every tier above them would be
-- unreachable. Raising the thresholds keeps every tier live, so somebody who
-- buys 50GB and then uses 50GB more is throttled again — which is what both
-- the operator and the customer expect of a top-up.
CREATE TABLE IF NOT EXISTS data_topups (
    id             SERIAL      PRIMARY KEY,
    subscriber_id  INTEGER     NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    bytes          BIGINT      NOT NULL CHECK (bytes > 0),

    -- The end of the cycle it was bought in. A top-up does not roll over:
    -- it lifts the throttle "for the rest of the cycle", and carrying it
    -- forward would let somebody bank allowance against a plan they may no
    -- longer be on. NOT NULL because a row with no expiry would lift a
    -- throttle forever, and that must not be reachable by omission.
    expires_at     TIMESTAMPTZ NOT NULL,

    -- The wallet debit that paid for it. A top-up must never exist without
    -- a payment behind it, and a subscriber disputing a charge has to be
    -- shown what it bought. Nullable only for a staff-granted goodwill
    -- top-up, which is why purchased_by is recorded separately.
    transaction_id INTEGER     REFERENCES wallet_ledgers(id),

    purchased_by   TEXT        NOT NULL,
    purchased_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_topup_expires_after_purchase CHECK (expires_at > purchased_at)
);

-- The scanner's query, on every pass: this subscriber's unexpired top-ups.
-- Partial on the expiry so the index stays small as old rows accumulate —
-- they are kept rather than purged, because they are what a billing dispute
-- is answered from.
CREATE INDEX idx_data_topups_subscriber ON data_topups (subscriber_id, expires_at);

-- +goose Down
DROP INDEX IF EXISTS idx_data_topups_subscriber;
DROP TABLE IF EXISTS data_topups;
ALTER TABLE subscribers DROP COLUMN IF EXISTS fup_applied_rate;
DROP INDEX IF EXISTS idx_plans_fup_policy;
ALTER TABLE plans DROP COLUMN IF EXISTS fup_policy_id;
DROP INDEX IF EXISTS idx_fup_policy_tiers_policy;
DROP TABLE IF EXISTS fup_policy_tiers;
DROP INDEX IF EXISTS uq_fup_policy_name_active;
DROP TABLE IF EXISTS fup_policies;
