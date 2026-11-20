package nas

import (
	"strings"
	"testing"
)

// Per-plan, per-vendor RADIUS reply attributes — FR-CAT-004.

// The dictionary exists to turn a silent no-op into a refusal, so the first
// property is that it refuses what RADIUS would ignore.
func TestFR_CAT_004_AnUnknownAttributeIsRefusedRatherThanSent(t *testing.T) {
	for _, name := range []string{
		"Mikrotik-Rate-Limitt",    // a typo
		"Aruba-User-Role",         // right name, wrong vendor
		"Session-Timout",          // a typo in a standard attribute
		"",                        // nothing
		"DROP TABLE subscribers;", // not an attribute at all
	} {
		if _, err := LookupAttribute(VendorMikrotik, name); err == nil {
			t.Errorf("%q was accepted for a MikroTik NAS", name)
		}
	}
}

func TestFR_CAT_004_AnUnknownVendorIsRefusedAndTheKnownOnesAreNamed(t *testing.T) {
	_, err := LookupAttribute(Vendor("ubiquiti"), "Session-Timeout")
	if err == nil {
		t.Fatal("an unknown vendor was accepted")
	}
	// The message has to list what is available, or the operator is left
	// guessing at the spelling.
	for _, vendor := range Vendors() {
		if !strings.Contains(err.Error(), vendor) {
			t.Errorf("the error does not name the known vendor %q: %v", vendor, err)
		}
	}
}

// Standard RFC attributes must be offered for every vendor, because every
// NAS understands them. Duplicating them per vendor would be the bug this
// guards against: one vendor's list falling behind the others.
func TestFR_CAT_004_StandardAttributesAreOfferedForEveryVendor(t *testing.T) {
	for _, vendor := range Vendors() {
		for _, want := range []string{"Session-Timeout", "Idle-Timeout", "Acct-Interim-Interval", "Framed-Pool"} {
			if _, err := LookupAttribute(Vendor(vendor), want); err != nil {
				t.Errorf("%s cannot be set for a %s NAS: %v", want, vendor, err)
			}
		}
	}
}

// The safety rule that matters most: an operator must not be able to define
// an attribute the bandwidth builder already sends. Two attributes of the
// same type in one packet is undefined behaviour, so a plan could silently
// deliver a speed other than the one its profile says.
func TestFR_CAT_004_AttributesTheBandwidthBuilderSendsAreReserved(t *testing.T) {
	cases := []struct {
		vendor Vendor
		name   string
	}{
		{VendorMikrotik, "Mikrotik-Rate-Limit"},
		{VendorHuawei, "Huawei-Input-Average-Rate"},
		{VendorHuawei, "Huawei-Output-Average-Rate"},
		{VendorZTE, "ZTE-Rate-Limit-In"},
		{VendorZTE, "ZTE-Rate-Limit-Out"},
		{VendorJuniper, "Filter-Id"},
		{VendorCiscoWLC, "Airespace-QoS-Level"},
		{VendorAruba, "Aruba-User-Role"},
		{VendorRuckus, "Ruckus-Role"},
	}
	for _, tc := range cases {
		t.Run(string(tc.vendor)+"/"+tc.name, func(t *testing.T) {
			_, err := LookupAttribute(tc.vendor, tc.name)
			if err == nil {
				t.Fatalf("%s can be set per plan, and would compete with the plan's own shaping", tc.name)
			}
			// Refused with a reason, not reported as unknown: "no such
			// attribute" would send the operator hunting a typo.
			if strings.Contains(err.Error(), "not an attribute") {
				t.Errorf("%s is reported as unknown rather than reserved: %v", tc.name, err)
			}
			if !strings.Contains(err.Error(), "cannot be set per plan") {
				t.Errorf("the refusal does not explain itself: %v", err)
			}
		})
	}
}

// Every reserved entry has to carry a reason, or the console shows an
// operator a greyed-out row with no explanation.
func TestFR_CAT_004_EveryReservedAttributeExplainsItself(t *testing.T) {
	for _, vendor := range Vendors() {
		for _, d := range AttributesFor(Vendor(vendor)) {
			if d.Reserved && d.ReservedBecause == "" {
				t.Errorf("%s/%s is reserved with no reason given", vendor, d.Name)
			}
			if d.Help == "" {
				t.Errorf("%s/%s has no operator-facing help", vendor, d.Name)
			}
		}
	}
}

