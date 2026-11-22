package staffui

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Captive-portal branding in the console — FR-HSP-004.

type fakeBranding struct {
	b       hotspot.Branding
	err     error
	saved   *hotspot.Branding
	logo    []byte
	cleared bool
	who     string
	calls   int
}

func (f *fakeBranding) PortalBranding(context.Context) (hotspot.Branding, error) { return f.b, f.err }

func (f *fakeBranding) SavePortalBranding(ctx context.Context, b hotspot.Branding) error {
	f.calls++
	f.saved, f.who = &b, middleware.SubjectFromContext(ctx)
	return f.err
}

func (f *fakeBranding) SetPortalLogo(ctx context.Context, data []byte) error {
	f.calls++
	f.logo, f.who = data, middleware.SubjectFromContext(ctx)
	return f.err
}

func (f *fakeBranding) ClearPortalLogo(ctx context.Context) error {
	f.calls++
	f.cleared, f.who = true, middleware.SubjectFromContext(ctx)
	return f.err
}

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func errParam(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	return loc.Query().Get("err")
}

// ── Reach ────────────────────────────────────────────────────────────────────

// Owner only. The page is the business speaking in public under the venue's
// name; a NOC engineer runs the hotspot but has no reason to change its
// wording.
func TestFR_HSP_004_OnlyTheOwnerReachesTheBrandingScreen(t *testing.T) {
	for _, role := range []string{"isp_owner", "billing_admin", "csr", "noc_engineer",
		"technician", "lco", "franchise_admin", "intern", ""} {
		var in bool
		for _, sec := range AllowedSections(role, false) {
			if sec.Key == "portal-branding" {
				in = true
			}
		}
		if in != (role == "isp_owner") {
			t.Errorf("role %q: in the Captive Portal section = %v", role, in)
		}
	}

	h := &Handler{portalBranding: &fakeBranding{}}
	if w := servePR(h, "noc_engineer", "GET", "/staff/portal-branding", nil, h.PortalBranding); w.Code != http.StatusForbidden {
		t.Errorf("a NOC engineer typing the URL got %d, want 403", w.Code)
	}
}

func TestFR_HSP_004_AnUnwiredConsoleSaysSo(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/portal-branding", nil, h.PortalBranding)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}

// ── Screen ───────────────────────────────────────────────────────────────────

func TestFR_HSP_004_TheScreenRendersWithRealData(t *testing.T) {
	for name, b := range map[string]hotspot.Branding{
		"nothing saved": {},
		"fully branded, with a logo and a pale accent": {
			VenueName: "Anna Nagar Café", Headline: "Free Wi-Fi", WelcomeText: "Ask at the counter.",
			FooterText: "Open 7am–11pm", AccentColor: "#ffeb3b", BackgroundColor: "#fff8e7",
			HasLogo: true, LogoContentType: "image/png",
			LogoSHA256: strings.Repeat("ab", 32), UpdatedBy: "owner_user", UpdatedAt: time.Now(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := &Handler{portalBranding: &fakeBranding{b: b}}
			w := servePR(h, "isp_owner", "GET", "/staff/portal-branding", nil, h.PortalBranding)
			if w.Code != http.StatusOK {
				t.Fatalf("got %d: %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if strings.Contains(body, "<no value>") {
				t.Error("a template field name is wrong — the page rendered <no value>")
			}
			for _, want := range []string{
				`action="/staff/portal-branding"`, `enctype="multipart/form-data"`,
				`/staff/portal-branding/preview?v=`, `placeholder="Wi-Fi Sign In"`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("the screen is missing %q", want)
				}
			}
			if b.VenueName != "" {
				if !strings.Contains(body, `value="Anna Nagar Café"`) {
					t.Error("the saved venue name is not in the form")
				}
				// A pale accent will not be used for links, and the operator
				// is told so rather than left to wonder.
				if !strings.Contains(body, "data-link-warning") {
					t.Error("a pale accent shows no warning")
				}
				if !strings.Contains(body, "Remove logo") {
					t.Error("a saved logo offers no way to remove it")
				}
			} else if strings.Contains(body, "data-link-warning") {
				t.Error("the default accent shows a contrast warning")
			}
		})
	}
}

// The preview is the real portal template with the saved branding and the
// forms showing — not an approximation of it.
func TestFR_HSP_004_ThePreviewIsTheRealPortalWithTheFormsShowing(t *testing.T) {
	h := &Handler{portalBranding: &fakeBranding{b: hotspot.Branding{VenueName: "Anna Nagar Café", AccentColor: "#0b6e4f"}}}
	w := servePR(h, "isp_owner", "GET", "/staff/portal-branding/preview", nil, h.PortalBrandingPreview)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Anna Nagar Café", "background: #0b6e4f", `action="/hotspot/voucher"`, `action="/hotspot/login"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the preview is missing %q", want)
		}
	}
	if w.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Error("the preview does not allow itself to be framed by the console")
	}
}

// ── Saving ───────────────────────────────────────────────────────────────────

