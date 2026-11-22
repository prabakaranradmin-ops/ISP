// Captive-portal branding — FR-HSP-004.
//
// Most of what is worth defending here is what branding cannot do: break the
// sign-in page, inject into it, or serve something from the portal's origin
// that is not the image it claims to be. The rest is that an unconfigured
// deployment renders exactly the page it rendered before.
package hotspot

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Fakes ────────────────────────────────────────────────────────────────────

type fakeBrandingStore struct {
	mu        sync.Mutex
	branding  Branding
	logo      []byte
	logoType  string
	err       error
	reads     int
	logoReads int
}

func (s *fakeBrandingStore) PortalBranding(context.Context) (Branding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return Branding{}, s.err
	}
	return s.branding, nil
}

func (s *fakeBrandingStore) PortalLogo(context.Context) ([]byte, string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logoReads++
	if s.err != nil {
		return nil, "", "", s.err
	}
	return s.logo, s.logoType, s.branding.LogoSHA256, nil
}

func brandedPortal(t *testing.T, store BrandingStore) *portalFixture {
	t.Helper()
	f := &portalFixture{
		grants:      &fakeGrants{},
		subscribers: newFakeSubscribers(t, "walkup@isp", "correct-horse", 55),
		limiter:     &fakeLimiter{unlimited: true},
	}
	f.mux = http.NewServeMux()
	NewHandler(Deps{
		Grants: f.grants, Subscribers: f.subscribers, Limiter: f.limiter,
		Branding: NewBrandingCache(store),
	}).RegisterRoutes(f.mux)
	return f
}

func pngLogo(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		img.Set(x, x%h, color.RGBA{R: 200, A: 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const sampleSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func venueBranding() Branding {
	return Branding{
		VenueName:       "Anna Nagar Café",
		Headline:        "Free Wi-Fi for customers",
		WelcomeText:     "Ask at the counter for a voucher.",
		FooterText:      "Open 7am–11pm\nHelp: 044-2812 3456",
		AccentColor:     "#0b6e4f",
		BackgroundColor: "#fff8e7",
	}
}

// ── An unconfigured deployment is unchanged ─────────────────────────────────

// The defaults are the page as it was before FR-HSP-004. A deployment that
// never opens the branding screen — and one where the branding read fails —
// must see the same sign-in page as before, in the same colours.
func TestFR_HSP_004_AnUnconfiguredPortalLooksExactlyAsBefore(t *testing.T) {
	for name, f := range map[string]*portalFixture{
		"no branding wired":     newPortal(t),
		"an empty branding row": brandedPortal(t, &fakeBrandingStore{}),
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.get("/hotspot/portal?mac=aa-bb-cc-dd-ee-ff")
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d", rec.Code)
			}
			body := rec.Body.String()
			for _, want := range []string{
				"<title>Wi-Fi Sign In</title>",
				"<h1>Wi-Fi Sign In</h1>",
				"Enter your voucher code, or sign in with your account.",
				"background: #1a6dcc",
				"background: #f4f5f7",
				"color: #ffffff", // the button label, computed — same white as before
			} {
				if !strings.Contains(body, want) {
					t.Errorf("the default page is missing %q", want)
				}
			}
			// The one deliberate difference. Before FR-HSP-004 the stylesheet
			// never overrode the link colour in dark mode, so the "continue to
			// where you were going" link was #1a6dcc on the #1d2126 card —
			// 3.16:1, below AA. The computed dark link colour now replaces it
			// with the readable text colour.
			if !strings.Contains(body, "a { color: #e8eaed; }") {
				t.Error("the default page's dark-mode links are not the readable text colour")
			}
			if strings.Contains(body, `class="logo"`) || strings.Contains(body, `class="venue"`) ||
				strings.Contains(body, `class="footer"`) {
				t.Error("an unconfigured page renders branding elements with nothing in them")
			}
		})
	}
}

// ── Branding reaches every page the visitor can see ─────────────────────────

