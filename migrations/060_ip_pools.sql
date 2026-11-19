-- +goose Up
-- IP pools and static addresses — FR-NET-005.
--
-- Until now the only addresses this system recorded were the CGNAT port
-- blocks a session was given (migration 011) and whatever the NAS put in
-- Framed-IP-Address on an accounting record. Nothing decided an address:
-- the router's own pool did, and a customer who paid for a static IP got
-- it by someone configuring it on the router by hand, where nothing
-- billed it and nothing stopped the same address being handed to two
-- subscribers.
--
-- Three tables: the pools an operator may hand addresses out of, the
-- addresses inside them that must not be handed out, and who currently
-- holds which address.

CREATE TABLE ip_pools (
    id            SERIAL      PRIMARY KEY,
    -- The router this pool lives behind. NULL means "not tied to one",
    -- which is the right answer for a single-router deployment or a routed
    -- block that follows the subscriber rather than the NAS.
    --
    -- ON DELETE RESTRICT, not CASCADE: deleting a router must not silently
    -- take the record of who holds which address with it.
    nas_id        INTEGER     REFERENCES nas_devices(id) ON DELETE RESTRICT,
    name          TEXT        NOT NULL,
    -- What the pool is for. Only 'static' is handed out by this system
    -- today; 'dhcp' and 'cgnat' are recorded so an operator can document
    -- the ranges the router owns and this system must not touch, which is
    -- the first thing anyone asks when an address collides.
    purpose       TEXT        NOT NULL,
    first_address INET        NOT NULL,
    last_address  INET        NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    created_by    TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by    TEXT,
    updated_at    TIMESTAMPTZ,

    CONSTRAINT chk_ip_pool_name CHECK (length(btrim(name)) BETWEEN 1 AND 60),
    CONSTRAINT chk_ip_pool_description CHECK (length(description) <= 200),
    CONSTRAINT chk_ip_pool_purpose CHECK (purpose IN ('static', 'dhcp', 'cgnat')),
    -- One address family, in order. A range from an IPv4 address to an
    -- IPv6 one is not a range at all, and Postgres would happily compare
    -- them.
    CONSTRAINT chk_ip_pool_range CHECK (
        family(first_address) = family(last_address) AND first_address <= last_address
    ),
    -- Single hosts, not networks. INET carries a mask, so 10.0.0.0/24
    -- would be ambiguous about whether the /24 is part of the value or
    -- just documentation; requiring /32 (or /128) makes the stored value
    -- exactly an address.
    CONSTRAINT chk_ip_pool_hosts CHECK (
        masklen(first_address) = CASE WHEN family(first_address) = 4 THEN 32 ELSE 128 END
        AND masklen(last_address) = CASE WHEN family(last_address) = 4 THEN 32 ELSE 128 END
    )
);

-- Pool names are unique per router, case-insensitively: "Static" and
-- "static" on one NAS are the same pool to an operator reading a list.
-- COALESCE because NULL nas_id would otherwise never collide with itself.
CREATE UNIQUE INDEX uq_ip_pool_name ON ip_pools (COALESCE(nas_id, 0), lower(btrim(name)));
CREATE INDEX idx_ip_pools_nas ON ip_pools (nas_id, purpose);

-- Addresses inside a pool that must not be handed out: the gateway, the
-- broadcast address, anything already configured statically on the router.
CREATE TABLE ip_pool_exclusions (
    id            SERIAL      PRIMARY KEY,
    pool_id       INTEGER     NOT NULL REFERENCES ip_pools(id) ON DELETE CASCADE,
    first_address INET        NOT NULL,
    last_address  INET        NOT NULL,
    -- Required: an exclusion nobody can explain is one nobody dares
    -- remove, and it will outlive the reason for it.
    reason        TEXT        NOT NULL,
    created_by    TEXT        NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_ip_excl_reason CHECK (length(btrim(reason)) BETWEEN 1 AND 200),
    CONSTRAINT chk_ip_excl_range CHECK (
        family(first_address) = family(last_address) AND first_address <= last_address
    ),
    CONSTRAINT chk_ip_excl_hosts CHECK (
        masklen(first_address) = CASE WHEN family(first_address) = 4 THEN 32 ELSE 128 END
        AND masklen(last_address) = CASE WHEN family(last_address) = 4 THEN 32 ELSE 128 END
    )
);

CREATE INDEX idx_ip_excl_pool ON ip_pool_exclusions (pool_id, first_address);

-- Who holds which address.
CREATE TABLE static_ip_assignments (
    id            SERIAL      PRIMARY KEY,
    -- RESTRICT for the same reason as nas_id above: a pool with live
    -- assignments is not something to delete by accident.
    pool_id       INTEGER     NOT NULL REFERENCES ip_pools(id) ON DELETE RESTRICT,
    subscriber_id INTEGER     NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    address       INET        NOT NULL,
    status        TEXT        NOT NULL DEFAULT 'active',
    -- The FR-BIL-022 attachment that bills this address, when the operator
    -- chose to bill it. This is what "billed through FR-BIL-022" means:
    -- a static IP is charged as an ordinary catalogue product on the
    -- subscriber's invoice, not through a billing path of its own.
    -- Releasing the address ends that charge in the same transaction.
    subscriber_product_id INTEGER REFERENCES subscriber_products(id),
    assigned_by    TEXT        NOT NULL,
    assigned_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at    TIMESTAMPTZ,
    released_by    TEXT,
    release_reason TEXT,

    CONSTRAINT chk_static_ip_host CHECK (
        masklen(address) = CASE WHEN family(address) = 4 THEN 32 ELSE 128 END
    ),
    CONSTRAINT chk_static_ip_status CHECK (
        (status = 'active'   AND released_at IS NULL)
     OR (status = 'released' AND released_at IS NOT NULL AND released_by IS NOT NULL
            AND length(btrim(COALESCE(release_reason, ''))) > 0)
    )
);

-- One live holder per address. This is the rule the whole table exists
-- for: two subscribers configured with one address is the failure an
-- operator spends an afternoon on, and it is a one-line database rule.
CREATE UNIQUE INDEX uq_static_ip_address
    ON static_ip_assignments (address) WHERE status = 'active';

-- One live address per subscriber.
--
-- Not an arbitrary limit: Framed-IP-Address carries exactly one address,
-- so a second one could not be returned and would sit in the table
-- looking allocated while being unreachable. A customer who needs a block
-- needs a routed subnet (Framed-Route or a delegated prefix), which this
-- table does not model and should not pretend to.
CREATE UNIQUE INDEX uq_static_ip_subscriber
    ON static_ip_assignments (subscriber_id) WHERE status = 'active';

CREATE INDEX idx_static_ip_pool ON static_ip_assignments (pool_id, status);
CREATE INDEX idx_static_ip_history ON static_ip_assignments (subscriber_id, assigned_at DESC);

-- +goose Down
DROP TABLE IF EXISTS static_ip_assignments;
DROP TABLE IF EXISTS ip_pool_exclusions;
DROP TABLE IF EXISTS ip_pools;
