package nas

import (
	"errors"
	"net/netip"
	"testing"
)

// IP pools and static addresses — FR-NET-005.

func addr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a.Unmap()
}

func TestFR_NET_005_ParseAddrRange(t *testing.T) {
	t.Run("a pair of addresses", func(t *testing.T) {
		r, err := ParseAddrRange("203.0.113.10", "203.0.113.20")
		if err != nil {
			t.Fatal(err)
		}
		if r.First != addr(t, "203.0.113.10") || r.Last != addr(t, "203.0.113.20") {
			t.Errorf("got %s", r)
		}
		if n := r.Size(); n.Int64() != 11 {
			t.Errorf("size %s, want 11 (both ends included)", n)
		}
	})

	t.Run("a single address is a range of one", func(t *testing.T) {
		r, err := ParseAddrRange("203.0.113.5", "")
		if err != nil {
			t.Fatal(err)
		}
		if r.First != r.Last || r.Size().Int64() != 1 {
			t.Errorf("got %s size %s", r, r.Size())
		}
		if r.String() != "203.0.113.5" {
			t.Errorf("a one-address range reads %q", r.String())
		}
	})

	// A /29 means its eight addresses, network and broadcast included.
	// Which of them are unusable is what exclusions are for; guessing here
	// would silently shrink a range the operator can see.
	t.Run("a CIDR block is the whole block", func(t *testing.T) {
		r, err := ParseAddrRange("203.0.113.0/29", "")
		if err != nil {
			t.Fatal(err)
		}
		if r.First != addr(t, "203.0.113.0") || r.Last != addr(t, "203.0.113.7") {
			t.Errorf("got %s, want 203.0.113.0–203.0.113.7", r)
		}
		if n := r.Size().Int64(); n != 8 {
			t.Errorf("size %d, want 8", n)
		}
	})

	t.Run("a CIDR block that is not on its boundary is masked down", func(t *testing.T) {
		r, err := ParseAddrRange("203.0.113.5/29", "")
		if err != nil {
			t.Fatal(err)
		}
		if r.First != addr(t, "203.0.113.0") || r.Last != addr(t, "203.0.113.7") {
			t.Errorf("got %s, want the containing /29", r)
		}
	})

	t.Run("a /32 is one address", func(t *testing.T) {
		r, err := ParseAddrRange("203.0.113.9/32", "")
		if err != nil {
			t.Fatal(err)
		}
		if r.First != r.Last || r.First != addr(t, "203.0.113.9") {
			t.Errorf("got %s", r)
		}
	})

	t.Run("IPv6 works, including a prefix", func(t *testing.T) {
		r, err := ParseAddrRange("2001:db8::/126", "")
		if err != nil {
			t.Fatal(err)
		}
		if r.First != addr(t, "2001:db8::") || r.Last != addr(t, "2001:db8::3") {
			t.Errorf("got %s", r)
		}
		if r.Size().Int64() != 4 {
			t.Errorf("size %s, want 4", r.Size())
		}
	})

	t.Run("a very large IPv6 range does not overflow", func(t *testing.T) {
		r, err := ParseAddrRange("2001:db8::/64", "")
		if err != nil {
			t.Fatal(err)
		}
		// 2^64 does not fit in an int64; Size must still be right rather
		// than wrapping to a negative number.
		if r.Size().IsInt64() {
			t.Errorf("a /64 reported %s, which fits in an int64", r.Size())
		}
		if r.Size().BitLen() != 65 {
			t.Errorf("a /64 holds a %d-bit count, want 65 (2^64)", r.Size().BitLen())
		}
	})

	cases := []struct{ name, first, last string }{
		{"nothing at all", "", ""},
		{"not an address", "the office", ""},
		{"a bad second address", "203.0.113.1", "nope"},
		{"mixed families", "203.0.113.1", "2001:db8::1"},
		{"the wrong way round", "203.0.113.20", "203.0.113.10"},
		{"a malformed block", "203.0.113.0/99", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseAddrRange(tc.first, tc.last); !errors.Is(err, ErrPoolInvalid) {
				t.Fatalf("want ErrPoolInvalid, got %v", err)
			}
		})
	}
}

func TestFR_NET_005_RangeContains(t *testing.T) {
	r, err := ParseAddrRange("203.0.113.10", "203.0.113.20")
	if err != nil {
		t.Fatal(err)
	}
	// Both ends are inside: the range is inclusive.
	for _, in := range []string{"203.0.113.10", "203.0.113.15", "203.0.113.20"} {
		if !r.Contains(addr(t, in)) {
			t.Errorf("%s should be inside %s", in, r)
		}
	}
	for _, out := range []string{"203.0.113.9", "203.0.113.21", "198.51.100.15"} {
		if r.Contains(addr(t, out)) {
			t.Errorf("%s should be outside %s", out, r)
		}
	}
	// A different family is never inside, however the bytes compare.
	if r.Contains(addr(t, "2001:db8::10")) {
		t.Error("an IPv6 address was found inside an IPv4 range")
	}
	// A zero range contains nothing rather than everything.
	if (AddrRange{}).Contains(addr(t, "203.0.113.10")) {
		t.Error("an empty range contains an address")
	}
}