func TestFR_HSP_004_TheVenuesBrandingIsOnTheLandingPage(t *testing.T) {
	store := &fakeBrandingStore{branding: venueBranding()}
	store.branding.HasLogo, store.branding.LogoContentType, store.branding.LogoSHA256 = true, "image/png", sampleSHA
	store.logo, store.logoType = pngLogo(t, 64, 32), "image/png"
	f := brandedPortal(t, store)

	body := f.get("/hotspot/portal?mac=aa-bb-cc-dd-ee-ff").Body.String()
	for _, want := range []string{
		"<title>Anna Nagar Café</title>",
		`<p class="venue">Anna Nagar Café</p>`,
		"<h1>Free Wi-Fi for customers</h1>",
		"Ask at the counter for a voucher.",
		"Open 7am–11pm\nHelp: 044-2812 3456",
		"background: #0b6e4f",
		"background: #fff8e7",
		`src="/hotspot/logo?v=0123456789abcdef"`,
		`alt="Anna Nagar Café"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the branded page is missing %q", want)
		}
	}
	if strings.Contains(body, "Wi-Fi Sign In") {
		t.Error("the default headline survives alongside the venue's own")
	}
}

// Every refusal page renders through the same helper, so none of them can
// come back in the default colours while the page before it wore the venue's.
// Checked on the voucher refusal and the incomplete-form rejection, the two a
// visitor actually sees.
func TestFR_HSP_004_RefusalsAndTheSuccessPageCarryTheBrandingToo(t *testing.T) {
	store := &fakeBrandingStore{branding: venueBranding()}
	f := brandedPortal(t, store)

	refused := f.post("/hotspot/voucher", url.Values{"mac": {"aa-bb-cc-dd-ee-ff"}, "code": {"HS-NOPE-NOPE-NOPE"}})
	incomplete := f.post("/hotspot/voucher", url.Values{"mac": {"aa-bb-cc-dd-ee-ff"}})
	granted := f.post("/hotspot/login", url.Values{
		"mac": {"aa-bb-cc-dd-ee-ff"}, "username": {"walkup@isp"}, "password": {"correct-horse"},
	})
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"a refused voucher": refused, "an incomplete form": incomplete, "a granted login": granted,
	} {
		body := rec.Body.String()
		if !strings.Contains(body, "Anna Nagar Café") || !strings.Contains(body, "background: #0b6e4f") {
			t.Errorf("%s (status %d) is not branded", name, rec.Code)
		}
	}
	if granted.Code != http.StatusOK || !strings.Contains(granted.Body.String(), "You're connected") {
		t.Fatalf("the login did not reach the success page: %d", granted.Code)
	}
}

// ── What branding cannot do ──────────────────────────────────────────────────

// Text is escaped like any other value; a venue name is never markup.
func TestFR_HSP_004_BrandingTextCannotInjectMarkup(t *testing.T) {
	store := &fakeBrandingStore{branding: Branding{
		VenueName:   `<script>alert(1)</script>`,
		Headline:    `"><img src=x onerror=alert(2)>`,
		WelcomeText: `<b>bold</b>`,
		FooterText:  `</p><a href="javascript:alert(3)">x</a>`,
	}}
	body := brandedPortal(t, store).get("/hotspot/portal?mac=aa-bb-cc-dd-ee-ff").Body.String()
	for _, bad := range []string{"<script>alert", "<img src=x", "<b>bold</b>", `<a href="javascript:`} {
		if strings.Contains(body, bad) {
			t.Errorf("branding text reached the page unescaped: %q", bad)
		}
	}
}

// A colour that is not a strict #rrggbb — however it got into the table —
// renders as the default colour. The page's safety does not rest on
// html/template's CSS escaper alone.
func TestFR_HSP_004_ABadStoredColourFallsBackRatherThanReachingTheStylesheet(t *testing.T) {
	store := &fakeBrandingStore{branding: Branding{
		AccentColor:     `red;}</style><script>alert(1)</script><style>a{`,
		BackgroundColor: `expression(alert(1))`,
	}}
	body := brandedPortal(t, store).get("/hotspot/portal?mac=aa-bb-cc-dd-ee-ff").Body.String()
	if strings.Contains(body, "<script>alert") || strings.Contains(body, "expression(") {
		t.Fatal("a stored colour broke out of the stylesheet")
	}
	if !strings.Contains(body, "background: #1a6dcc") || !strings.Contains(body, "background: #f4f5f7") {
		t.Error("an invalid stored colour did not fall back to the default")
	}
}

// ── A broken branding store never breaks sign-in ────────────────────────────

