package staffui

import (
	"context"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// Captive-portal branding — FR-HSP-004.
//
// PortalBrandingManager is satisfied by *api.Handler, so a save is audited and
// invalidates the portal's cache exactly as it would from any other caller —
// the arrangement BandwidthManager and VendorManager use.
type PortalBrandingManager interface {
	PortalBranding(ctx context.Context) (hotspot.Branding, error)
	SavePortalBranding(ctx context.Context, b hotspot.Branding) error
	SetPortalLogo(ctx context.Context, data []byte) error
	ClearPortalLogo(ctx context.Context) error
}

// brandingUploadLimit is the whole multipart body: the logo's own cap plus
// room for the boundary, headers and the CSRF field. The logo is checked
// against hotspot.MaxLogoBytes separately, with a message that says so; this
// only stops the server buffering an arbitrarily large body to find out.
const brandingUploadLimit = hotspot.MaxLogoBytes + 64*1024

var brandingMessages = map[string]string{
	"saved":        "Saved. The captive portal shows it from the next page load.",
	"logo_set":     "Logo uploaded.",
	"logo_cleared": "Logo removed. The portal shows no logo.",
	"failed":       "That did not go through. Nothing was changed; try again.",
}

type brandingData struct {
	B hotspot.Branding
	// LinkWarning tells the operator when the accent will not be used for
	// links because it would be unreadable — the page substitutes a readable
	// colour silently, and this is so they know why.
	LinkWarning string

	DefaultHeadline   string
	DefaultWelcome    string
	DefaultAccent     string
	DefaultBackground string
	MaxLogoKB         int
	MaxVenueName      int
	MaxHeadline       int
	MaxWelcomeText    int
	MaxFooterText     int
	PreviewVersion    string
	PortalURL         string
}

// PortalBranding handles GET /staff/portal-branding.
func (h *Handler) PortalBranding(w http.ResponseWriter, r *http.Request) {
	s, ok := h.brandingGate(w, r)
	if !ok {
		return
	}
	b, err := h.portalBranding.PortalBranding(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("staffui: read portal branding failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not read the captive-portal settings.")
		return
	}

	d := h.page(s, "Captive portal", "portal-branding")
	q := r.URL.Query()
	if m, ok := brandingMessages[q.Get("m")]; ok {
		if q.Get("m") == "failed" {
			d.Error = m
		} else {
			d.Message = m
		}
	}
	// A validation sentence names the field and the rule — "a colour is a hex
	// code such as #1a6dcc", "the logo must be a PNG or JPEG" — so it is
	// carried verbatim rather than mapped to a code.
	if problem := q.Get("err"); problem != "" {
		d.Error = problem
	}

	d.Data = brandingData{
		B:                 b,
		LinkWarning:       b.LinkWarning(),
		DefaultHeadline:   hotspot.DefaultHeadline,
		DefaultWelcome:    hotspot.DefaultWelcomeText,
		DefaultAccent:     hotspot.DefaultAccentColor,
		DefaultBackground: hotspot.DefaultBackgroundColor,
		MaxLogoKB:         hotspot.MaxLogoBytes / 1024,
		MaxVenueName:      hotspot.MaxVenueName,
		MaxHeadline:       hotspot.MaxHeadline,
		MaxWelcomeText:    hotspot.MaxWelcomeText,
		MaxFooterText:     hotspot.MaxFooterText,
		// The preview frame's URL changes whenever the branding does, so the
		// browser cannot show a cached preview of what was there before.
		PreviewVersion: strconv.FormatInt(b.UpdatedAt.UnixNano(), 36),
		PortalURL:      "/hotspot/portal",
	}
	h.render(w, "portal_branding", d)
}

// SavePortalBrandingForm handles POST /staff/portal-branding.
func (h *Handler) SavePortalBrandingForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.brandingGate(w, r)
	if !ok {
		return
	}
	b := hotspot.Branding{
		VenueName:       r.PostFormValue("venue_name"),
		Headline:        r.PostFormValue("headline"),
		WelcomeText:     r.PostFormValue("welcome_text"),
		FooterText:      r.PostFormValue("footer_text"),
		AccentColor:     r.PostFormValue("accent_color"),
		BackgroundColor: r.PostFormValue("background_color"),
	}
	// Checked here as well as in the store so the operator gets the sentence
	// that names the field, not "that did not go through".
	b.Normalise()
	if err := b.Validate(); err != nil {
		h.redirectBranding(w, r, "", userFacing(err))
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.portalBranding.SavePortalBranding(ctx, b); err != nil {
		log.Error().Err(err).Msg("staffui: save portal branding failed")
		h.redirectBranding(w, r, "failed", "")
		return
	}
	h.redirectBranding(w, r, "saved", "")
}

// UploadPortalLogoForm handles POST /staff/portal-branding/logo. The body has
// already been capped and its CSRF token checked by requireCSRFUpload.
func (h *Handler) UploadPortalLogoForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.brandingGate(w, r)
	if !ok {
		return
	}
	file, _, err := r.FormFile("logo")
	if err != nil {
		h.redirectBranding(w, r, "", "Choose an image file to upload.")
		return
	}
	defer file.Close()
	// One byte past the cap, so a file exactly at the limit is accepted and
	// one over it is seen as over rather than silently truncated to fit.
	data, err := io.ReadAll(io.LimitReader(file, hotspot.MaxLogoBytes+1))
	if err != nil {
		h.redirectBranding(w, r, "", "That file could not be read. Try again.")
		return
	}
	// The domain check runs here for its message and again in the store,
	// which is the last point before the bytes are served to the public.
	if _, err := hotspot.ValidateLogo(data); err != nil {
		h.redirectBranding(w, r, "", userFacing(err))
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.portalBranding.SetPortalLogo(ctx, data); err != nil {
		log.Error().Err(err).Msg("staffui: save portal logo failed")
		h.redirectBranding(w, r, "failed", "")
		return
	}
	h.redirectBranding(w, r, "logo_set", "")
}

// ClearPortalLogoForm handles POST /staff/portal-branding/logo/clear.
func (h *Handler) ClearPortalLogoForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.brandingGate(w, r)
	if !ok {
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	if err := h.portalBranding.ClearPortalLogo(ctx); err != nil {
		log.Error().Err(err).Msg("staffui: clear portal logo failed")
		h.redirectBranding(w, r, "failed", "")
		return
	}
	h.redirectBranding(w, r, "logo_cleared", "")
}

// PortalBrandingPreview handles GET /staff/portal-branding/preview — the real
// captive-portal template rendered with the saved branding, for the frame on
// the settings screen. The live portal itself cannot be framed for this: with
// no device address in the URL it deliberately shows no forms, which is not
// what a visitor sees.
func (h *Handler) PortalBrandingPreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.brandingGate(w, r); !ok {
		return
	}
	b, err := h.portalBranding.PortalBranding(r.Context())
	if err != nil {
		http.Error(w, "preview unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	hotspot.RenderPreview(w, b)
}

func (h *Handler) brandingGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "portal-branding")
	if !ok {
		return s, false
	}
	if h.portalBranding == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Captive-portal branding is not configured on this deployment.")
		return s, false
	}
	return s, true
}

func (h *Handler) redirectBranding(w http.ResponseWriter, r *http.Request, code, problem string) {
	url := "/staff/portal-branding"
	switch {
	case problem != "":
		url += "?err=" + neturl.QueryEscape(problem)
	case code != "":
		url += "?m=" + code
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}

// userFacing strips the package prefix from a domain error, so the operator
// reads "the logo must be a PNG or JPEG" rather than "hotspot: the logo...".
// Only ever given the domain's own sentinel errors, whose text is written for
// this purpose; anything else is mapped to the generic failure upstream.
func userFacing(err error) string {
	msg := strings.TrimPrefix(err.Error(), "hotspot: ")
	if msg == "" {
		return msg
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}
