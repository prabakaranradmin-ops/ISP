package hotspot

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // registers the JPEG decoder for image.DecodeConfig
	_ "image/png"  // registers the PNG decoder for image.DecodeConfig
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Captive-portal branding — FR-HSP-004 | migration 072.
//
// The venue's name, the page's text, two colours and a logo, edited from the
// console and applied to the walled-garden page without touching a template.
// Every field is optional; an unset one falls back to the page as it looked
// before this existed, so a deployment that never opens the screen sees no
// change at all.

// Field limits, in characters, matching migration 072. Counted in runes for
// the reason every other limit in this codebase is: VARCHAR counts
// characters, and a venue name in Tamil is three bytes a character.
const (
	MaxVenueName   = 80
	MaxHeadline    = 120
	MaxWelcomeText = 500
	MaxFooterText  = 1000

	// MaxLogoBytes matches chk_cpb_logo_size. The page is loaded over the
	// venue's shared uplink by a phone that is not on the network yet, and
	// the operating system's sign-in sheet gives up on a slow page.
	MaxLogoBytes = 100 * 1024
	// MaxLogoSide bounds the decoded dimensions. A small file can still
	// declare an enormous canvas, and a phone would allocate for it.
	MaxLogoSide = 2048
	// MinLogoSide refuses a tracking-pixel-sized image, which is never a logo
	// and is almost always an upload of the wrong file.
	MinLogoSide = 16
)

// The defaults are what portal.html showed before FR-HSP-004, so an empty
// branding row renders the page exactly as it was.
const (
	DefaultHeadline        = "Wi-Fi Sign In"
	DefaultWelcomeText     = "Enter your voucher code, or sign in with your account."
	DefaultAccentColor     = "#1a6dcc"
	DefaultBackgroundColor = "#f4f5f7"

	// The card the text sits on, in each colour scheme. Contrast is measured
	// against these, not against the page background, because nothing but
	// the card itself sits on the background.
	cardLight = "#ffffff"
	cardDark  = "#1d2126"
	// Text colours that are always readable on those cards, used when the
	// accent is not.
	textLight = "#1c1f23"
	textDark  = "#e8eaed"
)

var (
	ErrVenueNameTooLong   = errors.New("hotspot: the venue name is too long")
	ErrHeadlineTooLong    = errors.New("hotspot: the headline is too long")
	ErrWelcomeTextTooLong = errors.New("hotspot: the welcome text is too long")
	ErrFooterTextTooLong  = errors.New("hotspot: the footer text is too long")
	// ErrBadColor names the accepted shapes, because "invalid colour" alone
	// leaves somebody guessing whether rgb() or a colour name would work.
	ErrBadColor = errors.New("hotspot: a colour is a hex code such as #1a6dcc or #16c")

	ErrLogoTooLarge = fmt.Errorf("hotspot: the logo is over %d KB — export it smaller; a sign-in page loads over the venue's own connection", MaxLogoBytes/1024)
	ErrLogoEmpty    = errors.New("hotspot: the logo file is empty")
	// ErrLogoType deliberately says why SVG is refused, since it is the
	// format a designer is most likely to send.
	ErrLogoType       = errors.New("hotspot: the logo must be a PNG or JPEG — SVG is refused because it can carry script, and this page is served to anonymous visitors")
	ErrLogoUnreadable = errors.New("hotspot: the logo could not be read as an image — the file may be damaged, or its contents may not match its extension")
	ErrLogoDimensions = fmt.Errorf("hotspot: the logo must be between %d and %d pixels on each side", MinLogoSide, MaxLogoSide)
)

// Branding is the stored configuration. Empty strings are "not set".
type Branding struct {
	VenueName       string
	Headline        string
	WelcomeText     string
	FooterText      string
	AccentColor     string
	BackgroundColor string

	// The logo's bytes are not carried here: the page links it by hash and
	// it is fetched separately, so rendering the page never loads it.
	HasLogo         bool
	LogoContentType string
	LogoSHA256      string

	UpdatedBy string
	UpdatedAt time.Time
}