func TestFR_HSP_004_ABrandingReadFailureStillServesTheSignInPage(t *testing.T) {
	store := &fakeBrandingStore{err: errors.New("database unavailable")}
	f := brandedPortal(t, store)

	rec := f.get("/hotspot/portal?mac=aa-bb-cc-dd-ee-ff")
	if rec.Code != http.StatusOK {
		t.Fatalf("a branding read failure turned the sign-in page into a %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="/hotspot/voucher"`) || !strings.Contains(body, "Wi-Fi Sign In") {
		t.Error("the default sign-in page was not served when branding could not be read")
	}
}

func TestFR_HSP_004_TheCacheKeepsTheLastGoodCopyAndRetriesFailures(t *testing.T) {
	store := &fakeBrandingStore{branding: venueBranding()}
	c := NewBrandingCache(store)
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	if got := c.Get(ctx).VenueName; got != "Anna Nagar Café" {
		t.Fatalf("first read: %q", got)
	}
	// Within the TTL the store is not asked again — the portal is probed by
	// every joining phone and branding changes a few times a year.
	c.Get(ctx)
	c.Get(ctx)
	if store.reads != 1 {
		t.Errorf("the store was read %d times within the TTL, want 1", store.reads)
	}

	// The store fails after expiry: the last good copy is served, not the
	// defaults, and the failure is not cached as a success.
	store.err = errors.New("down")
	now = now.Add(BrandingCacheTTL + time.Second)
	if got := c.Get(ctx).VenueName; got != "Anna Nagar Café" {
		t.Errorf("after a failed refresh the venue became %q — the last good copy was dropped", got)
	}
	readsAfterFailure := store.reads
	c.Get(ctx)
	if store.reads == readsAfterFailure {
		t.Error("a failed read was cached, so the store is not retried until the TTL passes")
	}

	// Invalidate makes a save visible on the next request.
	store.err = nil
	store.branding.VenueName = "Renamed Café"
	c.Invalidate()
	if got := c.Get(ctx).VenueName; got != "Renamed Café" {
		t.Errorf("after Invalidate the cache still served %q", got)
	}
}

// The logo is fetched only when its hash changes, so a routine refresh costs
// one small row rather than up to 100 KB.
func TestFR_HSP_004_TheLogoIsOnlyRefetchedWhenItChanges(t *testing.T) {
	store := &fakeBrandingStore{branding: venueBranding()}
	store.branding.HasLogo, store.branding.LogoSHA256 = true, sampleSHA
	store.logo, store.logoType = pngLogo(t, 32, 32), "image/png"
	c := NewBrandingCache(store)
	now := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return now }

	c.Get(context.Background())
	now = now.Add(BrandingCacheTTL + time.Second)
	c.Get(context.Background())
	if store.logoReads != 1 {
		t.Errorf("an unchanged logo was fetched %d times, want 1", store.logoReads)
	}
}

// ── The logo endpoint ────────────────────────────────────────────────────────

func TestFR_HSP_004_TheLogoIsServedAsTheImageItWasSniffedAs(t *testing.T) {
	logo := pngLogo(t, 64, 32)
	store := &fakeBrandingStore{branding: Branding{HasLogo: true, LogoContentType: "image/png", LogoSHA256: sampleSHA}}
	store.logo, store.logoType = logo, "image/png"
	f := brandedPortal(t, store)

	rec := f.get("/hotspot/logo?v=0123456789abcdef")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type %q, want image/png", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the logo is served without nosniff, so a browser may second-guess its type")
	}
	if !bytes.Equal(rec.Body.Bytes(), logo) {
		t.Error("the bytes served are not the logo")
	}

	// A conditional request for the current version is a 304, so a phone that
	// already has it does not download it again over the venue's uplink.
	req := httptest.NewRequest(http.MethodGet, "/hotspot/logo?v=0123456789abcdef", nil) //nolint:noctx // matches get()
	req.Header.Set("If-None-Match", `"`+sampleSHA+`"`)
	cached := httptest.NewRecorder()
	f.mux.ServeHTTP(cached, req)
	if cached.Code != http.StatusNotModified {
		t.Errorf("a matching If-None-Match got %d, want 304", cached.Code)
	}
}

func TestFR_HSP_004_NoLogoIsANotFoundNotABrokenImage(t *testing.T) {
	for name, f := range map[string]*portalFixture{
		"no branding wired": newPortal(t),
		"no logo uploaded":  brandedPortal(t, &fakeBrandingStore{branding: venueBranding()}),
	} {
		if rec := f.get("/hotspot/logo"); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d, want 404", name, rec.Code)
		}
	}
}

// ── Domain rules ─────────────────────────────────────────────────────────────

func TestFR_HSP_004_ColoursArriveInTheShapesABrandGuideUses(t *testing.T) {
	cases := []struct {
		in, want string
		err      error
	}{
		{"#1A6DCC", "#1a6dcc", nil},
		{"1a6dcc", "#1a6dcc", nil},
		{"#16c", "#1166cc", nil},
		{"  #0B6E4F ", "#0b6e4f", nil},
		{"", "", nil},
		{"red", "", ErrBadColor},
		{"rgb(1,2,3)", "", ErrBadColor},
		{"#12345", "", ErrBadColor},
		{"#1a6dcc;", "", ErrBadColor},
	}
	for _, tc := range cases {
		got, err := NormaliseColor(tc.in)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("NormaliseColor(%q) = %q, %v; want %q, %v", tc.in, got, err, tc.want, tc.err)
		}
	}
}

