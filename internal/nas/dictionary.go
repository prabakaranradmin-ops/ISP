package nas

import (
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"layeh.com/radius"
)

// Per-plan, per-vendor RADIUS reply attributes — FR-CAT-004.
//
// The bandwidth attribute is not the only thing an operator needs to send.
// A plan might have to put the subscriber in a MikroTik address list, hand
// them an address from a named pool, cut the session after a fixed time, or
// ask the NAS for more frequent interim accounting. All of those are reply
// attributes, and before this they could only be added by editing Go.
//
// They are validated against a dictionary rather than stored as free text
// because RADIUS is silent about attributes it does not understand: a NAS
// receiving a misspelled or wrongly typed attribute ignores it, so the
// operator sees a plan that looks configured and does nothing. That is the
// same failure this package was created to stop, and the dictionary is what
// turns it into a refusal at the point of entry.

// ValueKind is how an attribute's value is encoded on the wire. The kind
// decides both the encoding and what the console will accept, so a value
// that cannot be encoded is refused rather than silently truncated.
type ValueKind string

const (
	// KindString is free text, encoded as-is. RADIUS limits one attribute's
	// value to 253 bytes.
	KindString ValueKind = "string"
	// KindUint32 is a 32-bit unsigned integer, big-endian — the width every
	// numeric RADIUS attribute here uses.
	KindUint32 ValueKind = "uint32"
	// KindIPv4 is a bare IPv4 address, four bytes. RADIUS address
	// attributes carry no mask.
	KindIPv4 ValueKind = "ipaddr"
)

// AttributeDef is one attribute an operator may define for a plan.
//
// VendorID zero means a standard RFC attribute sent at the top level, with
// Standard giving its type; otherwise it is a vendor-specific attribute
// (RFC 2865 §5.26) carried inside type 26 with SubType identifying it.
type AttributeDef struct {
	Name     string
	Kind     ValueKind
	Help     string
	VendorID uint32
	SubType  byte
	Standard radius.Type

	// Reserved marks an attribute one of the bandwidth builders already
	// sends. An operator attribute with the same type would arrive in the
	// same packet as the one the plan's profile produced, and which of the
	// two a NAS honours is not defined — so these are refused rather than
	// allowed to quietly fight the bandwidth profile (FR-CAT-002).
	Reserved        bool
	ReservedBecause string

	// Unverified marks a sub-type this codebase has not confirmed against
	// real hardware, following the same convention as the builders in
	// huawei.go, zte.go and wireless.go. The structure is right; the number
	// may not be, and sending a wrong sub-type is silently ignored by the
	// NAS. The console shows this so an operator knows to check before
	// relying on it.
	Unverified bool
}

// standardAttributes are RFC attributes any NAS understands, so they are
// offered for every vendor rather than duplicated into each vendor's list.
var standardAttributes = []AttributeDef{
	{
		Name: "Session-Timeout", Kind: KindUint32, Standard: 27,
		Help: "Seconds after which the NAS ends the session, whatever the plan's expiry says. " +
			"Useful for trial and hotspot plans; leave unset for ordinary broadband.",
	},
	{
		Name: "Idle-Timeout", Kind: KindUint32, Standard: 28,
		Help: "Seconds of no traffic after which the NAS ends the session.",
	},
	{
		Name: "Acct-Interim-Interval", Kind: KindUint32, Standard: 85,
		Help: "Seconds between interim accounting updates. Lower means fresher usage figures " +
			"and more RADIUS traffic; below 60 is rarely worth it.",
	},
	{
		Name: "Framed-MTU", Kind: KindUint32, Standard: 12,
		Help: "MTU the NAS should use for the session.",
	},
	{
		Name: "Framed-Pool", Kind: KindString, Standard: 88,
		Help: "Name of an address pool configured on the NAS to allocate from. " +
			"This is the NAS's own pool, not an IP pool defined here (FR-NET-005).",
	},
	{
		Name: "Filter-Id", Kind: KindString, Standard: 11,
		Help: "Name of a filter or policer provisioned on the NAS.",
		// Juniper's bandwidth builder sends exactly this attribute.
		Reserved: true,
		ReservedBecause: "the Juniper bandwidth builder sends Filter-Id to name the plan's " +
			"policer, so a second one would compete with the plan's own shaping",
	},
}