func TestFR_HSP_004_ASaveIsNormalisedAndFiledAgainstTheOperator(t *testing.T) {
	f := &fakeBranding{}
	h := &Handler{portalBranding: f}
	w := servePR(h, "isp_owner", "POST", "/staff/portal-branding", url.Values{
		"venue_name": {"  Anna Nagar Café "}, "accent_color": {"0B6E4F"}, "background_color": {"#fe7"},
	}, h.SavePortalBrandingForm)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("got %d", w.Code)
	}
	if f.who != "isp_owner_user" {
		t.Errorf("filed by %q, want the session's own username", f.who)
	}
	if f.saved == nil || f.saved.VenueName != "Anna Nagar Café" || f.saved.AccentColor != "#0b6e4f" ||
		f.saved.BackgroundColor != "#ffee77" {
		t.Errorf("reached the store as %+v", f.saved)
	}
}

// A bad colour is refused before the store with the sentence that says what
// shape a colour takes.
func TestFR_HSP_004_ABadColourIsRefusedWithTheShapeItShouldHave(t *testing.T) {
	f := &fakeBranding{}
	h := &Handler{portalBranding: f}
	w := servePR(h, "isp_owner", "POST", "/staff/portal-branding",
		url.Values{"accent_color": {"green"}}, h.SavePortalBrandingForm)
	if f.calls != 0 {
		t.Error("an invalid colour reached the store")
	}
	msg := errParam(t, w)
	if !strings.Contains(msg, "#1a6dcc") || strings.HasPrefix(msg, "hotspot:") {
		t.Errorf("the message is %q — it should show an example and not carry the package prefix", msg)
	}
}

// ── Uploading ────────────────────────────────────────────────────────────────

// uploadRequest builds a real multipart POST carrying file under "logo".
func uploadRequest(t *testing.T, token string, file []byte, extra int) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("csrf_token", token)
	part, err := mw.CreateFormFile("logo", "logo.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(file)
	if extra > 0 {
		_ = mw.WriteField("padding", strings.Repeat("x", extra))
	}
	_ = mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/staff/portal-branding/logo", &body) //nolint:noctx // test
	r.Header.Set("Content-Type", mw.FormDataContentType())
	s := Session{Username: "isp_owner_user", Role: "isp_owner", Token: "session-jwt"}
	return r.WithContext(context.WithValue(r.Context(), sessionKey, s))
}

// The wrapper caps the body before parsing and checks the token straight
// after, so neither an oversized body nor a forged token reaches the handler.
func TestFR_HSP_004_TheUploadWrapperCapsTheBodyAndChecksTheToken(t *testing.T) {
	h := &Handler{jwtSecret: "test-secret-for-the-upload-wrapper", portalBranding: &fakeBranding{}}
	good := h.csrfToken("session-jwt")

	var reached int
	next := func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(http.StatusNoContent) }
	wrapped := h.requireCSRFUpload(brandingUploadLimit, "/staff/portal-branding", next)

	t.Run("a valid upload reaches the handler", func(t *testing.T) {
		reached = 0
		w := httptest.NewRecorder()
		wrapped(w, uploadRequest(t, good, testPNG(t, 64, 32), 0))
		if reached != 1 {
			t.Fatalf("a valid upload did not reach the handler (status %d)", w.Code)
		}
	})

	t.Run("a forged token is refused", func(t *testing.T) {
		reached = 0
		w := httptest.NewRecorder()
		wrapped(w, uploadRequest(t, "forged", testPNG(t, 64, 32), 0))
		if reached != 0 || w.Code != http.StatusForbidden {
			t.Errorf("a forged token got %d and reached the handler %d time(s)", w.Code, reached)
		}
	})

	t.Run("an oversized body is turned away before the handler", func(t *testing.T) {
		reached = 0
		w := httptest.NewRecorder()
		wrapped(w, uploadRequest(t, good, testPNG(t, 64, 32), brandingUploadLimit))
		if reached != 0 {
			t.Error("an oversized body reached the handler")
		}
		if w.Code != http.StatusSeeOther || !strings.Contains(errParam(t, w), "too large") {
			t.Errorf("an oversized body got %d %q, want a redirect that says it was too large",
				w.Code, errParam(t, w))
		}
	})
}

func TestFR_HSP_004_TheUploadHandlerStoresOnlyARealImage(t *testing.T) {
	h := &Handler{jwtSecret: "test-secret-for-the-upload-wrapper"}
	token := h.csrfToken("session-jwt")
	serve := func(f *fakeBranding, file []byte) *httptest.ResponseRecorder {
		h.portalBranding = f
		w := httptest.NewRecorder()
		h.requireCSRFUpload(brandingUploadLimit, "/staff/portal-branding", h.UploadPortalLogoForm)(
			w, uploadRequest(t, token, file, 0))
		return w
	}

	t.Run("a PNG is stored against the operator", func(t *testing.T) {
		f := &fakeBranding{}
		logo := testPNG(t, 64, 32)
		w := serve(f, logo)
		if w.Code != http.StatusSeeOther || !bytes.Equal(f.logo, logo) || f.who != "isp_owner_user" {
			t.Errorf("got %d, stored %d bytes by %q", w.Code, len(f.logo), f.who)
		}
	})

	t.Run("an SVG is refused, and the message says why", func(t *testing.T) {
		f := &fakeBranding{}
		w := serve(f, []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`))
		if f.calls != 0 {
			t.Error("an SVG reached the store")
		}
		if msg := errParam(t, w); !strings.Contains(msg, "SVG") {
			t.Errorf("the refusal %q does not explain the SVG rule", msg)
		}
	})
}
