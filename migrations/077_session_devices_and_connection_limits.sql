-- +goose Up
-- Which device a session is, and how many connections a subscriber may hold
-- — FR-HSP-002, FR-AAA-002, FR-BIL-022.
--
-- Two gaps with one cause: a session row said who and where, never which
-- device.
--
--   * Deactivating a lost or stolen phone registered for MAC Auth Bypass
--     stopped its next login and left the session it already had running.
--     A Disconnect-Request is addressed by Acct-Session-Id, and nothing
--     recorded which session belonged to the phone.
--
--   * Nothing limited how many PPPoE logins one subscriber account could run
--     at once, so one username and password could serve several premises.
--     A limit has to tell a second connection from the same router redialling
--     after its session dropped without an Accounting-Stop, or it locks a
--     paying subscriber out of their own line. The device is how.
--
-- The connection limit is 1 plus the quantity of the subscriber's active
-- recurring charges for catalogue products marked adds_connections: extra
-- connections are sold at a flat monthly price through the ordinary product
-- catalogue, and billed by the ordinary renewal.

-- The device behind the session: the MAC that authenticated, for MAC Auth
-- Bypass, and the Calling-Station-Id otherwise (the subscriber's router).
-- Normalised AA:BB:CC:DD:EE:FF; NULL when the NAS sent nothing MAC-shaped.
ALTER TABLE subscriber_session_history
    ADD COLUMN IF NOT EXISTS device_mac VARCHAR(17),
    -- Authenticated by MAC (a registered hotspot device), not by password.
    -- Those do not count towards the connection limit.
    ADD COLUMN IF NOT EXISTS mab        BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE subscriber_session_history
    ADD CONSTRAINT chk_session_device_mac
        CHECK (device_mac IS NULL OR device_mac ~ '^[0-9A-F]{2}(:[0-9A-F]{2}){5}$');

-- Deactivating a device looks up its live sessions by MAC.
CREATE INDEX IF NOT EXISTS idx_session_open_device
    ON subscriber_session_history (device_mac)
    WHERE stop_time IS NULL AND device_mac IS NOT NULL;

-- A catalogue product that adds one connection per unit attached. Only a
-- recurring charge can: a connection is held month by month, and a one-time
-- charge would sell one forever.
ALTER TABLE products
    ADD COLUMN IF NOT EXISTS adds_connections BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE products
    ADD CONSTRAINT chk_product_adds_connections
        CHECK (NOT adds_connections OR kind = 'recurring');

-- A login refused for being over the subscriber's connection limit is its
-- own reason in the authentication log, so "why can't they connect" is
-- answerable without reading code.
ALTER TABLE radius_auth_log DROP CONSTRAINT IF EXISTS chk_auth_log_reason_vocab;
ALTER TABLE radius_auth_log ADD CONSTRAINT chk_auth_log_reason_vocab CHECK (reject_reason IN (
    '', 'unknown_user', 'bad_password', 'not_authorised', 'locked_out',
    'no_mab_device', 'eap_failed', 'internal_error', 'connection_limit'
));

-- +goose Down
ALTER TABLE radius_auth_log DROP CONSTRAINT IF EXISTS chk_auth_log_reason_vocab;
DELETE FROM radius_auth_log WHERE reject_reason = 'connection_limit';
ALTER TABLE radius_auth_log ADD CONSTRAINT chk_auth_log_reason_vocab CHECK (reject_reason IN (
    '', 'unknown_user', 'bad_password', 'not_authorised', 'locked_out',
    'no_mab_device', 'eap_failed', 'internal_error'
));

ALTER TABLE products DROP CONSTRAINT IF EXISTS chk_product_adds_connections;
ALTER TABLE products DROP COLUMN IF EXISTS adds_connections;

DROP INDEX IF EXISTS idx_session_open_device;
ALTER TABLE subscriber_session_history DROP CONSTRAINT IF EXISTS chk_session_device_mac;
ALTER TABLE subscriber_session_history
    DROP COLUMN IF EXISTS mab,
    DROP COLUMN IF EXISTS device_mac;
