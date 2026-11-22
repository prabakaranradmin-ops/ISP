package vendors_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// The vendor master's domain rules — FR-INV-004.

// The check-digit algorithm, pinned against the GST registry's own published
// example. This test is the reason the checksum is enforced rather than
// merely warned about: without it the implementation would be a transcribed
// algorithm, and refusing a correct GSTIN is worse than accepting a wrong one
// — the operator would have no way to record a legitimate supplier at all.
func TestFR_INV_004_GSTINCheckDigitMatchesThePublishedExample(t *testing.T) {
	const example = "27AAPFU0939F1ZV"
	if got := vendors.GSTINCheckDigit(example[:14]); got != example[14] {
		t.Fatalf("check digit for %s came out %q, want %q — the algorithm is wrong, so every"+
			" GSTIN it refuses is suspect", example[:14], got, example[14])
	}
	if err := vendors.ValidateGSTIN(example); err != nil {
		t.Errorf("the registry's own example GSTIN was refused: %v", err)
	}
}

// Every single-character change to a valid GSTIN's body must break the
// checksum, and every transposition of two adjacent differing characters too.
// That is the whole point of a check digit, and it is what distinguishes this
// from the format check the schema already does.
func TestFR_INV_004_TheChecksumCatchesTyposTheFormatCannot(t *testing.T) {
	const valid = "27AAPFU0939F1ZV"

	t.Run("a transposition is still well-formed but must be refused", func(t *testing.T) {
		// 27 -> 72: two digits swapped, still two digits, so the format
		// regexp is perfectly happy with it.
		const swapped = "72AAPFU0939F1ZV"
		if err := vendors.ValidateGSTIN(swapped); !errors.Is(err, vendors.ErrGSTINChecksum) {
			t.Errorf("a transposed GSTIN gave %v, want ErrGSTINChecksum", err)
		}
	})

	t.Run("one mistyped digit", func(t *testing.T) {
		// 0939 -> 0938, in the numeric block, so the shape is untouched.
		const mistyped = "27AAPFU0938F1ZV"
		if err := vendors.ValidateGSTIN(mistyped); !errors.Is(err, vendors.ErrGSTINChecksum) {
			t.Errorf("a mistyped GSTIN gave %v, want ErrGSTINChecksum", err)
		}
	})

	t.Run("one mistyped letter", func(t *testing.T) {
		// AAPFU -> AAPFV, in the PAN block.
		const mistyped = "27AAPFV0939F1ZV"
		if err := vendors.ValidateGSTIN(mistyped); !errors.Is(err, vendors.ErrGSTINChecksum) {
			t.Errorf("a mistyped GSTIN gave %v, want ErrGSTINChecksum", err)
		}
	})

	// And the check digit itself being wrong, which is the most common paste
	// error of all: a truncated-then-retyped last character.
	for _, wrong := range []string{"A", "0", "Z", "9"} {
		if wrong == string(valid[14]) {
			continue
		}
		candidate := valid[:14] + wrong
		if err := vendors.ValidateGSTIN(candidate); !errors.Is(err, vendors.ErrGSTINChecksum) {
			t.Errorf("%s gave %v, want ErrGSTINChecksum", candidate, err)
		}
	}
}

// The check digit is one of 36 characters, so a wrong GSTIN has roughly a 1
// in 36 chance of passing by luck. This generates a body, computes the right
// digit, and confirms that exactly one of the 36 possible last characters is
// accepted — which is what makes the previous test's failures meaningful
// rather than coincidental.
func TestFR_INV_004_ExactlyOneCheckDigitIsAcceptedPerBody(t *testing.T) {
	const body = "33AAAAA0000A1Z"
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

	accepted := 0
	for _, c := range alphabet {
		if err := vendors.ValidateGSTIN(body + string(c)); err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("%d of 36 check digits were accepted for %s, want exactly 1", accepted, body)
	}
}