func TestFR_HSP_004_ValidateNamesWhatIsWrong(t *testing.T) {
	long := func(n int) string { return strings.Repeat("அ", n) }
	cases := []struct {
		name string
		b    Branding
		want error
	}{
		{"all empty is valid — every field defaults", Branding{}, nil},
		// Characters, not bytes: 80 Tamil characters are 240 bytes.
		{"a full-length Tamil venue name", Branding{VenueName: long(MaxVenueName)}, nil},
		{"a venue name one too long", Branding{VenueName: long(MaxVenueName + 1)}, ErrVenueNameTooLong},
		{"a headline too long", Branding{Headline: long(MaxHeadline + 1)}, ErrHeadlineTooLong},
		{"welcome text too long", Branding{WelcomeText: long(MaxWelcomeText + 1)}, ErrWelcomeTextTooLong},
		{"footer too long", Branding{FooterText: long(MaxFooterText + 1)}, ErrFooterTextTooLong},
		{"a colour name", Branding{AccentColor: "green"}, ErrBadColor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.b
			b.Normalise()
			if err := b.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A brand colour picks its own button label. A yellow accent with white text
// would be a sign-in page whose one button cannot be read.
func TestFR_HSP_004_TheButtonLabelIsAlwaysReadable(t *testing.T) {
	for _, accent := range []string{"#ffeb3b", "#ffffff", "#9be7ff", "#1a6dcc", "#0b1f3a", "#000000", "#e53935"} {
		v := Branding{AccentColor: accent}.View()
		if r := ContrastRatio(v.Accent, v.AccentText); r < 4.5 {
			t.Errorf("accent %s gets label %s at contrast %.2f, below AA", accent, v.AccentText, r)
		}
	}
	if (Branding{AccentColor: "#ffeb3b"}).View().AccentText == "#ffffff" {
		t.Error("a yellow button got a white label")
	}
}

// Links sit on the card, so a pale accent is replaced by the text colour for
// links — and the console says so rather than leaving the operator to wonder.
func TestFR_HSP_004_ALinkColourTooFaintToReadIsReplacedAndReported(t *testing.T) {
	pale := Branding{AccentColor: "#ffeb3b"}
	if v := pale.View(); v.LinkLight != textLight {
		t.Errorf("a pale accent was used for links on the white card: %s", v.LinkLight)
	}
	if pale.LinkWarning() == "" {
		t.Error("a pale accent produced no warning for the console")
	}

	deep := Branding{AccentColor: "#0b1f3a"}
	if v := deep.View(); v.LinkLight != "#0b1f3a" || v.LinkDark != textDark {
		t.Errorf("a navy accent: light links %s (want the accent), dark links %s (want the text colour)",
			v.LinkLight, v.LinkDark)
	}
	if !strings.Contains(deep.LinkWarning(), "dark mode") {
		t.Errorf("a navy accent's warning does not mention dark mode: %q", deep.LinkWarning())
	}

	if ContrastRatio("#000000", "#ffffff") < 20.9 {
		t.Error("the contrast formula does not give 21:1 for black on white")
	}
}

// ── Logo validation ──────────────────────────────────────────────────────────

func TestFR_HSP_004_OnlyARealPNGOrJPEGIsAcceptedAsALogo(t *testing.T) {
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 40, 40)), nil); err != nil {
		t.Fatal(err)
	}
	pngSig := []byte("\x89PNG\r\n\x1a\n")
	oversized := append(append([]byte{}, pngSig...), make([]byte, MaxLogoBytes)...)

	cases := []struct {
		name     string
		data     []byte
		wantType string
		wantErr  error
	}{
		{"a PNG", pngLogo(t, 120, 40), "image/png", nil},
		{"a JPEG", jpg.Bytes(), "image/jpeg", nil},
		// The format a designer is most likely to send, and the one that can
		// carry script.
		{"an SVG", []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><rect/></svg>`), "", ErrLogoType},
		// Named logo.png by the uploader; the bytes decide, not the name.
		{"HTML posing as a PNG", []byte("<!doctype html><script>alert(1)</script>"), "", ErrLogoType},
		{"a PNG signature on garbage", append(append([]byte{}, pngSig...), []byte("not really an image at all")...), "", ErrLogoUnreadable},
		{"a tracking pixel", pngLogo(t, 1, 1), "", ErrLogoDimensions},
		{"a canvas too wide", pngLogo(t, MaxLogoSide+1, 16), "", ErrLogoDimensions},
		{"over the size cap", oversized, "", ErrLogoTooLarge},
		{"empty", nil, "", ErrLogoEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateLogo(tc.data)
			if !errors.Is(err, tc.wantErr) || got != tc.wantType {
				t.Errorf("got %q, %v; want %q, %v", got, err, tc.wantType, tc.wantErr)
			}
		})
	}
}
