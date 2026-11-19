package nas

import (
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"time"
)

// IP pools and static addresses — FR-NET-005 | migration 060.
//
// A pool is a range of host addresses an operator may hand out, optionally
// tied to one router. Exclusions are addresses inside a pool that must not
// be handed out — the gateway, the broadcast address, anything already
// configured on the router by hand.
//
// Only 'static' pools are allocated from. The other purposes exist so an
// operator can write down the ranges the router owns and this system must
// keep away from, which is the first question asked when an address
// collides.

// Pool purposes.
const (
	// PoolStatic is allocated from by AllocateStaticIP.
	PoolStatic = "static"
	// PoolDHCP is the router's own dynamic range: recorded, never touched.
	PoolDHCP = "dhcp"
	// PoolCGNAT is the carrier-grade NAT range (migration 011 records the
	// port blocks): recorded, never touched.
	PoolCGNAT = "cgnat"
)

// Static assignment statuses.
const (
	AssignmentActive   = "active"
	AssignmentReleased = "released"
)

// Limits, mirrored by migration 060's CHECKs.
const (
	PoolMaxName        = 60
	PoolMaxDescription = 200
	PoolMaxReason      = 200
)

var (
	// ErrPoolInvalid wraps an operator-facing sentence.
	ErrPoolInvalid = errors.New("nas: invalid IP pool")
	// ErrPoolNotFound: no such pool.
	ErrPoolNotFound = errors.New("nas: IP pool not found")
	// ErrPoolNameTaken: that router already has a pool with that name.
	ErrPoolNameTaken = errors.New("nas: a pool with that name already exists on this router")
	// ErrPoolInUse: addresses from this pool are still held, so it cannot
	// be deleted.
	ErrPoolInUse = errors.New("nas: addresses from this pool are still assigned")
	// ErrPoolExhausted: every address in the pool is excluded or held.
	ErrPoolExhausted = errors.New("nas: no free address left in this pool")
	// ErrAddressTaken: that address is already held by someone.
	ErrAddressTaken = errors.New("nas: that address is already assigned")
	// ErrAddressOutsidePool: the address is not in the pool's range.
	ErrAddressOutsidePool = errors.New("nas: that address is not in the pool")
	// ErrAddressExcluded: the address is inside an exclusion.
	ErrAddressExcluded = errors.New("nas: that address is excluded from the pool")
	// ErrAssignmentNotFound: no such assignment.
	ErrAssignmentNotFound = errors.New("nas: static address assignment not found")
	// ErrAssignmentNotActive: already released.
	ErrAssignmentNotActive = errors.New("nas: that address has already been released")
	// ErrSubscriberHasAddress: the subscriber already holds one, and
	// Framed-IP-Address carries exactly one.
	ErrSubscriberHasAddress = errors.New("nas: this subscriber already holds a static address")
	// ErrPoolNotStatic: only a static pool is allocated from.
	ErrPoolNotStatic = errors.New("nas: addresses are only handed out from a static pool")
)

// PoolInvalidReason returns the sentence inside an ErrPoolInvalid, or "".
func PoolInvalidReason(err error) string {
	if !errors.Is(err, ErrPoolInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrPoolInvalid.Error()+": ")
}

// AddrRange is an inclusive range of host addresses of one family.
type AddrRange struct {
	First netip.Addr
	Last  netip.Addr
}

// ParseAddrRange parses a first/last pair, accepting a single address
// ("203.0.113.5") as a range of one and a CIDR block ("203.0.113.0/29") as
// the whole block including its network and broadcast addresses.
//
// The block form includes both ends deliberately: an operator writing a
// /29 means those eight addresses, and which of them are unusable is what
// exclusions are for. Guessing here would silently shrink a range the
// operator can see in front of them.
func ParseAddrRange(first, last string) (AddrRange, error) {
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		return AddrRange{}, fmt.Errorf("%w: give the first address of the range", ErrPoolInvalid)
	}

	// A CIDR block in the first field, with no second field, is the block.
	if last == "" && strings.Contains(first, "/") {
		p, err := netip.ParsePrefix(first)
		if err != nil {
			return AddrRange{}, fmt.Errorf("%w: %q is not an address or a CIDR block", ErrPoolInvalid, first)
		}
		p = p.Masked()
		return AddrRange{First: p.Addr(), Last: lastInPrefix(p)}, nil
	}

	f, err := netip.ParseAddr(first)
	if err != nil {
		return AddrRange{}, fmt.Errorf("%w: %q is not an IP address", ErrPoolInvalid, first)
	}
	if last == "" {
		return AddrRange{First: f.Unmap(), Last: f.Unmap()}, nil
	}
	l, err := netip.ParseAddr(last)
	if err != nil {
		return AddrRange{}, fmt.Errorf("%w: %q is not an IP address", ErrPoolInvalid, last)
	}
	f, l = f.Unmap(), l.Unmap()
	if f.Is4() != l.Is4() {
		return AddrRange{}, fmt.Errorf("%w: %s and %s are different address families", ErrPoolInvalid, f, l)
	}
	if l.Less(f) {
		return AddrRange{}, fmt.Errorf("%w: %s comes after %s; give them the other way round", ErrPoolInvalid, f, l)
	}
	return AddrRange{First: f, Last: l}, nil
}

