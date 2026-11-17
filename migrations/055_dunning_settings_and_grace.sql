-- +goose Up
-- Configurable dunning (FR-BIL-017) and per-subscriber grace (FR-BIL-016).
--
-- The ladder's five numbers were Go constants: changing "suspend after 3
-- days" meant a code change and a redeploy. They move to a single settings
-- row the scanner reads on every pass.

CREATE TABLE billing_settings (
    id                  SMALLINT    PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    remind_first_days   INTEGER     NOT NULL DEFAULT 7,
    remind_second_days  INTEGER     NOT NULL DEFAULT 3,
    remind_last_days    INTEGER     NOT NULL DEFAULT 1,
    grace_days          INTEGER     NOT NULL DEFAULT 3,
    soft_suspend_days   INTEGER     NOT NULL DEFAULT 3,
    updated_by          VARCHAR(100),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Reminders count down to expiry, so each must come no later than the
    -- one before it; equal values collapse two reminders into one, which is
    -- allowed. The upper bounds keep a typo (300 for 30) from suspending
    -- nobody for most of a year.
    CONSTRAINT chk_billing_reminder_order CHECK (
        remind_first_days >= remind_second_days AND remind_second_days >= remind_last_days AND remind_last_days >= 0),
    CONSTRAINT chk_billing_reminder_range CHECK (remind_first_days <= 30),
    CONSTRAINT chk_billing_grace_range CHECK (grace_days BETWEEN 0 AND 30),
    CONSTRAINT chk_billing_soft_range  CHECK (soft_suspend_days BETWEEN 0 AND 60)
);
INSERT INTO billing_settings (id) VALUES (1);

-- A grace extension granted to one subscriber: until grace_until they stay
-- in grace (service on) however far past expiry they are. Who granted it
-- and why are kept on the row; the status-capture trigger records the
-- state changes it causes.
ALTER TABLE subscribers
    ADD COLUMN IF NOT EXISTS grace_until  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS grace_reason TEXT,
    ADD COLUMN IF NOT EXISTS grace_set_by VARCHAR(100);

ALTER TABLE subscribers ADD CONSTRAINT chk_grace_has_reason CHECK (
    grace_until IS NULL OR (grace_reason IS NOT NULL AND grace_set_by IS NOT NULL));

-- +goose Down
ALTER TABLE subscribers DROP CONSTRAINT IF EXISTS chk_grace_has_reason;
ALTER TABLE subscribers DROP COLUMN IF EXISTS grace_set_by;
ALTER TABLE subscribers DROP COLUMN IF EXISTS grace_reason;
ALTER TABLE subscribers DROP COLUMN IF EXISTS grace_until;
DROP TABLE IF EXISTS billing_settings;
