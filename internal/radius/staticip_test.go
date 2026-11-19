package radius

import (
	"net"
	"testing"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

// Framed-IP-Address for a subscriber holding a static address —
// FR-NET-005.
//
// Tested on the packet rather than on a return value: the requirement is
// that the NAS receives the attribute, and "the function computed the
// right string" has been the shape of a passing-but-wrong test in this
// package before (see radius_test.go's own opening comment).

func acceptFor(sub *Subscriber) *radius.Packet {
	p := radius.New(radius.CodeAccessAccept, []byte("secret"))
	applyStaticIP(p, sub)
	return p
}

func TestFR_NET_005_StaticAddressIsReturnedAsFramedIPAddress(t *testing.T) {
	p := acceptFor(&Subscriber{Username: "static@isp", StaticIP: "203.0.113.4"})

	got := rfc2865.FramedIPAddress_Get(p)
	if got == nil {
		t.Fatal("no Framed-IP-Address in the Access-Accept")
	}
	if !got.Equal(net.ParseIP("203.0.113.4")) {
		t.Errorf("Framed-IP-Address is %v, want 203.0.113.4", got)
	}
	// Four bytes on the wire, not a v4-in-v6 form: a 16-byte value would
	// be read by the NAS as something else entirely.
	if attr, err := rfc2865.FramedIPAddress_Lookup(p); err != nil {
		t.Fatalf("lookup: %v", err)
	} else if len(attr) != 4 {
		t.Errorf("Framed-IP-Address is %d bytes on the wire, want 4", len(attr))
	}
}

// The ordinary case: almost no subscriber holds a static address, and
// those that do not must get no attribute at all rather than 0.0.0.0 —
// which some NASes treat as "assign from pool" and others as a routing
// error.
func TestFR_NET_005_NoStaticAddressMeansNoAttribute(t *testing.T) {
	p := acceptFor(&Subscriber{Username: "dynamic@isp"})
	if got := rfc2865.FramedIPAddress_Get(p); got != nil {
		t.Errorf("a subscriber with no static address got Framed-IP-Address %v", got)
	}
}

// A bad row must not put a paying customer offline. They fall back to a
// dynamic address, which is what they had before this feature existed.
func TestFR_NET_005_AMalformedStoredAddressIsSkippedNotFatal(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "203.0.113.4/32", "203.0.113.999", " "} {
		p := acceptFor(&Subscriber{Username: "broken@isp", StaticIP: bad})
		if got := rfc2865.FramedIPAddress_Get(p); got != nil {
			t.Errorf("%q produced Framed-IP-Address %v", bad, got)
		}
	}
}

// Framed-IP-Address is IPv4-only (RFC 2865 §5.8). An IPv6 address is
// skipped rather than truncated to four bytes, which is what To4() would
// hand the NAS if nobody checked.
func TestFR_NET_005_IPv6IsSkippedRatherThanTruncated(t *testing.T) {
	p := acceptFor(&Subscriber{Username: "v6@isp", StaticIP: "2001:db8::4"})
	if got := rfc2865.FramedIPAddress_Get(p); got != nil {
		t.Errorf("an IPv6 static address produced Framed-IP-Address %v", got)
	}
}

// Every authentication method shapes its Accept through applyRateLimit, so
// the address cannot depend on how the subscriber authenticated. Asserted
// by driving applyRateLimit itself — the shared path — rather than
// applyStaticIP.
func TestFR_NET_005_StaticAddressComesThroughTheSharedAcceptPath(t *testing.T) {
	d := &RadiusDaemon{}
	resp := radius.New(radius.CodeAccessAccept, []byte("secret"))
	req := &radius.Request{}

	d.applyRateLimit(resp, &Subscriber{
		Username: "static@isp", StaticIP: "203.0.113.4", RateLimitStr: "100M/100M",
	}, req)

	got := rfc2865.FramedIPAddress_Get(resp)
	if got == nil || !got.Equal(net.ParseIP("203.0.113.4")) {
		t.Errorf("the shared Accept path gave Framed-IP-Address %v", got)
	}
	// And the rate limit is still attached: adding the address must not
	// have displaced what was already there.
	if len(resp.Attributes) < 2 {
		t.Errorf("the Accept carries %d attributes; the bandwidth attribute looks lost", len(resp.Attributes))
	}
}