// staticPool is a /29 with its network, gateway and broadcast excluded —
// the ordinary shape of a customer static block.
func staticPool(t *testing.T) Pool {
	t.Helper()
	r, err := ParseAddrRange("203.0.113.0/29", "")
	if err != nil {
		t.Fatal(err)
	}
	netExcl, err := ParseAddrRange("203.0.113.0", "203.0.113.1")
	if err != nil {
		t.Fatal(err)
	}
	bcast, err := ParseAddrRange("203.0.113.7", "")
	if err != nil {
		t.Fatal(err)
	}
	return Pool{
		ID: 1, Name: "Customer statics", Purpose: PoolStatic, Range: r,
		Exclusions: []PoolExclusion{
			{Range: netExcl, Reason: "network and gateway"},
			{Range: bcast, Reason: "broadcast"},
		},
	}
}

func TestFR_NET_005_NextFreeAddressSkipsExclusionsAndHolders(t *testing.T) {
	pool := staticPool(t)

	t.Run("the first address after the exclusions", func(t *testing.T) {
		got, err := NextFreeAddress(pool, nil)
		if err != nil {
			t.Fatal(err)
		}
		// .0 and .1 are excluded, so .2 is first.
		if got != addr(t, "203.0.113.2") {
			t.Errorf("got %s, want 203.0.113.2", got)
		}
	})

	t.Run("already-held addresses are skipped", func(t *testing.T) {
		taken := map[netip.Addr]bool{
			addr(t, "203.0.113.2"): true,
			addr(t, "203.0.113.3"): true,
		}
		got, err := NextFreeAddress(pool, taken)
		if err != nil {
			t.Fatal(err)
		}
		if got != addr(t, "203.0.113.4") {
			t.Errorf("got %s, want 203.0.113.4", got)
		}
	})

	t.Run("a pool with nothing left is exhausted, not wrong", func(t *testing.T) {
		taken := map[netip.Addr]bool{}
		for _, s := range []string{"203.0.113.2", "203.0.113.3", "203.0.113.4",
			"203.0.113.5", "203.0.113.6"} {
			taken[addr(t, s)] = true
		}
		if _, err := NextFreeAddress(pool, taken); !errors.Is(err, ErrPoolExhausted) {
			t.Fatalf("want ErrPoolExhausted, got %v", err)
		}
	})

	t.Run("a non-static pool is never allocated from", func(t *testing.T) {
		dhcp := pool
		dhcp.Purpose = PoolDHCP
		if _, err := NextFreeAddress(dhcp, nil); !errors.Is(err, ErrPoolNotStatic) {
			t.Fatalf("want ErrPoolNotStatic, got %v", err)
		}
	})

	// The walk must not run past the end of the range into the next
	// block's addresses.
	t.Run("the walk stops at the end of the range", func(t *testing.T) {
		one, err := ParseAddrRange("203.0.113.5", "203.0.113.5")
		if err != nil {
			t.Fatal(err)
		}
		p := Pool{Purpose: PoolStatic, Range: one}
		got, err := NextFreeAddress(p, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != addr(t, "203.0.113.5") {
			t.Fatalf("got %s", got)
		}
		taken := map[netip.Addr]bool{addr(t, "203.0.113.5"): true}
		if _, err := NextFreeAddress(p, taken); !errors.Is(err, ErrPoolExhausted) {
			t.Errorf("a full one-address pool gave %v", err)
		}
	})
}

func TestFR_NET_005_CheckAddressAllocatable(t *testing.T) {
	pool := staticPool(t)

	if err := CheckAddressAllocatable(pool, addr(t, "203.0.113.4"), nil); err != nil {
		t.Errorf("a free in-range address was refused: %v", err)
	}

	t.Run("outside the pool", func(t *testing.T) {
		err := CheckAddressAllocatable(pool, addr(t, "198.51.100.4"), nil)
		if !errors.Is(err, ErrAddressOutsidePool) {
			t.Fatalf("want ErrAddressOutsidePool, got %v", err)
		}
	})
	t.Run("excluded", func(t *testing.T) {
		err := CheckAddressAllocatable(pool, addr(t, "203.0.113.1"), nil)
		if !errors.Is(err, ErrAddressExcluded) {
			t.Fatalf("want ErrAddressExcluded, got %v", err)
		}
	})
	t.Run("already held", func(t *testing.T) {
		taken := map[netip.Addr]bool{addr(t, "203.0.113.4"): true}
		err := CheckAddressAllocatable(pool, addr(t, "203.0.113.4"), taken)
		if !errors.Is(err, ErrAddressTaken) {
			t.Fatalf("want ErrAddressTaken, got %v", err)
		}
	})
	t.Run("the broadcast address is excluded like any other", func(t *testing.T) {
		if err := CheckAddressAllocatable(pool, addr(t, "203.0.113.7"), nil); !errors.Is(err, ErrAddressExcluded) {
			t.Errorf("got %v", err)
		}
	})
}

func TestFR_NET_005_PoolValidation(t *testing.T) {
	r, err := ParseAddrRange("203.0.113.0/29", "")
	if err != nil {
		t.Fatal(err)
	}
	valid := Pool{Name: "  Customer   statics ", Purpose: PoolStatic, Range: r}

	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid pool was refused: %v", err)
	}
	if got := valid.Normalise().Name; got != "Customer statics" {
		t.Errorf("name not collapsed: %q", got)
	}

	nasID := 0
	cases := []struct {
		name string
		mut  func(*Pool)
	}{
		{"no name", func(p *Pool) { p.Name = "   " }},
		{"an overlong name", func(p *Pool) {
			long := ""
			for len(long) <= PoolMaxName {
				long += "x"
			}
			p.Name = long
		}},
		{"no purpose", func(p *Pool) { p.Purpose = "" }},
		{"an unknown purpose", func(p *Pool) { p.Purpose = "whatever" }},
		{"no range", func(p *Pool) { p.Range = AddrRange{} }},
		{"a nonsense router id", func(p *Pool) { p.NASID = &nasID }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.mut(&p)
			if err := p.Validate(); !errors.Is(err, ErrPoolInvalid) {
				t.Fatalf("want ErrPoolInvalid, got %v", err)
			} else if PoolInvalidReason(err) == "" {
				t.Error("no operator-facing reason on the error")
			}
		})
	}
}

