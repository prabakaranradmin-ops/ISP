package nas

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The walled garden's NAS half — FR-BIL-004. See walledgarden.go.

// On MikroTik the garden is one Mikrotik-Address-List naming the list the
// router script matches. Decoded from the wire bytes rather than compared
// with a second encoding, so a wrong vendor id or sub-type fails here.
func TestFR_BIL_004_TheMikroTikWalledGardenIsTheSuspendedAddressList(t *testing.T) {
	attrs, err := WalledGardenAttrs(VendorMikrotik)
	if err != nil {
		t.Fatal(err)
	}
	if len(attrs) != 1 {
		t.Fatalf("%d attributes, want exactly one", len(attrs))
	}
	a := attrs[0]
	if a.Type != 26 {
		t.Fatalf("type %d, want 26 (Vendor-Specific)", a.Type)
	}
	v := a.Value
	if len(v) < 6 {
		t.Fatalf("VSA of %d bytes", len(v))
	}
	if id := binary.BigEndian.Uint32(v[:4]); id != 14988 {
		t.Errorf("vendor %d, want 14988 (MikroTik)", id)
	}
	if v[4] != 19 {
		t.Errorf("sub-type %d, want 19 (Mikrotik-Address-List)", v[4])
	}
	if int(v[5]) != len(v)-4 {
		t.Errorf("sub-attribute length %d for %d bytes", v[5], len(v)-4)
	}
	if got := string(v[6:]); got != WalledGardenAddressList {
		t.Errorf("list %q, want %q", got, WalledGardenAddressList)
	}
}

// Every other vendor is refused, not sent something it may ignore. Derived
// from the registry, so a vendor added later is refused until somebody
// builds and checks its garden.
func TestFR_BIL_004_NoOtherVendorClaimsAWalledGarden(t *testing.T) {
	for _, name := range Vendors() {
		v := Vendor(name)
		attrs, err := WalledGardenAttrs(v)
		if v == VendorMikrotik {
			if err != nil || !SupportsWalledGarden(v) {
				t.Errorf("mikrotik: %v", err)
			}
			continue
		}
		if !errors.Is(err, ErrNoWalledGarden) || attrs != nil || SupportsWalledGarden(v) {
			t.Errorf("%s: attrs=%v err=%v — no garden has been built or checked for it", v, attrs, err)
		}
	}
	if SupportsWalledGarden("") || SupportsWalledGarden("some-new-vendor") {
		t.Error("an unknown vendor claims a walled garden")
	}
}

// The plan attributes a walled session drops are found by decoding, and the
// encodings come from the dictionary, so the two cannot name different
// sub-types.
func TestFR_BIL_004_OnlyTheAttributesThatWouldOpenTheGardenAreSuperseded(t *testing.T) {
	encode := func(vendor Vendor, name, value string) Attr {
		t.Helper()
		def, err := LookupAttribute(vendor, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		a, err := EncodeAttribute(def, value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return a
	}
	superseded := map[string]Attr{
		"Mikrotik-Address-List":        encode(VendorMikrotik, "Mikrotik-Address-List", "business"),
		"Mikrotik-Delegated-IPv6-Pool": encode(VendorMikrotik, "Mikrotik-Delegated-IPv6-Pool", "v6-pool"),
	}
	for name, a := range superseded {
		if !SupersededByWalledGarden(a) {
			t.Errorf("%s survives into a walled session", name)
		}
	}

	rate, err := mikrotikRateLimitAttrs("50M/50M")
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]Attr{
		"Mikrotik-Group":  encode(VendorMikrotik, "Mikrotik-Group", "pppoe-default"),
		"Session-Timeout": encode(VendorMikrotik, "Session-Timeout", "3600"),
		"Framed-Pool":     encode(VendorMikrotik, "Framed-Pool", "pool1"),
		"Cisco-AVPair":    encode(VendorCisco, "Cisco-AVPair", "ip:addr-pool=x"),
		"the rate limit":  rate[0],
		"a short VSA":     {Type: 26, Value: []byte{0, 0, 0x3a, 0x8c}},
	}
	for name, a := range kept {
		if SupersededByWalledGarden(a) {
			t.Errorf("%s is dropped from a walled session; it grants no access", name)
		}
	}
	// And the garden's own attribute is not mistaken for a plan's and
	// dropped by the filter meant for the plan's.
	garden, _ := WalledGardenAttrs(VendorMikrotik)
	if !SupersededByWalledGarden(garden[0]) {
		t.Error("the filter does not recognise an address list")
	}
}

// The router rules are written against the list name. A rename on either
// side leaves the walled garden restricting nothing, with no error anywhere,
// so the shipped script is read and held to the constant.
func TestFR_BIL_004_TheRouterScriptMatchesTheListRADIUSSends(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "mikrotik", "walled-garden.rsc")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the walled garden needs its router rules: %v", err)
	}
	lists := regexp.MustCompile(`src-address-list=(\S+)`).FindAllStringSubmatch(string(body), -1)
	if len(lists) == 0 {
		t.Fatal("the script matches no address list")
	}
	for _, m := range lists {
		if m[1] != WalledGardenAddressList {
			t.Errorf("the script matches src-address-list=%s; RADIUS puts sessions on %s", m[1], WalledGardenAddressList)
		}
	}
}