// lastInPrefix is the final address of a masked prefix.
func lastInPrefix(p netip.Prefix) netip.Addr {
	bytes := p.Addr().AsSlice()
	bits := p.Addr().BitLen() - p.Bits()
	for i := len(bytes) - 1; i >= 0 && bits > 0; i-- {
		n := bits
		if n > 8 {
			n = 8
		}
		bytes[i] |= byte(0xFF >> (8 - n))
		bits -= n
	}
	addr, _ := netip.AddrFromSlice(bytes)
	return addr.Unmap()
}

// Contains reports whether addr falls inside the range. An address of a
// different family never does.
func (r AddrRange) Contains(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !r.First.IsValid() || addr.Is4() != r.First.Is4() {
		return false
	}
	return !addr.Less(r.First) && !r.Last.Less(addr)
}

// Size is how many addresses the range holds, as a big.Int because an IPv6
// range does not fit in an int64.
func (r AddrRange) Size() *big.Int {
	if !r.First.IsValid() || !r.Last.IsValid() {
		return big.NewInt(0)
	}
	first := new(big.Int).SetBytes(r.First.AsSlice())
	last := new(big.Int).SetBytes(r.Last.AsSlice())
	return new(big.Int).Add(new(big.Int).Sub(last, first), big.NewInt(1))
}

// String reads "203.0.113.10–203.0.113.20", or just the address when the
// range holds one.
func (r AddrRange) String() string {
	if r.First == r.Last {
		return r.First.String()
	}
	return r.First.String() + "–" + r.Last.String()
}

// Pool is one IP pool as stored.
type Pool struct {
	ID    int
	NASID *int
	// NASIP and NASDescription identify the router for display, when the
	// query asked for them.
	NASIP          string
	NASDescription string
	Name           string
	Purpose        string
	Range          AddrRange
	Description    string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedBy      string
	UpdatedAt      *time.Time
	// Exclusions, when the query asked for them.
	Exclusions []PoolExclusion
	// Assigned is how many addresses from this pool are currently held,
	// when the query asked for it.
	Assigned int
}

// Static reports whether addresses may be handed out from this pool.
func (p Pool) Static() bool { return p.Purpose == PoolStatic }

// PurposeLabel is the operator-facing purpose.
func (p Pool) PurposeLabel() string {
	switch p.Purpose {
	case PoolStatic:
		return "Static assignment"
	case PoolDHCP:
		return "Router DHCP (not allocated here)"
	case PoolCGNAT:
		return "CGNAT (not allocated here)"
	default:
		return p.Purpose
	}
}

// Size is how many addresses the pool spans, exclusions included.
func (p Pool) Size() *big.Int { return p.Range.Size() }

// SizeLabel is the pool's size for display, or "very large" when it does
// not fit in an int64 — which an IPv6 pool generally does not, and
// printing 2^64 as a capacity figure helps nobody.
func (p Pool) SizeLabel() string {
	n := p.Size()
	if !n.IsInt64() {
		return "very large"
	}
	return fmt.Sprintf("%d", n.Int64())
}

// Validate checks a pool before it is written.
func (p Pool) Validate() error {
	name := strings.Join(strings.Fields(p.Name), " ")
	switch {
	case name == "":
		return fmt.Errorf("%w: give the pool a name", ErrPoolInvalid)
	case len([]rune(name)) > PoolMaxName:
		return fmt.Errorf("%w: keep the name to %d characters", ErrPoolInvalid, PoolMaxName)
	case len([]rune(p.Description)) > PoolMaxDescription:
		return fmt.Errorf("%w: keep the description to %d characters", ErrPoolInvalid, PoolMaxDescription)
	case p.Purpose != PoolStatic && p.Purpose != PoolDHCP && p.Purpose != PoolCGNAT:
		return fmt.Errorf("%w: say what the pool is for — static, dhcp or cgnat", ErrPoolInvalid)
	case !p.Range.First.IsValid() || !p.Range.Last.IsValid():
		return fmt.Errorf("%w: give the range of addresses", ErrPoolInvalid)
	case p.NASID != nil && *p.NASID <= 0:
		return fmt.Errorf("%w: that is not a valid router", ErrPoolInvalid)
	}
	return nil
}

// Normalise returns the pool with its text fields tidied.
func (p Pool) Normalise() Pool {
	p.Name = strings.Join(strings.Fields(p.Name), " ")
	p.Description = strings.TrimSpace(p.Description)
	return p
}

// PoolExclusion is a range inside a pool that must not be handed out.
type PoolExclusion struct {
	ID        int
	PoolID    int
	Range     AddrRange
	Reason    string
	CreatedBy string
	CreatedAt time.Time
}

