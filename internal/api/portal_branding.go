package api

import (
	"context"
	"errors"

	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Captive-portal branding — FR-HSP-004 | migration 072.
//
// Service methods only, no HTTP routes: the console is where branding is
// edited, and it calls these rather than the store so a change leaves the
// same audit record and invalidates the portal's cache whichever screen made
// it — the arrangement internal/api/groups.go and bandwidth.go use.

// PortalBrandingStore persists the branding. Satisfied by *db.HotspotStore.
type PortalBrandingStore interface {
	PortalBranding(ctx context.Context) (hotspot.Branding, error)
	SavePortalBranding(ctx context.Context, b hotspot.Branding, by string) error
	SetPortalLogo(ctx context.Context, data []byte, by string) error
	ClearPortalLogo(ctx context.Context, by string) error
}

// BrandingInvalidator drops the captive portal's cached branding. Satisfied
// by *hotspot.BrandingCache.
//
// Without it a save would take up to hotspot.BrandingCacheTTL to appear, and
// an operator who saved and immediately looked at the portal would conclude
// the save had not worked.
type BrandingInvalidator interface {
	Invalidate()
}

// ErrPortalBrandingUnwired is returned when no branding store is configured.
var ErrPortalBrandingUnwired = errors.New("api: captive-portal branding is not configured")

// PortalBranding returns the stored branding.
func (h *Handler) PortalBranding(ctx context.Context) (hotspot.Branding, error) {
	if h.portalBranding == nil {
		return hotspot.Branding{}, ErrPortalBrandingUnwired
	}
	return h.portalBranding.PortalBranding(ctx)
}

// SavePortalBranding writes the text and colours.
//
// Audited by which fields are set rather than by their text: the trail needs
// to show that the portal's public wording changed and who changed it, and
// the wording itself is on the row.
func (h *Handler) SavePortalBranding(ctx context.Context, b hotspot.Branding) error {
	if h.portalBranding == nil {
		return ErrPortalBrandingUnwired
	}
	if err := h.portalBranding.SavePortalBranding(ctx, b, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	h.invalidateBranding()
	b.Normalise()
	middleware.Audit(ctx, "portal_branding.saved", "captive_portal", map[string]any{
		"venue_name_set":   b.VenueName != "",
		"headline_set":     b.Headline != "",
		"welcome_text_set": b.WelcomeText != "",
		"footer_text_set":  b.FooterText != "",
		"accent_color":     b.AccentColor,
		"background_color": b.BackgroundColor,
	})
	return nil
}

// SetPortalLogo stores a new logo.
func (h *Handler) SetPortalLogo(ctx context.Context, data []byte) error {
	if h.portalBranding == nil {
		return ErrPortalBrandingUnwired
	}
	if err := h.portalBranding.SetPortalLogo(ctx, data, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	h.invalidateBranding()
	middleware.Audit(ctx, "portal_branding.logo_set", "captive_portal",
		map[string]any{"bytes": len(data)})
	return nil
}

// ClearPortalLogo removes the logo.
func (h *Handler) ClearPortalLogo(ctx context.Context) error {
	if h.portalBranding == nil {
		return ErrPortalBrandingUnwired
	}
	if err := h.portalBranding.ClearPortalLogo(ctx, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	h.invalidateBranding()
	middleware.Audit(ctx, "portal_branding.logo_cleared", "captive_portal", nil)
	return nil
}

func (h *Handler) invalidateBranding() {
	if h.brandingCache != nil {
		h.brandingCache.Invalidate()
	}
}