func TestFR_INV_004_TheFormatAndTheChecksumGiveDifferentAnswers(t *testing.T) {
	cases := []struct {
		name  string
		gstin string
		want  error
	}{
		{"empty is fine — a GSTIN is optional", "", nil},
		{"a PAN is not a GSTIN", "AAPFU0939F", vendors.ErrGSTINFormat},
		{"too short", "27AAPFU0939F1Z", vendors.ErrGSTINFormat},
		{"too long", "27AAPFU0939F1ZVX", vendors.ErrGSTINFormat},
		{"the 14th character must be Z", "27AAPFU0939F1AV", vendors.ErrGSTINFormat},
		{"letters where the state code goes", "AAAAPFU0939F1ZV", vendors.ErrGSTINFormat},
		{"a phone number", "+919876543210", vendors.ErrGSTINFormat},
		{"well-formed but mistyped", "27AAPFU0939F1ZA", vendors.ErrGSTINChecksum},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := vendors.ValidateGSTIN(tc.gstin); !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A lower-case GSTIN pasted out of an email is not a mistake; the registry
// issues them in upper case and the operator did nothing wrong.
func TestFR_INV_004_ALowerCaseGSTINIsAccepted(t *testing.T) {
	v := vendors.Vendor{Name: "Chennai Networks", GSTIN: "27aapfu0939f1zv"}
	v.Normalise()
	if v.GSTIN != "27AAPFU0939F1ZV" {
		t.Errorf("GSTIN normalised to %q", v.GSTIN)
	}
	if err := v.Validate(); err != nil {
		t.Errorf("a lower-case GSTIN was refused: %v", err)
	}
}

// Normalise has to collapse internal whitespace, not merely trim, or the name
// stored differs from the name uq_vendors_name_active keys it under — and the
// duplicate the index exists to refuse goes live.
func TestFR_INV_004_NormaliseCollapsesInternalWhitespace(t *testing.T) {
	cases := map[string]string{
		"  Chennai Networks  ":     "Chennai Networks",
		"Anna  Nagar Cables":       "Anna Nagar Cables",
		"Sri\tBalaji\n\nSupplies":  "Sri Balaji Supplies",
		"Chennai   Networks   Ltd": "Chennai Networks Ltd",
	}
	for in, want := range cases {
		v := vendors.Vendor{Name: in}
		v.Normalise()
		if v.Name != want {
			t.Errorf("%q normalised to %q, want %q", in, v.Name, want)
		}
		// And the two forms must key identically, which is the property the
		// unique index depends on.
		if vendors.NormalisedKey(in) != vendors.NormalisedKey(want) {
			t.Errorf("%q and %q key differently, so the index would not see them as one vendor", in, want)
		}
	}
}

// Lengths are measured in characters, because the columns are. A 150-character
// Tamil vendor name is 450 bytes; refusing it would be refusing data the
// database would have accepted.
func TestFR_INV_004_LengthsAreCharactersNotBytes(t *testing.T) {
	// 150 Tamil characters: well inside VARCHAR(200), well over 200 bytes.
	long := strings.Repeat("அ", 150)
	v := vendors.Vendor{Name: long}
	v.Normalise()
	if err := v.Validate(); err != nil {
		t.Errorf("a %d-character (%d-byte) name was refused: %v",
			len([]rune(long)), len(long), err)
	}

	tooLong := vendors.Vendor{Name: strings.Repeat("அ", vendors.MaxName+1)}
	tooLong.Normalise()
	if err := tooLong.Validate(); !errors.Is(err, vendors.ErrNameTooLong) {
		t.Errorf("a %d-character name gave %v, want ErrNameTooLong", vendors.MaxName+1, err)
	}
}

func TestFR_INV_004_ValidateNamesTheFieldThatIsWrong(t *testing.T) {
	cases := []struct {
		name   string
		vendor vendors.Vendor
		want   error
	}{
		{"no name", vendors.Vendor{Name: ""}, vendors.ErrNameRequired},
		{"whitespace is no name", vendors.Vendor{Name: "   "}, vendors.ErrNameRequired},
		{"name too long", vendors.Vendor{Name: strings.Repeat("x", vendors.MaxName+1)}, vendors.ErrNameTooLong},
		{"contact too long", vendors.Vendor{Name: "OK", ContactPerson: strings.Repeat("x", vendors.MaxContact+1)}, vendors.ErrContactTooLong},
		{"phone too long", vendors.Vendor{Name: "OK", Phone: strings.Repeat("9", vendors.MaxPhone+1)}, vendors.ErrPhoneTooLong},
		{"email too long", vendors.Vendor{Name: "OK", Email: strings.Repeat("x", vendors.MaxEmail+1)}, vendors.ErrEmailTooLong},
		{"a bad GSTIN", vendors.Vendor{Name: "OK", GSTIN: "nope"}, vendors.ErrGSTINFormat},
		{"everything in order", vendors.Vendor{
			Name: "Chennai Networks", ContactPerson: "R. Kumar",
			Phone: "044-2812 3456 x14", Email: "kumar@example.com",
			GSTIN: "27AAPFU0939F1ZV",
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := tc.vendor
			v.Normalise()
			if err := v.Validate(); !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// A landline with an extension is the normal shape of a supplier's number and
// has to be storable — the E.164 rule that governs subscriber numbers exists
// because those get texted, and nothing ever texts a vendor.
func TestFR_INV_004_ALandlineWithAnExtensionIsAcceptable(t *testing.T) {
	for _, phone := range []string{"044-2812 3456", "044-2812 3456 x14", "+91 44 2812 3456", "9876543210"} {
		v := vendors.Vendor{Name: "Chennai Networks", Phone: phone}
		v.Normalise()
		if err := v.Validate(); err != nil {
			t.Errorf("phone %q was refused: %v", phone, err)
		}
	}
}

func TestFR_INV_004_UsageKnowsWhetherAnythingPointsAtTheVendor(t *testing.T) {
	cases := []struct {
		name  string
		usage vendors.Usage
		want  bool
	}{
		{"nothing at all", vendors.Usage{}, false},
		{"one device type", vendors.Usage{DeviceTypes: 1}, true},
		{"one purchase", vendors.Usage{Purchases: 1}, true},
		{"one purchase order", vendors.Usage{PurchaseOrders: 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.Referenced(); got != tc.want {
				t.Errorf("Referenced() = %v, want %v", got, tc.want)
			}
		})
	}
}