var hexColor = regexp.MustCompile(`^#?([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// NormaliseColor turns an operator's colour into the stored #rrggbb form, or
// returns ErrBadColor. Accepts a missing '#' and the three-digit shorthand,
// because that is how colours arrive copied out of a brand guide, and
// lower-cases the result to match chk_cpb_accent_color. Empty stays empty:
// it means "use the default".
func NormaliseColor(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if !hexColor.MatchString(s) {
		return "", ErrBadColor
	}
	s = strings.ToLower(strings.TrimPrefix(s, "#"))
	if len(s) == 3 {
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	}
	return "#" + s, nil
}

// Normalise trims every text field and normalises both colours. A colour that
// cannot be normalised is left as typed, for Validate to refuse with a
// message.
func (b *Branding) Normalise() {
	b.VenueName = strings.TrimSpace(b.VenueName)
	b.Headline = strings.TrimSpace(b.Headline)
	b.WelcomeText = strings.TrimSpace(b.WelcomeText)
	// Line breaks inside the footer are kept: it is where opening hours and a
	// support number go, one to a line.
	b.FooterText = strings.TrimSpace(strings.ReplaceAll(b.FooterText, "\r\n", "\n"))
	if c, err := NormaliseColor(b.AccentColor); err == nil {
		b.AccentColor = c
	}
	if c, err := NormaliseColor(b.BackgroundColor); err == nil {
		b.BackgroundColor = c
	}
}

// Validate reports the first thing wrong with a normalised Branding.
func (b Branding) Validate() error {
	switch {
	case utf8.RuneCountInString(b.VenueName) > MaxVenueName:
		return ErrVenueNameTooLong
	case utf8.RuneCountInString(b.Headline) > MaxHeadline:
		return ErrHeadlineTooLong
	case utf8.RuneCountInString(b.WelcomeText) > MaxWelcomeText:
		return ErrWelcomeTextTooLong
	case utf8.RuneCountInString(b.FooterText) > MaxFooterText:
		return ErrFooterTextTooLong
	}
	for _, c := range []string{b.AccentColor, b.BackgroundColor} {
		if c == "" {
			continue
		}
		if n, err := NormaliseColor(c); err != nil || n != c {
			return ErrBadColor
		}
	}
	return nil
}

// ValidateLogo checks an uploaded logo and returns the content type it will be
// served with.
//
// The type is decided from the bytes, never from the upload's own header or
// file name: both are the uploader's claim, and a file named logo.png that is
// actually HTML would otherwise be served from the portal's origin as
// whatever it said it was. The image is then decoded far enough to read its
// dimensions, which proves it is an image of that type rather than something
// with a PNG signature stuck on the front.
func ValidateLogo(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrLogoEmpty
	}
	if len(data) > MaxLogoBytes {
		return "", ErrLogoTooLarge
	}
	sniffed := http.DetectContentType(data)
	var want string
	switch sniffed {
	case "image/png":
		want = "png"
	case "image/jpeg":
		want = "jpeg"
	default:
		return "", ErrLogoType
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != want {
		return "", ErrLogoUnreadable
	}
	if cfg.Width < MinLogoSide || cfg.Height < MinLogoSide ||
		cfg.Width > MaxLogoSide || cfg.Height > MaxLogoSide {
		return "", ErrLogoDimensions
	}
	return sniffed, nil
}

// ── Colour arithmetic ────────────────────────────────────────────────────────

// relativeLuminance is the WCAG 2.x definition, for a normalised #rrggbb.
func relativeLuminance(hex string) float64 {
	channel := func(s string) float64 {
		var v int
		_, _ = fmt.Sscanf(s, "%02x", &v)
		c := float64(v) / 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	r, g, b := channel(hex[1:3]), channel(hex[3:5]), channel(hex[5:7])
	return 0.2126*r + 0.7152*g + 0.0722*b
}

// ContrastRatio is the WCAG contrast ratio between two #rrggbb colours, from 1
// (identical) to 21 (black on white).
func ContrastRatio(a, b string) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// minTextContrast is WCAG AA for body-size text.
const minTextContrast = 4.5

// textOn picks whichever of near-black and white reads better on bg.
//
// The button's label is chosen rather than configured. A venue whose brand
// colour is yellow would otherwise get white text on a yellow button — a
// sign-in page whose one button cannot be read — and nobody setting a brand
// colour thinks to set a second colour for the words on it.
//
// Pure black, not a softer near-black. The two candidates give equal
// contrast at a luminance where both are about 4.58:1, and that crossover is
// the worst case anywhere on the scale — so black and white between them
// always reach AA. A near-black like #111111 lowers the crossover below 4.5,
// and mid-tone reds such as #e53935 then get a label that fails with either
// choice. Found by TestFR_HSP_004_TheButtonLabelIsAlwaysReadable.
func textOn(bg string) string {
	if ContrastRatio(bg, "#ffffff") >= ContrastRatio(bg, "#000000") {
		return "#ffffff"
	}
	return "#000000"
}

// readableOn returns accent when it meets AA against card, and fallback
// otherwise. Used for links, which sit directly on the card: a pale brand
// colour makes a link that cannot be seen, and the link on the "you're
// connected" page is the one a visitor most needs to find.
func readableOn(accent, card, fallback string) string {
	if ContrastRatio(accent, card) >= minTextContrast {
		return accent
	}
	return fallback
}

// ── What the template sees ───────────────────────────────────────────────────

// brandView is Branding with every default filled in and every derived colour
// computed, so the template only ever prints values and never decides.
type brandView struct {
	// Title is the <title> and the browser tab: the venue's name when set,
	// since that is what a visitor is looking for in a list of networks.
	Title       string
	VenueName   string
	Headline    string
	WelcomeText string
	FooterText  string

	Accent     string
	AccentText string
	LinkLight  string
	LinkDark   string
	Background string

	// LogoURL is empty when there is no logo. Keyed by the logo's hash, so a
	// replaced logo is a new URL rather than a stale cache entry.
	LogoURL string
}

// View resolves defaults and derived colours.
func (b Branding) View() brandView {
	v := brandView{
		VenueName:   b.VenueName,
		Headline:    orDefault(b.Headline, DefaultHeadline),
		WelcomeText: orDefault(b.WelcomeText, DefaultWelcomeText),
		FooterText:  b.FooterText,
		Accent:      safeColor(b.AccentColor, DefaultAccentColor),
		Background:  safeColor(b.BackgroundColor, DefaultBackgroundColor),
	}
	v.Title = orDefault(b.VenueName, DefaultHeadline)
	v.AccentText = textOn(v.Accent)
	v.LinkLight = readableOn(v.Accent, cardLight, textLight)
	v.LinkDark = readableOn(v.Accent, cardDark, textDark)
	if b.HasLogo && b.LogoSHA256 != "" {
		v.LogoURL = "/hotspot/logo?v=" + b.LogoSHA256[:16]
	}
	return v
}

// LinkWarning returns a sentence for the console when the accent will not be
// used for links in one of the colour schemes, or "" when it will be. The
// page silently substitutes a readable colour either way; this is so the
// operator knows that is happening rather than wondering why their brand
// colour is not on the links.
//
// Silent when no accent has been set. The default accent is itself too dark
// for dark-mode links (3.16:1 on the dark card), and the page substitutes for
// it like any other — but that is this codebase's colour, not the operator's,
// and telling them their choice is unreadable when they made no choice would
// send them looking for a setting they never touched.
func (b Branding) LinkWarning() string {
	if b.AccentColor == "" {
		return ""
	}
	accent := safeColor(b.AccentColor, DefaultAccentColor)
	light := ContrastRatio(accent, cardLight) >= minTextContrast
	dark := ContrastRatio(accent, cardDark) >= minTextContrast
	switch {
	case light && dark:
		return ""
	case !light && !dark:
		return "This accent colour is too faint to read as link text in either light or dark mode, so links will use the ordinary text colour. Buttons still use it."
	case !light:
		return "This accent colour is too pale to read as link text on the white card, so in light mode links will use the ordinary text colour. Buttons still use it."
	default:
		return "This accent colour is too dark to read as link text in dark mode, so phones in dark mode will show links in the ordinary text colour. Buttons still use it."
	}
}

// safeColor re-validates a stored colour on the way to the page and falls back
// to def if it does not normalise to itself.
//
// The value goes into an inline <style> block. It has already passed
// Validate and chk_cpb_*_color, and html/template's CSS escaper sits behind
// this — but a colour written into the table by hand, or by a migration that
// forgot the constraint, should produce the default colour, not a page whose
// safety rests on an escaper's behaviour in a context it was not written for.
func safeColor(stored, def string) string {
	if n, err := NormaliseColor(stored); err == nil && n != "" && n == stored {
		return n
	}
	return def
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
