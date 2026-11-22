-- +goose Up
-- Captive-portal branding — FR-HSP-004.
--
-- The walled-garden page (internal/hotspot/templates/portal.html) said
-- "Wi-Fi Sign In" in the codebase's own blue for every deployment, and the only
-- way to put a venue's name or logo on it was to edit the template and rebuild.
-- This holds the venue name, the page text, two colours and a logo, edited from
-- the console.
--
-- A table rather than environment variables. On the native Windows install
-- the environment lives in the service configuration, which an operator can
-- only reach from an administrator shell and which takes effect only after a
-- restart — "without editing templates" would be met in the letter and missed
-- in practice. Edited from a screen, it is something the person who owns the
-- venue's brand can actually change.
--
-- One row, enforced. The requirement is branding per deployment, and a
-- singleton keyed on a constant cannot drift into two rows that disagree about
-- which one the portal shows. An absent row means "use the defaults", so a
-- deployment that never opens the screen renders exactly what it did before
-- this migration.
CREATE TABLE IF NOT EXISTS captive_portal_branding (
    id                 SMALLINT     PRIMARY KEY DEFAULT 1 CHECK (id = 1),

    -- Every text column is nullable, and NULL means "the default text". An
    -- empty string is refused instead of stored, so there is exactly one
    -- spelling of "not set" and the renderer never has to ask whether ''
    -- meant "blank this out" or "nobody filled it in".
    venue_name         VARCHAR(80),
    headline           VARCHAR(120),
    welcome_text       VARCHAR(500),
    -- Shown under the forms: support contact, opening hours, the fair-use line
    -- a venue is obliged to display. Plain text — the page has no route off
    -- the walled garden yet, so a link to terms hosted elsewhere would not
    -- load for the very person it is addressed to.
    footer_text        VARCHAR(1000),

    -- Six-digit lowercase hex only. These are interpolated into an inline
    -- <style> block, and a strict shape is what makes that safe by
    -- construction rather than by escaping: nothing that matches this can
    -- close the declaration or start another one.
    accent_color       CHAR(7),
    background_color   CHAR(7),

    -- PNG or JPEG only, never SVG. An SVG served from the portal's own origin
    -- can carry script, and this is the one page in the system an anonymous
    -- visitor is sent to by design.
    logo               BYTEA,
    logo_content_type  VARCHAR(20),
    -- Hex SHA-256 of the logo, used as its cache key: the page links the logo
    -- by this hash, so a replaced logo is a new URL and no device keeps
    -- showing the old one from its cache.
    logo_sha256        CHAR(64),

    updated_by         VARCHAR(100),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_cpb_text_not_blank CHECK (
        (venue_name   IS NULL OR btrim(venue_name)   <> '') AND
        (headline     IS NULL OR btrim(headline)     <> '') AND
        (welcome_text IS NULL OR btrim(welcome_text) <> '') AND
        (footer_text  IS NULL OR btrim(footer_text)  <> '')
    ),
    CONSTRAINT chk_cpb_accent_color CHECK (
        accent_color IS NULL OR accent_color ~ '^#[0-9a-f]{6}$'
    ),
    CONSTRAINT chk_cpb_background_color CHECK (
        background_color IS NULL OR background_color ~ '^#[0-9a-f]{6}$'
    ),
    -- The logo, its type and its hash arrive together or not at all; a logo
    -- with no type could not be served with a Content-Type, and one with no
    -- hash could not be cache-busted.
    CONSTRAINT chk_cpb_logo_complete CHECK (
        (logo IS NULL) = (logo_content_type IS NULL) AND
        (logo IS NULL) = (logo_sha256 IS NULL)
    ),
    CONSTRAINT chk_cpb_logo_type CHECK (
        logo_content_type IS NULL OR logo_content_type IN ('image/png', 'image/jpeg')
    ),
    -- 100 KB, the same figure internal/hotspot enforces with a readable
    -- message. A captive portal is loaded over the venue's own shared uplink
    -- by a phone that has not been let onto the network yet; a two-megabyte
    -- logo is the difference between a page that appears and one the phone's
    -- sign-in sheet gives up on.
    CONSTRAINT chk_cpb_logo_size CHECK (
        logo IS NULL OR octet_length(logo) <= 102400
    ),
    CONSTRAINT chk_cpb_logo_sha256 CHECK (
        logo_sha256 IS NULL OR logo_sha256 ~ '^[0-9a-f]{64}$'
    )
);

-- +goose Down
DROP TABLE IF EXISTS captive_portal_branding;
