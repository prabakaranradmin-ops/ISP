-- +goose Up
-- Persistent backing for the RADIUS fast-verifier cache
-- (internal/radius/verifiercache.go), which until now lived only in the
-- daemon's process memory.
--
-- WHY THIS EXISTS
--
-- The cache lets a repeat authentication skip bcrypt cost-12 (~280ms,
-- ~19x NFR-PERF-001's 15ms budget). In memory, it is empty after every
-- restart — which is precisely when it is needed most. A deploy, a crash,
-- or an OOM kill is usually followed by subscribers reconnecting, and every
-- one of those reconnects then pays full bcrypt. On a 2-vCPU host that
-- caps cold authentication at roughly 7/second, so a few thousand
-- subscribers take tens of minutes to get back online while RADIUS clients
-- retransmit and deepen the queue. The restart that was supposed to fix an
-- outage extends it.
--
-- It also closes a second gap. cmd/api/main.go's subCacheInvalidator is a
-- documented no-op: api_service cannot reach into radiusd's process memory,
-- so a password change relied on a 60-second TTL to take effect. A table
-- both processes can see makes that invalidation immediate.
--
-- WHAT IS STORED, AND THE TRADE-OFF THAT COMES WITH IT
--
-- Never a password and never a bcrypt hash — only
-- HMAC-SHA256(secret, len||password || len||passwordHash), 32 bytes. The
-- secret is RADIUS_VERIFIER_SECRET, which lives in the environment and is
-- deliberately NOT stored here.
--
-- Persisting these does widen one attack path, and it should be stated
-- rather than glossed: an attacker holding BOTH a database read and the
-- verifier secret can test password guesses against these rows offline at
-- HMAC speed (microseconds) instead of bcrypt speed (~280ms). That is a
-- real amplification. What bounds it:
--
--   * A database read alone is useless — the rows are indistinguishable
--     from random without the secret.
--   * An attacker who has the environment secret has also, by
--     construction, got DB_DSN and AES_KEY_STORE_URL out of the same
--     .env — at which point the encrypted PII columns are already exposed
--     and this is not the weakest link.
--   * Rows expire (expires_at) and are reaped, so the exposed set is
--     recent authentications rather than the whole subscriber base.
--
-- If that trade is unacceptable for a given deployment, leave
-- VERIFIER_CACHE_PERSIST unset: the daemon falls back to the in-memory
-- cache and behaves exactly as before.

CREATE TABLE IF NOT EXISTS radius_verifier_cache (
    -- Keyed on the subscriber rather than the username so that deleting a
    -- subscriber takes their verifier with it, with no application code
    -- needed to remember. Terminating an account should not leave a usable
    -- fast-path entry behind under any circumstances.
    subscriber_id INTEGER      PRIMARY KEY REFERENCES subscribers(id) ON DELETE CASCADE,

    -- Denormalised so both hot paths can work from the identifier they
    -- already hold: the daemon warms its in-process map keyed by username,
    -- and api_service invalidates by username (its SubCache interface has
    -- no subscriber id). UNIQUE because subscribers.username is.
    username      TEXT         NOT NULL UNIQUE,

    -- HMAC-SHA256 output: always 32 bytes. The CHECK is a guard against a
    -- future caller storing something else entirely — a truncated or
    -- oversized value here would silently never match, degrading every
    -- authentication to the bcrypt path with nothing reporting why.
    verifier      BYTEA        NOT NULL CHECK (octet_length(verifier) = 32),

    expires_at    TIMESTAMPTZ  NOT NULL,
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- The reaper's scan, and the warmup query's filter. Partial on the
-- predicate that matters would not help here (every row is a candidate as
-- it ages), so this is a plain btree.
CREATE INDEX IF NOT EXISTS idx_verifier_cache_expiry
    ON radius_verifier_cache (expires_at);

-- +goose Down
DROP TABLE IF EXISTS radius_verifier_cache CASCADE;