// Validate checks an exclusion against the pool it belongs to.
func (e PoolExclusion) Validate(pool Pool) error {
	switch {
	case strings.TrimSpace(e.Reason) == "":
		return fmt.Errorf("%w: say why these addresses are excluded", ErrPoolInvalid)
	case len([]rune(e.Reason)) > PoolMaxReason:
		return fmt.Errorf("%w: keep the reason to %d characters", ErrPoolInvalid, PoolMaxReason)
	case !e.Range.First.IsValid() || !e.Range.Last.IsValid():
		return fmt.Errorf("%w: give the addresses to exclude", ErrPoolInvalid)
	}
	// Containment is checked here rather than by a database CHECK, which
	// cannot see the parent row: an exclusion for the wrong pool is the
	// mistake worth catching, and one that merely overhangs the end of its
	// own pool is harmless.
	if !pool.Range.Contains(e.Range.First) || !pool.Range.Contains(e.Range.Last) {
		return fmt.Errorf("%w: %s is outside the pool's range (%s)",
			ErrPoolInvalid, e.Range, pool.Range)
	}
	return nil
}

// StaticAssignment is one subscriber holding one address.
type StaticAssignment struct {
	ID           int
	PoolID       int
	PoolName     string
	SubscriberID int
	Username     string
	Address      netip.Addr
	Status       string
	// SubscriberProductID is the FR-BIL-022 attachment billing this
	// address, nil when the operator chose not to bill it.
	SubscriberProductID *int
	AssignedBy          string
	AssignedAt          time.Time
	ReleasedAt          *time.Time
	ReleasedBy          string
	ReleaseReason       string
}

// Active reports whether the subscriber still holds this address.
func (a StaticAssignment) Active() bool { return a.Status == AssignmentActive }

// StatusLabel is the operator-facing status.
func (a StaticAssignment) StatusLabel() string {
	if a.Active() {
		return "Assigned"
	}
	return "Released"
}

// Billed reports whether this address carries a catalogue charge.
func (a StaticAssignment) Billed() bool { return a.SubscriberProductID != nil }

// IP returns the address as a net.IP, for the RADIUS attribute.
func (a StaticAssignment) IP() net.IP { return net.IP(a.Address.AsSlice()) }

// AllocateRequest is staff giving a subscriber a static address.
type AllocateRequest struct {
	PoolID       int
	SubscriberID int
	// Address is a specific address to assign. Zero means "the first free
	// one in the pool", which is what an operator usually wants.
	Address netip.Addr
	// ProductID attaches a catalogue product to bill the address
	// (FR-BIL-022). Zero means the address is not billed.
	ProductID int
}

// Validate checks an allocate request.
func (r AllocateRequest) Validate() error {
	switch {
	case r.PoolID <= 0:
		return fmt.Errorf("%w: choose a pool", ErrPoolInvalid)
	case r.SubscriberID <= 0:
		return fmt.Errorf("%w: no subscriber given", ErrPoolInvalid)
	}
	return nil
}

// NextFreeAddress returns the lowest address in the pool that is neither
// excluded nor already held.
//
// A linear walk from the bottom of the range, which is what an operator
// expects ("it gave out .11 because .10 is the gateway") and what makes a
// pool's used addresses contiguous and readable. Bounded by maxScan so a
// pool written as a /8 — or an IPv6 range — cannot spin: the answer for
// such a pool is still found in the first few addresses unless the first
// maxScan are genuinely all taken, in which case the operator has a
// pool-sizing problem rather than an allocation one.
func NextFreeAddress(pool Pool, taken map[netip.Addr]bool) (netip.Addr, error) {
	if !pool.Static() {
		return netip.Addr{}, ErrPoolNotStatic
	}
	const maxScan = 65536

	addr := pool.Range.First
	for i := 0; i < maxScan; i++ {
		if !pool.Range.Contains(addr) {
			break
		}
		if !taken[addr] && !excluded(pool, addr) {
			return addr, nil
		}
		next := addr.Next()
		if !next.IsValid() {
			break
		}
		addr = next
	}
	return netip.Addr{}, ErrPoolExhausted
}

// excluded reports whether addr falls inside any of the pool's exclusions.
func excluded(pool Pool, addr netip.Addr) bool {
	for _, e := range pool.Exclusions {
		if e.Range.Contains(addr) {
			return true
		}
	}
	return false
}

// CheckAddressAllocatable reports whether a specific address may be handed
// out from this pool, given what is already taken.
func CheckAddressAllocatable(pool Pool, addr netip.Addr, taken map[netip.Addr]bool) error {
	if !pool.Static() {
		return ErrPoolNotStatic
	}
	addr = addr.Unmap()
	switch {
	case !addr.IsValid():
		return fmt.Errorf("%w: that is not an IP address", ErrPoolInvalid)
	case !pool.Range.Contains(addr):
		return fmt.Errorf("%w: %s is not inside %s", ErrAddressOutsidePool, addr, pool.Range)
	case excluded(pool, addr):
		return fmt.Errorf("%w: %s", ErrAddressExcluded, addr)
	case taken[addr]:
		return fmt.Errorf("%w: %s", ErrAddressTaken, addr)
	}
	return nil
}

// PoolFilter narrows a pool listing.
type PoolFilter struct {
	NASID   int    // 0 for any
	Purpose string // "" for any
	Limit   int
}