// An exclusion belongs to one pool, and one written for the wrong pool is
// the mistake worth catching.
func TestFR_NET_005_ExclusionMustLieInsideItsPool(t *testing.T) {
	pool := staticPool(t)

	inside, err := ParseAddrRange("203.0.113.3", "203.0.113.4")
	if err != nil {
		t.Fatal(err)
	}
	if err := (PoolExclusion{Range: inside, Reason: "reserved for the NVR"}).Validate(pool); err != nil {
		t.Errorf("an in-range exclusion was refused: %v", err)
	}

	outside, err := ParseAddrRange("198.51.100.3", "198.51.100.4")
	if err != nil {
		t.Fatal(err)
	}
	err = (PoolExclusion{Range: outside, Reason: "wrong pool"}).Validate(pool)
	if !errors.Is(err, ErrPoolInvalid) {
		t.Fatalf("want ErrPoolInvalid for an exclusion outside the pool, got %v", err)
	}

	// Half in, half out is refused too: it almost certainly means the
	// operator typed the wrong range.
	straddling, err := ParseAddrRange("203.0.113.6", "203.0.113.9")
	if err != nil {
		t.Fatal(err)
	}
	if err := (PoolExclusion{Range: straddling, Reason: "straddles the end"}).Validate(pool); err == nil {
		t.Error("an exclusion running past the end of the pool was accepted")
	}

	if err := (PoolExclusion{Range: inside, Reason: "  "}).Validate(pool); !errors.Is(err, ErrPoolInvalid) {
		t.Errorf("a reasonless exclusion was accepted: %v", err)
	}
}

func TestFR_NET_005_PoolAndAssignmentLabels(t *testing.T) {
	pool := staticPool(t)
	if !pool.Static() {
		t.Error("a static pool does not report itself static")
	}
	if pool.SizeLabel() != "8" {
		t.Errorf("a /29 reports size %q", pool.SizeLabel())
	}

	// An IPv6 /64's capacity is not a useful number to print.
	big6, err := ParseAddrRange("2001:db8::/64", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := (Pool{Range: big6}).SizeLabel(); got != "very large" {
		t.Errorf("a /64 reports size %q", got)
	}

	for purpose, want := range map[string]bool{PoolStatic: true, PoolDHCP: false, PoolCGNAT: false} {
		p := pool
		p.Purpose = purpose
		if p.Static() != want {
			t.Errorf("%s: Static()=%v", purpose, p.Static())
		}
		if p.PurposeLabel() == "" {
			t.Errorf("%s has no label", purpose)
		}
	}

	a := StaticAssignment{Status: AssignmentActive, Address: addr(t, "203.0.113.4")}
	if !a.Active() || a.StatusLabel() != "Assigned" {
		t.Errorf("active assignment reads %v / %q", a.Active(), a.StatusLabel())
	}
	if a.Billed() {
		t.Error("an assignment with no product reports itself billed")
	}
	pid := 7
	a.SubscriberProductID = &pid
	if !a.Billed() {
		t.Error("an assignment with a product does not report itself billed")
	}
	// The RADIUS attribute needs a 4-byte net.IP for an IPv4 address, not
	// a 16-byte v4-in-v6 form, or the NAS reads it as IPv6.
	if got := a.IP(); len(got) != 4 || got.String() != "203.0.113.4" {
		t.Errorf("IP() gave %v (%d bytes), want a 4-byte 203.0.113.4", got, len(got))
	}

	a.Status = AssignmentReleased
	if a.Active() || a.StatusLabel() != "Released" {
		t.Errorf("released assignment reads %v / %q", a.Active(), a.StatusLabel())
	}
}