// Cisco-AVPair carries many settings keyed by the string before "=", so the
// whole attribute cannot be reserved. The QoS keys are, and the rest work.
func TestFR_CAT_004_OnlyTheQoSAVPairKeysAreReserved(t *testing.T) {
	def, err := LookupAttribute(VendorCisco, "Cisco-AVPair")
	if err != nil {
		t.Fatalf("Cisco-AVPair is not definable at all: %v", err)
	}

	for _, blocked := range []string{
		"subscriber:sub-qos-policy-in=PLAN_50M",
		"subscriber:sub-qos-policy-out=PLAN_50M",
		"ip:sub-qos-policy-in=PLAN_50M",
		// Case and spacing must not get round it.
		"Subscriber:Sub-QoS-Policy-In=PLAN_50M",
		"  subscriber:sub-qos-policy-in  =PLAN_50M",
	} {
		if _, err := EncodeAttribute(def, blocked); err == nil {
			t.Errorf("the reserved AVPair %q was accepted", blocked)
		}
	}

	for _, allowed := range []string{
		"ip:addr-pool=retail",
		"subscriber:accounting-list=default",
	} {
		if _, err := EncodeAttribute(def, allowed); err != nil {
			t.Errorf("the legitimate AVPair %q was refused: %v", allowed, err)
		}
	}

	// An AVPair with no "=" is not an AVPair, and a NAS would ignore it.
	if _, err := EncodeAttribute(def, "just-a-string"); err == nil {
		t.Error("an AVPair with no \"=\" was accepted")
	}
}

func TestFR_CAT_004_ValuesAreCheckedAgainstTheirKind(t *testing.T) {
	uint32Def, err := LookupAttribute(VendorMikrotik, "Session-Timeout")
	if err != nil {
		t.Fatalf("Session-Timeout: %v", err)
	}
	for _, bad := range []string{"soon", "-1", "3.5", "4294967296", ""} {
		if _, err := EncodeAttribute(uint32Def, bad); err == nil {
			t.Errorf("Session-Timeout accepted %q", bad)
		}
	}
	for _, good := range []string{"0", "3600", "4294967295"} {
		if _, err := EncodeAttribute(uint32Def, good); err != nil {
			t.Errorf("Session-Timeout refused %q: %v", good, err)
		}
	}

	// A string longer than RADIUS allows cannot be sent, and truncating it
	// would send a value the operator did not enter.
	strDef, err := LookupAttribute(VendorMikrotik, "Mikrotik-Address-List")
	if err != nil {
		t.Fatalf("Mikrotik-Address-List: %v", err)
	}
	// A vendor attribute spends 6 bytes on the vendor id and sub-attribute
	// header, so 247 is the real ceiling, not 253. Storing a 253-byte value
	// would fail at encode time with "value too long" — a packet the
	// subscriber never gets rather than a message the operator can act on.
	if _, err := EncodeAttribute(strDef, strings.Repeat("x", 248)); err == nil {
		t.Error("a 248-byte vendor value was accepted; the VSA envelope leaves room for 247")
	}
	if _, err := EncodeAttribute(strDef, strings.Repeat("x", 247)); err != nil {
		t.Errorf("a 247-byte vendor value was refused: %v", err)
	}

	// A standard attribute has the whole 253 available.
	pool, err := LookupAttribute(VendorMikrotik, "Framed-Pool")
	if err != nil {
		t.Fatalf("Framed-Pool: %v", err)
	}
	if _, err := EncodeAttribute(pool, strings.Repeat("x", 253)); err != nil {
		t.Errorf("a 253-byte standard value was refused: %v", err)
	}
	if _, err := EncodeAttribute(pool, strings.Repeat("x", 254)); err == nil {
		t.Error("a 254-byte standard value was accepted")
	}
}

// A uint32 must go on the wire as four big-endian bytes, and a standard
// attribute at the top level rather than wrapped in a VSA — getting either
// wrong is silently ignored by the NAS.
func TestFR_CAT_004_TheEncodingMatchesTheAttributeKind(t *testing.T) {
	std, _ := LookupAttribute(VendorMikrotik, "Session-Timeout")
	attr, err := EncodeAttribute(std, "3600")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if attr.Type != 27 {
		t.Errorf("Session-Timeout encoded as type %d, want 27 at the top level", attr.Type)
	}
	// 3600 = 0x00000E10.
	want := []byte{0x00, 0x00, 0x0E, 0x10}
	if string(attr.Value) != string(want) {
		t.Errorf("Session-Timeout encoded as % x, want % x", attr.Value, want)
	}

	vsa, _ := LookupAttribute(VendorMikrotik, "Mikrotik-Address-List")
	attr, err = EncodeAttribute(vsa, "retail")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if attr.Type != 26 {
		t.Errorf("a vendor attribute encoded as type %d, want 26 (Vendor-Specific)", attr.Type)
	}
	if !strings.Contains(string(attr.Value), "retail") {
		t.Errorf("the VSA does not carry its value: % x", attr.Value)
	}
}

