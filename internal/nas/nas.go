// Package nas resolves per-NAS RADIUS behavior: which vendor's bandwidth
// attribute to send, which shared secret to verify a packet against, and
// which CoA/PoD port to target.
//
// Before this package existed, internal/radius and internal/fup each
// hand-encoded one hardcoded MikroTik vendor-specific attribute (vendor
// 14988) into every Access-Accept and CoA packet, and every NAS shared one
// global RADIUS secret. Any Cisco, Juniper, Huawei, ZTE, or wireless-
// controller NAS authenticated subscribers correctly but received an
// attribute it does not understand — RADIUS silently ignores unrecognized
// vendor attributes, so the subscriber connected at whatever the NAS's own
// default was, never their plan speed, with no error anywhere.
//
// FR: FR-NAS-001..004 | MDS §4.11
package nas

import "layeh.com/radius"

// Vendor identifies which attribute dialect a NAS speaks. Distinct wireless
// controller brands are separate values, not one "wireless_generic" bucket:
// Cisco WLC inherited Airespace's vendor ID (14179) rather than Cisco's own
// (9), and Aruba/Ruckus each have their own vendor-specific dictionaries —
// they cannot share a builder.
type Vendor string

const (
	VendorMikrotik Vendor = "mikrotik"
	VendorHuawei   Vendor = "huawei"
	VendorZTE      Vendor = "zte"
	VendorCisco    Vendor = "cisco"
	VendorJuniper  Vendor = "juniper"
	VendorCiscoWLC Vendor = "cisco_wlc"
	VendorAruba    Vendor = "aruba"
	VendorRuckus   Vendor = "ruckus"
)

// RateProfile carries both representations a builder might need. Dynamic-
// rate vendors (MikroTik, Huawei, ZTE) read RateLimitString directly.
// Policy-reference vendors (Cisco, Juniper, the wireless controllers) read
// ProfileName instead — RADIUS never sends them a raw number, only the name
// of a QoS policy the NAS already has provisioned locally (MDS §4.11).
type RateProfile struct {
	RateLimitString string // e.g. "50M/50M" — plans.rate_limit_string
	ProfileName     string // pre-provisioned NAS-side policy name, from plan_nas_profiles

	// Shaping is the plan's bandwidth profile when it has one (FR-CAT-002,
	// migration 065). Nil means the plan shapes from RateLimitString, which
	// is the case for every plan that predates profiles and for the ones
	// created without going near the catalogue — hotspot vouchers and the
	// demo seed both do. Nil is therefore a supported steady state, not a
	// migration window.
	//
	// Builders must not read this field directly. Use the two accessors
	// below: the burst form is a MikroTik queue dialect and is unparseable
	// by the integer-VSA vendors, so reaching it from the wrong builder
	// would stop authentication on every Huawei and ZTE NAS in the estate.
	Shaping *Shaping
}

// mikrotikRateLimit is the string the MikroTik dialect should send: the full
// positional burst form when the plan has a profile, and the plan's own
// rate_limit_string when it does not.
func (p RateProfile) mikrotikRateLimit() (string, error) {
	if p.Shaping == nil {
		return p.RateLimitString, nil
	}
	return p.Shaping.MikrotikRateLimit()
}

// plainRateLimit is the string every other rate-carrying dialect should
// parse: the ceiling alone, never the burst form.
func (p RateProfile) plainRateLimit() string {
	if p.Shaping == nil {
		return p.RateLimitString
	}
	return p.Shaping.PlainRateLimit()
}

// Attr is a single (type, value) RADIUS attribute pair, ready for
// (*radius.Packet).Add.
type Attr struct {
	Type  radius.Type
	Value radius.Attribute
}

// AttributeBuilder constructs the vendor-specific attributes for one NAS
// dialect. PoD (Disconnect-Request) is deliberately not part of this
// interface: RFC 3576 Disconnect-Request needs only Acct-Session-Id, no
// vendor attribute, for any vendor.
type AttributeBuilder interface {
	BuildAccept(p RateProfile) ([]Attr, error)
	BuildCoA(p RateProfile) ([]Attr, error)
}

// Device is a resolved, ready-to-use NAS: secret already decrypted, vendor
// already known.
type Device struct {
	ID      int
	IP      string
	Vendor  Vendor
	Secret  []byte
	CoAPort int
	PoDPort int
	// AllowMAB permits MAC-address-only authentication from this NAS
	// (FR-HSP-002). False is the safe zero value and is what an unregistered
	// NAS resolves to, so MAB can never be reached by a device the operator
	// has not deliberately enabled it for.
	AllowMAB bool
}
