-- +goose Up
-- Recoverable copy of a subscriber's PPPoE password, so the operations console
-- can show it (the way Jaze/Winbox do). The authoritative credential for RADIUS
-- stays the one-way bcrypt hash in password_hash; this column is a second,
-- reversible copy encrypted with the AES key store (same scheme as nas_devices
-- secrets and KYC). NULL means the password was set before this column existed
-- or has not been re-saved since — the console shows "not captured" and offers a
-- reset rather than guessing.
ALTER TABLE subscribers ADD COLUMN password_enc TEXT;

COMMENT ON COLUMN subscribers.password_enc IS
  'AES-GCM versioned ciphertext of the PPPoE password for operator reveal. NULL = not captured (legacy or never re-saved). Authoritative credential remains password_hash.';

-- +goose Down
ALTER TABLE subscribers DROP COLUMN password_enc;