// Normalise has to canonicalise the name, or two spellings of one attribute
// both fit in the unique index and the NAS gets it twice.
func TestFR_CAT_004_NormaliseCanonicalisesTheNameAndVendor(t *testing.T) {
	a := ReplyAttribute{
		PlanID: 1, Vendor: Vendor("  MikroTik "),
		Name: "  mikrotik-address-list  ", Value: "  retail  ",
	}.Normalise()

	if a.Vendor != VendorMikrotik {
		t.Errorf("vendor normalised to %q, want %q", a.Vendor, VendorMikrotik)
	}
	if a.Name != "Mikrotik-Address-List" {
		t.Errorf("name normalised to %q, want the dictionary's spelling", a.Name)
	}
	if a.Value != "retail" {
		t.Errorf("value normalised to %q", a.Value)
	}
	if err := a.Validate(); err != nil {
		t.Errorf("the normalised attribute is invalid: %v", err)
	}
}

func TestFR_CAT_004_ValidateRefusesAnEmptyValue(t *testing.T) {
	a := ReplyAttribute{PlanID: 1, Vendor: VendorMikrotik, Name: "Mikrotik-Address-List"}
	if err := a.Normalise().Validate(); err == nil {
		t.Error("an attribute with no value was accepted")
	}
}

// Building for one vendor must send only that vendor's attributes: a plan
// may legitimately carry attributes for several vendors at once, and each
// NAS gets its own. Sending a MikroTik VSA to a Cisco NAS is ignored, which
// is harmless — but sending it instead of the Cisco one is not.
func TestFR_CAT_004_OnlyTheAnsweringVendorsAttributesAreSent(t *testing.T) {
	attrs := []ReplyAttribute{
		{PlanID: 1, Vendor: VendorMikrotik, Name: "Mikrotik-Address-List", Value: "retail", Enabled: true},
		{PlanID: 1, Vendor: VendorMikrotik, Name: "Session-Timeout", Value: "3600", Enabled: true},
		{PlanID: 1, Vendor: VendorCisco, Name: "Cisco-AVPair", Value: "ip:addr-pool=retail", Enabled: true},
	}

	built, errs := BuildReplyAttrs(VendorMikrotik, attrs)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(built) != 2 {
		t.Fatalf("built %d attributes for MikroTik, want 2", len(built))
	}

	built, errs = BuildReplyAttrs(VendorCisco, attrs)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(built) != 1 {
		t.Fatalf("built %d attributes for Cisco, want 1", len(built))
	}
}

// A disabled attribute is kept but not sent, so an operator can turn one
// off to test without losing what it was set to.
func TestFR_CAT_004_ADisabledAttributeIsNotSent(t *testing.T) {
	attrs := []ReplyAttribute{
		{PlanID: 1, Vendor: VendorMikrotik, Name: "Mikrotik-Address-List", Value: "retail", Enabled: false},
	}
	built, errs := BuildReplyAttrs(VendorMikrotik, attrs)
	if len(built) != 0 || len(errs) != 0 {
		t.Errorf("a disabled attribute produced %d attributes and %v", len(built), errs)
	}
}

// One bad attribute must not take the others down with it, and must not
// stop the Access-Accept: a subscriber should not be denied service because
// an extra attribute is malformed. Same stance applyRateLimit takes on a
// failed bandwidth attribute.
func TestFR_CAT_004_OneBadAttributeDoesNotDiscardTheGoodOnes(t *testing.T) {
	attrs := []ReplyAttribute{
		{PlanID: 1, Vendor: VendorMikrotik, Name: "Mikrotik-Address-List", Value: "retail", Enabled: true},
		// Stored before the dictionary knew better, or edited by hand.
		{PlanID: 1, Vendor: VendorMikrotik, Name: "Session-Timeout", Value: "whenever", Enabled: true},
		{PlanID: 1, Vendor: VendorMikrotik, Name: "Mikrotik-Group", Value: "retail-ppp", Enabled: true},
	}
	built, errs := BuildReplyAttrs(VendorMikrotik, attrs)

	if len(built) != 2 {
		t.Errorf("built %d attributes, want the 2 good ones", len(built))
	}
	if len(errs) != 1 {
		t.Errorf("reported %d errors, want 1 — a silently dropped attribute is the "+
			"failure this package exists to stop", len(errs))
	}
}

// An unverified sub-type must be flagged, following the convention the
// builders already use: the structure is right, the number may not be, and
// a wrong sub-type is silently ignored by the NAS.
func TestFR_CAT_004_UnverifiedSubTypesAreFlaggedForTheOperator(t *testing.T) {
	var sawUnverified bool
	for _, vendor := range Vendors() {
		for _, d := range AttributesFor(Vendor(vendor)) {
			if d.Unverified {
				sawUnverified = true
				if d.Help == "" {
					t.Errorf("%s/%s is unverified with no help text", vendor, d.Name)
				}
			}
			// A standard RFC attribute's type is not in doubt.
			if d.VendorID == 0 && d.Unverified {
				t.Errorf("%s/%s is a standard attribute marked unverified", vendor, d.Name)
			}
		}
	}
	if !sawUnverified {
		t.Skip("no unverified sub-types remain, which is good news")
	}
}
