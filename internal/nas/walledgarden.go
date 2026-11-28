package nas

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// The walled garden: soft suspension made real — FR-BIL-004.
//
// A soft-suspended subscriber is meant to stay connected but reach nothing
// except the payment page. The dunning screen has always said so, and the
// suspension notice tells the customer their service is restricted, but
// until 2026-11-28 RADIUS accepted a soft-suspended login exactly as it
// accepted an active one: full speed, nothing marked, nothing redirected.
// Soft suspension restricted nothing.
//
// RADIUS cannot redirect traffic. What it can do is mark the session so the
// router's own firewall does. On MikroTik that mark is Mikrotik-Address-List:
// the session's address joins the named list for as long as the session
// lasts, and the rules in deploy/mikrotik/walled-garden.rsc send that list's
// web traffic to the payment page and refuse everything else.
//
// Only MikroTik has a mark here. The other vendors have their own mechanisms
// (Cisco ISG redirect services, Juniper and Huawei portal policies,
// controller roles), and none has been built or checked against hardware.
// WalledGardenAttrs refuses them rather than sending something the NAS may
// ignore. The authentication path then refuses the login, because a soft
// suspension that silently restricts nothing is the defect this file exists
// to fix.

// WalledGardenAddressList is the MikroTik address list soft-suspended
// sessions join. deploy/mikrotik/walled-garden.rsc writes its firewall rules
// against this exact name; changing one without the other puts the walled
// garden back to restricting nothing, with no error anywhere.
const WalledGardenAddressList = "isp-suspended"

// MikroTik sub-types the walled garden sends or overrides. The dictionary
// offers both to operators as plan attributes (dictionary.go).
const (
	mikrotikAddressListType       = 19
	mikrotikDelegatedIPv6PoolType = 22
)

// ErrNoWalledGarden is returned for a NAS vendor this system cannot put
// behind a walled garden.
var ErrNoWalledGarden = errors.New("nas: no walled garden for this NAS vendor")

// SupportsWalledGarden reports whether a soft-suspended subscriber on this
// vendor's NAS can be walled in rather than refused.
func SupportsWalledGarden(vendor Vendor) bool { return vendor == VendorMikrotik }

// WalledGardenAttrs returns the attributes that put a session behind the
// walled garden on this vendor's NAS, or ErrNoWalledGarden.
func WalledGardenAttrs(vendor Vendor) ([]Attr, error) {
	if !SupportsWalledGarden(vendor) {
		return nil, fmt.Errorf("%w: %q", ErrNoWalledGarden, vendor)
	}
	a, err := buildVendorSubAttr(mikrotikVendorID, mikrotikAddressListType, []byte(WalledGardenAddressList))
	if err != nil {
		return nil, err
	}
	return []Attr{a}, nil
}

// SupersededByWalledGarden reports whether one of a plan's own reply
// attributes (FR-CAT-004) must be left out of a walled-garden session.
//
//   - Mikrotik-Address-List: the plan's list is how the operator's firewall
//     gives the plan its treatment, and a suspended subscriber should get the
//     suspension's treatment instead. Sending both would also leave which list
//     wins up to the router.
//   - Mikrotik-Delegated-IPv6-Pool: the walled garden's rules match IPv4
//     addresses. An IPv6 prefix delegated alongside them would be a route
//     around them.
//
// Everything else the plan sends (timeouts, accounting interval, pool, MTU)
// does not grant access, so it is left alone.
func SupersededByWalledGarden(a Attr) bool {
	// Type 26 is Vendor-Specific: vendor id (4 bytes), then the sub-type.
	if a.Type != 26 || len(a.Value) < 6 {
		return false
	}
	if binary.BigEndian.Uint32(a.Value[:4]) != mikrotikVendorID {
		return false
	}
	switch a.Value[4] {
	case mikrotikAddressListType, mikrotikDelegatedIPv6PoolType:
		return true
	}
	return false
}
