package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
)

// Captive-portal branding — FR-HSP-004 | migration 072.
// The domain rules live in internal/hotspot/branding.go.
//
// One row, id = 1. Every write is an upsert, so the first save from a
// deployment that has never opened the screen creates the row and every later
// save updates it; nothing ever has to ask whether the row exists yet.

var _ hotspot.BrandingStore = (*HotspotStore)(nil)

// PortalBranding returns the stored branding, or the zero Branding — every
// field defaulted — when nothing has been saved. The logo's bytes are not
// read here; see PortalLogo.
func (s *HotspotStore) PortalBranding(ctx context.Context) (hotspot.Branding, error) {
	const q = `
		SELECT COALESCE(venue_name, ''), COALESCE(headline, ''), COALESCE(welcome_text, ''),
		       COALESCE(footer_text, ''), COALESCE(accent_color, ''), COALESCE(background_color, ''),
		       logo IS NOT NULL, COALESCE(logo_content_type, ''), COALESCE(logo_sha256, ''),
		       COALESCE(updated_by, ''), updated_at
		  FROM captive_portal_branding WHERE id = 1`

	var b hotspot.Branding
	err := s.pool.QueryRow(ctx, q).Scan(&b.VenueName, &b.Headline, &b.WelcomeText,
		&b.FooterText, &b.AccentColor, &b.BackgroundColor,
		&b.HasLogo, &b.LogoContentType, &b.LogoSHA256, &b.UpdatedBy, &b.UpdatedAt)
	if isNoRows(err) {
		return hotspot.Branding{}, nil
	}
	if err != nil {
		return hotspot.Branding{}, fmt.Errorf("db: read portal branding: %w", err)
	}
	return b, nil
}

// PortalLogo returns the logo's bytes, type and hash, or nil when there is
// none.
func (s *HotspotStore) PortalLogo(ctx context.Context) ([]byte, string, string, error) {
	var (
		data       []byte
		ct, digest *string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT logo, logo_content_type, logo_sha256 FROM captive_portal_branding WHERE id = 1`).
		Scan(&data, &ct, &digest)
	if isNoRows(err) || data == nil {
		return nil, "", "", nil
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("db: read portal logo: %w", err)
	}
	return data, *ct, *digest, nil
}

// SavePortalBranding writes the text and colours. The logo is untouched: it
// has its own write, so saving a corrected welcome line never round-trips a
// 100 KB image through a form.
//
// Empty fields are stored as NULL — "use the default" — which is also how an
// operator puts a field back to its default: clear it and save.
func (s *HotspotStore) SavePortalBranding(ctx context.Context, b hotspot.Branding, by string) error {
	b.Normalise()
	if err := b.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO captive_portal_branding
		       (id, venue_name, headline, welcome_text, footer_text,
		        accent_color, background_color, updated_by, updated_at)
		VALUES (1, NULLIF($1, ''), NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''),
		        NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), NOW())
		ON CONFLICT (id) DO UPDATE SET
		       venue_name = EXCLUDED.venue_name, headline = EXCLUDED.headline,
		       welcome_text = EXCLUDED.welcome_text, footer_text = EXCLUDED.footer_text,
		       accent_color = EXCLUDED.accent_color, background_color = EXCLUDED.background_color,
		       updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
		b.VenueName, b.Headline, b.WelcomeText, b.FooterText,
		b.AccentColor, b.BackgroundColor, by)
	if err != nil {
		return fmt.Errorf("db: save portal branding: %w", err)
	}
	return nil
}

// SetPortalLogo validates and stores a logo, replacing any previous one.
//
// Validated here as well as by the caller, because this is the last point
// before the bytes become something the portal serves to anonymous visitors
// from its own origin — the check belongs next to the write, where no caller
// can skip it.
func (s *HotspotStore) SetPortalLogo(ctx context.Context, data []byte, by string) error {
	contentType, err := hotspot.ValidateLogo(data)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO captive_portal_branding (id, logo, logo_content_type, logo_sha256, updated_by, updated_at)
		VALUES (1, $1, $2, $3, NULLIF($4, ''), NOW())
		ON CONFLICT (id) DO UPDATE SET
		       logo = EXCLUDED.logo, logo_content_type = EXCLUDED.logo_content_type,
		       logo_sha256 = EXCLUDED.logo_sha256,
		       updated_by = EXCLUDED.updated_by, updated_at = NOW()`,
		data, contentType, hex.EncodeToString(sum[:]), by)
	if err != nil {
		return fmt.Errorf("db: save portal logo: %w", err)
	}
	return nil
}

// ClearPortalLogo removes the logo. A no-op, not an error, when there is none.
func (s *HotspotStore) ClearPortalLogo(ctx context.Context, by string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE captive_portal_branding
		   SET logo = NULL, logo_content_type = NULL, logo_sha256 = NULL,
		       updated_by = NULLIF($1, ''), updated_at = NOW()
		 WHERE id = 1`, by)
	if err != nil {
		return fmt.Errorf("db: clear portal logo: %w", err)
	}
	return nil
}
