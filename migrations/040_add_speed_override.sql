-- +goose Up
-- Owner-triggered temporary speed override (console "Speed override" card):
-- a CoA-pushed rate change that does not touch the subscriber's billed plan
-- and reverts on its own, distinct from a plan change. NULL
-- speed_override_expires_at means "until manually cleared".

ALTER TABLE subscribers ADD COLUMN speed_override_rate_limit VARCHAR(32);
ALTER TABLE subscribers ADD COLUMN speed_override_expires_at TIMESTAMPTZ;

-- Backs internal/fup/scanner.go's expiry sweep: find subscribers whose
-- override has passed its expiry without scanning every subscriber row.
CREATE INDEX idx_subscribers_speed_override_expiry
    ON subscribers(speed_override_expires_at)
    WHERE speed_override_expires_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_subscribers_speed_override_expiry;
ALTER TABLE subscribers DROP COLUMN speed_override_rate_limit;
ALTER TABLE subscribers DROP COLUMN speed_override_expires_at;