// vendorAttributes are the vendor-specific attributes, by vendor.
//
// This list is deliberately short and made of attributes worth having
// rather than a transcription of each vendor's whole dictionary: every
// entry here is something an operator can be given a sentence about, and
// an attribute nobody can explain is one nobody should be sending. Adding
// more is a one-line change here plus a test.
var vendorAttributes = map[Vendor][]AttributeDef{
	VendorMikrotik: {
		{
			Name: "Mikrotik-Rate-Limit", Kind: KindString,
			VendorID: mikrotikVendorID, SubType: mikrotikRateLimitType,
			Help:     "The queue rate limit.",
			Reserved: true,
			ReservedBecause: "this is the attribute the plan's bandwidth profile renders " +
				"(FR-CAT-002); set the speed on the profile instead",
		},
		{
			Name: "Mikrotik-Address-List", Kind: KindString,
			VendorID: mikrotikVendorID, SubType: 19,
			Help: "Adds the subscriber's address to this MikroTik address list for the life of " +
				"the session, so firewall rules can be written against the plan.",
		},
		{
			Name: "Mikrotik-Group", Kind: KindString,
			VendorID: mikrotikVendorID, SubType: 3,
			Help: "Name of a PPP profile or group configured on the router to apply to the session.",
		},
		{
			Name: "Mikrotik-Realm", Kind: KindString,
			VendorID: mikrotikVendorID, SubType: 9,
			Help: "Realm the router should associate with the session.",
		},
		{
			Name: "Mikrotik-Delegated-IPv6-Pool", Kind: KindString,
			VendorID: mikrotikVendorID, SubType: 22,
			Help: "Name of a router-side pool to delegate an IPv6 prefix from (see FR-NET-003 for " +
				"how delegations are recorded).",
			Unverified: true,
		},
	},
	VendorHuawei: {
		{
			Name: "Huawei-Input-Average-Rate", Kind: KindUint32,
			VendorID: huaweiVendorID, SubType: huaweiInputAvgRateType,
			Help:     "Inbound committed rate in bits per second.",
			Reserved: true,
			ReservedBecause: "the Huawei bandwidth builder sends this from the plan's rate " +
				"or profile (FR-CAT-002)",
			Unverified: true,
		},
		{
			Name: "Huawei-Output-Average-Rate", Kind: KindUint32,
			VendorID: huaweiVendorID, SubType: huaweiOutputAvgRateType,
			Help:     "Outbound committed rate in bits per second.",
			Reserved: true,
			ReservedBecause: "the Huawei bandwidth builder sends this from the plan's rate " +
				"or profile (FR-CAT-002)",
			Unverified: true,
		},
	},
	VendorZTE: {
		{
			Name: "ZTE-Rate-Limit-In", Kind: KindUint32,
			VendorID: zteVendorID, SubType: zteInputAvgRateType,
			Help:     "Inbound rate in bits per second.",
			Reserved: true,
			ReservedBecause: "the ZTE bandwidth builder sends this from the plan's rate or " +
				"profile (FR-CAT-002)",
			Unverified: true,
		},
		{
			Name: "ZTE-Rate-Limit-Out", Kind: KindUint32,
			VendorID: zteVendorID, SubType: zteOutputAvgRateType,
			Help:     "Outbound rate in bits per second.",
			Reserved: true,
			ReservedBecause: "the ZTE bandwidth builder sends this from the plan's rate or " +
				"profile (FR-CAT-002)",
			Unverified: true,
		},
	},
	VendorCisco: {
		{
			Name: "Cisco-AVPair", Kind: KindString,
			VendorID: ciscoVendorID, SubType: ciscoAVPairType,
			Help: "A Cisco attribute-value pair, written \"key=value\". The QoS policy keys " +
				"subscriber:sub-qos-policy-in and -out are refused: those are what the plan's " +
				"own shaping sends.",
		},
	},
	VendorJuniper: {},
	VendorCiscoWLC: {
		{
			Name: "Airespace-QoS-Level", Kind: KindUint32,
			VendorID: ciscoWLCVendorID, SubType: ciscoWLCQoSType,
			Help:     "QoS level the controller should apply.",
			Reserved: true,
			ReservedBecause: "the Cisco WLC bandwidth builder sends this to name the plan's " +
				"bandwidth contract (FR-CAT-002)",
			Unverified: true,
		},
	},
	VendorAruba: {
		{
			Name: "Aruba-User-Role", Kind: KindString,
			VendorID: arubaVendorID, SubType: arubaUserRoleType,
			Help:     "Name of a role configured on the controller.",
			Reserved: true,
			ReservedBecause: "the Aruba bandwidth builder sends the user role to select the " +
				"plan's bandwidth contract (FR-CAT-002)",
		},
	},
	VendorRuckus: {
		{
			Name: "Ruckus-Role", Kind: KindString,
			VendorID: ruckusVendorID, SubType: ruckusRoleType,
			Help:     "Name of a role configured on the controller.",
			Reserved: true,
			ReservedBecause: "the Ruckus bandwidth builder sends the role to select the plan's " +
				"bandwidth contract (FR-CAT-002)",
			Unverified: true,
		},
	},
}

