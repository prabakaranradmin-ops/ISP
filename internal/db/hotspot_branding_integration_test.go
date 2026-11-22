//go:build integration

package db_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
)

// Captive-portal branding against a real database — FR-HSP-004 | migration 072.
//
// What needs PostgreSQL rather than a fake: that NULL really is how "use the
// default" round-trips, that the singleton stays one row across saves, that
// saving the text never touches the logo and the reverse, and that the store
// refuses a non-image at the last point before it would be served.

func brandingPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 120, 40))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFR_HSP_004_AnUnsavedDeploymentReadsAsAllDefaults(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Hotspot()

	b, err := store.PortalBranding(ctx)
	if err != nil {
		t.Fatalf("read with nothing saved: %v", err)
	}
	if b != (hotspot.Branding{}) {
		t.Errorf("nothing saved should read as the zero Branding, got %+v", b)
	}
	data, ct, sha, err := store.PortalLogo(ctx)
	if err != nil || data != nil || ct != "" || sha != "" {
		t.Errorf("no logo should read as nil, got %d bytes %q %q %v", len(data), ct, sha, err)
	}
}

func TestFR_HSP_004_TextAndColoursRoundTripAndClearingRestoresTheDefault(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Hotspot()

	// Non-ASCII through pgx, the way the console sends it.
	in := hotspot.Branding{
		VenueName: "அண்ணா நகர் Café", Headline: "Free Wi-Fi", WelcomeText: "Ask at the counter.",
		FooterText: "Open 7am–11pm\r\nHelp: 044-2812 3456", AccentColor: "0B6E4F", BackgroundColor: "#fe7",
	}
	if err := store.SavePortalBranding(ctx, in, "owner"); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := store.PortalBranding(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.VenueName != "அண்ணா நகர் Café" || got.AccentColor != "#0b6e4f" ||
		got.BackgroundColor != "#ffee77" || got.FooterText != "Open 7am–11pm\nHelp: 044-2812 3456" ||
		got.UpdatedBy != "owner" {
		t.Errorf("read back as %+v", got)
	}

	// Clearing a field and saving is how it returns to the default: it must
	// be stored as NULL, not as an empty string the page would print.
	in.Headline, in.AccentColor = "", ""
	if err := store.SavePortalBranding(ctx, in, "owner"); err != nil {
		t.Fatalf("save cleared: %v", err)
	}
	if n := countRows(ctx, t, pool,
		`SELECT count(*) FROM captive_portal_branding WHERE headline IS NULL AND accent_color IS NULL`); n != 1 {
		t.Error("a cleared field was stored as something other than NULL")
	}
	got, _ = store.PortalBranding(ctx)
	if v := got.View(); v.Headline != hotspot.DefaultHeadline || v.Accent != hotspot.DefaultAccentColor {
		t.Errorf("a cleared field did not fall back to the default: %q %q", v.Headline, v.Accent)
	}

	// Two saves, one row.
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM captive_portal_branding`); n != 1 {
		t.Errorf("the singleton holds %d rows", n)
	}
}

func TestFR_HSP_004_TheLogoAndTheTextAreWrittenIndependently(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Hotspot()
	logo := brandingPNG(t)

	// The logo first, on a deployment that has never saved anything — the
	// upsert must create the row.
	if err := store.SetPortalLogo(ctx, logo, "owner"); err != nil {
		t.Fatalf("set logo: %v", err)
	}
	if err := store.SavePortalBranding(ctx, hotspot.Branding{VenueName: "Café"}, "owner"); err != nil {
		t.Fatalf("save text: %v", err)
	}

	data, ct, sha, err := store.PortalLogo(ctx)
	if err != nil {
		t.Fatalf("read logo: %v", err)
	}
	sum := sha256.Sum256(logo)
	if !bytes.Equal(data, logo) || ct != "image/png" || sha != hex.EncodeToString(sum[:]) {
		t.Errorf("saving the text disturbed the logo: %d bytes, %q, %q", len(data), ct, sha)
	}
	b, _ := store.PortalBranding(ctx)
	if !b.HasLogo || b.LogoSHA256 != sha || b.VenueName != "Café" {
		t.Errorf("branding reads as %+v", b)
	}

	// Clearing the logo keeps the text.
	if err := store.ClearPortalLogo(ctx, "owner"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	b, _ = store.PortalBranding(ctx)
	if b.HasLogo || b.VenueName != "Café" {
		t.Errorf("after clearing the logo: %+v", b)
	}
}

// The store re-validates, because it is the last point before the bytes are
// served from the portal's origin to anonymous visitors.
func TestFR_HSP_004_TheStoreRefusesWhatIsNotAPNGOrJPEG(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Hotspot()

	for name, data := range map[string][]byte{
		"an SVG":       []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`),
		"HTML":         []byte("<!doctype html><script>alert(1)</script>"),
		"a bare stamp": []byte("\x89PNG\r\n\x1a\nnot an image"),
	} {
		if err := store.SetPortalLogo(ctx, data, "owner"); err == nil {
			t.Errorf("%s was stored as a logo", name)
		} else if !errors.Is(err, hotspot.ErrLogoType) && !errors.Is(err, hotspot.ErrLogoUnreadable) {
			t.Errorf("%s gave %v", name, err)
		}
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM captive_portal_branding`); n != 0 {
		t.Errorf("a refused logo still created %d row(s)", n)
	}
}

func TestFR_HSP_004_TheStoreRefusesABadColour(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	err := database.Hotspot().SavePortalBranding(ctx, hotspot.Branding{AccentColor: "red"}, "owner")
	if !errors.Is(err, hotspot.ErrBadColor) {
		t.Errorf("a colour name gave %v, want ErrBadColor", err)
	}
}