// reservedAVPairKeys are the Cisco-AVPair keys the bandwidth builder sends.
//
// Cisco-AVPair is one attribute carrying many different settings keyed by
// the string before "=", so reserving the whole attribute would block every
// legitimate use. These two keys are reserved instead.
var reservedAVPairKeys = []string{
	"subscriber:sub-qos-policy-in",
	"subscriber:sub-qos-policy-out",
	"ip:sub-qos-policy-in",
	"ip:sub-qos-policy-out",
}

// AttributesFor returns every attribute definable for vendor, standard
// attributes included, sorted by name. Reserved entries are included so the
// console can show them greyed out with the reason — an operator who cannot
// see why an attribute is missing will assume the dictionary is incomplete
// and ask for it to be added.
func AttributesFor(vendor Vendor) []AttributeDef {
	if !KnownVendor(vendor) {
		return nil
	}
	defs := make([]AttributeDef, 0, len(standardAttributes)+4)
	defs = append(defs, standardAttributes...)
	defs = append(defs, vendorAttributes[vendor]...)
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// LookupAttribute finds one definable attribute for a vendor.
//
// A reserved attribute is found and refused with its reason, rather than
// reported as unknown: "there is no such attribute" would send the operator
// looking for a typo that is not there.
func LookupAttribute(vendor Vendor, name string) (AttributeDef, error) {
	if !KnownVendor(vendor) {
		return AttributeDef{}, fmt.Errorf("unknown NAS vendor %q (known: %s)",
			vendor, strings.Join(Vendors(), ", "))
	}
	name = strings.TrimSpace(name)
	for _, d := range AttributesFor(vendor) {
		if !strings.EqualFold(d.Name, name) {
			continue
		}
		if d.Reserved {
			return d, fmt.Errorf("%s cannot be set per plan: %s", d.Name, d.ReservedBecause)
		}
		return d, nil
	}
	return AttributeDef{}, fmt.Errorf("%q is not an attribute this system can send to a %s NAS",
		name, vendor)
}

// ErrReplyAttributeExists is returned when a plan already carries that
// attribute for that vendor. Lives here with the other domain sentinels so
// the console can tell it apart from a generic failure without importing
// internal/db.
var ErrReplyAttributeExists = errors.New("nas: this plan already has that attribute for that vendor")

// ReplyAttribute is one operator-defined attribute on a plan.
type ReplyAttribute struct {
	ID      int
	PlanID  int
	Vendor  Vendor
	Name    string
	Value   string
	Enabled bool

	CreatedBy string
	CreatedAt time.Time
}

// Normalise trims and canonicalises, so what Validate checks is what gets
// stored and two spellings of one attribute cannot both be stored.
func (a ReplyAttribute) Normalise() ReplyAttribute {
	a.Name = strings.TrimSpace(a.Name)
	a.Value = strings.TrimSpace(a.Value)
	a.Vendor = Vendor(strings.ToLower(strings.TrimSpace(string(a.Vendor))))
	// Canonical capitalisation from the dictionary, so "mikrotik-group" and
	// "Mikrotik-Group" cannot both occupy the unique index.
	for _, d := range AttributesFor(a.Vendor) {
		if strings.EqualFold(d.Name, a.Name) {
			a.Name = d.Name
			break
		}
	}
	return a
}

// Validate checks the attribute against the dictionary and encodes its
// value, so a stored attribute is always one that can be sent.
func (a ReplyAttribute) Validate() error {
	def, err := LookupAttribute(a.Vendor, a.Name)
	if err != nil {
		return err
	}
	if a.Value == "" {
		return fmt.Errorf("%s needs a value", def.Name)
	}
	if _, err := EncodeAttribute(def, a.Value); err != nil {
		return err
	}
	return nil
}

// EncodeAttribute turns an operator-entered value into the wire attribute.
func EncodeAttribute(def AttributeDef, value string) (Attr, error) {
	switch def.Kind {
	case KindUint32:
		n, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
		if err != nil {
			return Attr{}, fmt.Errorf("%s takes a whole number from 0 to 4294967295, not %q",
				def.Name, value)
		}
		return encodeAttr(def, uint32BE(uint32(n))) //nolint:gosec // bounds-checked by ParseUint
	case KindIPv4:
		addr, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			return Attr{}, fmt.Errorf("%s takes an IP address, not %q", def.Name, value)
		}
		v4 := addr.Unmap()
		if !v4.Is4() {
			return Attr{}, fmt.Errorf("%s takes an IPv4 address; %q is IPv6, which RADIUS "+
				"carries in different attributes", def.Name, value)
		}
		b := v4.As4()
		return encodeAttr(def, b[:])
	case KindString:
		if err := checkAVPair(def, value); err != nil {
			return Attr{}, err
		}
		if max := maxValueBytes(def); len(value) > max {
			return Attr{}, fmt.Errorf("%s is %d bytes; RADIUS allows at most %d here",
				def.Name, len(value), max)
		}
		return encodeAttr(def, []byte(value))
	default:
		return Attr{}, fmt.Errorf("%s has no encoding for kind %q", def.Name, def.Kind)
	}
}

// maxValueBytes is how many bytes of value this attribute can actually
// carry.
//
// RFC 2865 §5 caps an attribute's value at 253 bytes, which is the limit for
// a standard attribute. A vendor-specific one spends six of those on the
// envelope — four for the vendor id, then a sub-attribute type and length
// (§5.26) — so only 247 are left for the value itself. Checking 253 for both
// would store a value that cannot be sent: the encode fails at the outer
// layer with "value too long", which is a packet the subscriber never
// receives rather than a message the operator can act on.
func maxValueBytes(def AttributeDef) int {
	if def.VendorID == 0 {
		return 253
	}
	return 253 - 6
}

// checkAVPair refuses the Cisco-AVPair keys the bandwidth builder owns.
func checkAVPair(def AttributeDef, value string) error {
	if def.Name != "Cisco-AVPair" {
		return nil
	}
	key := value
	if i := strings.Index(value, "="); i >= 0 {
		key = value[:i]
	}
	key = strings.ToLower(strings.TrimSpace(key))
	for _, reserved := range reservedAVPairKeys {
		if key == reserved {
			return fmt.Errorf("Cisco-AVPair %q is what the plan's own shaping sends "+
				"(FR-CAT-002); set the speed on the plan's bandwidth profile instead", key)
		}
	}
	if !strings.Contains(value, "=") {
		return fmt.Errorf("Cisco-AVPair is written \"key=value\"; %q has no \"=\"", value)
	}
	return nil
}

func encodeAttr(def AttributeDef, raw []byte) (Attr, error) {
	if def.VendorID == 0 {
		return Attr{Type: def.Standard, Value: radius.Attribute(raw)}, nil
	}
	return buildVendorSubAttr(def.VendorID, def.SubType, raw)
}

// BuildReplyAttrs encodes a plan's attributes for one vendor.
//
// Attributes for other vendors are skipped rather than reported: a plan may
// legitimately carry MikroTik and Cisco attributes at once, and the NAS
// answering this request gets only its own. An attribute that cannot be
// encoded is skipped with its error returned, so the Access-Accept still
// goes out — the same stance applyRateLimit takes on a failed bandwidth
// attribute, because a subscriber should not be denied service by a
// malformed extra attribute.
func BuildReplyAttrs(vendor Vendor, attrs []ReplyAttribute) ([]Attr, []error) {
	var (
		out  []Attr
		errs []error
	)
	for _, a := range attrs {
		if a.Vendor != vendor || !a.Enabled {
			continue
		}
		def, err := LookupAttribute(a.Vendor, a.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		attr, err := EncodeAttribute(def, a.Value)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, attr)
	}
	return out, errs
}
