package radius

import (
	"context"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

// Per-plan reply attributes reaching an Access-Accept — FR-CAT-004.

// replyAttrStore is a nas.DeviceStore serving one plan's attributes.
type replyAttrStore struct {
	attrs []nas.ReplyAttribute
}

func (s *replyAttrStore) ListNASDevices(context.Context) ([]nas.DeviceRow, error) {
	return nil, nil
}

func (s *replyAttrStore) ListPlanNASProfiles(context.Context) ([]nas.PlanProfileRow, error) {
	return nil, nil
}

func (s *replyAttrStore) ListEnabledReplyAttributes(context.Context) ([]nas.ReplyAttribute, error) {
	return s.attrs, nil
}

// resolverWith builds a warmed resolver over the given attributes.
func resolverWith(t *testing.T, attrs ...nas.ReplyAttribute) *nas.Resolver {
	t.Helper()
	r := nas.NewResolver(&replyAttrStore{attrs: attrs}, nil, []byte("secret"), 3799)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return r
}

// The wiring that makes the feature real. Without this the attributes are
// storable, listable, validated against a dictionary — and never sent.
func TestFR_CAT_004_ThePlansAttributesReachTheAccessAccept(t *testing.T) {
	d := &RadiusDaemon{}
	d.SetNASResolver(resolverWith(t,
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Address-List", Value: "retail", Enabled: true},
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Session-Timeout", Value: "3600", Enabled: true},
	))

	resp := radius.New(radius.CodeAccessAccept, []byte("secret"))
	d.applyRateLimit(resp, &Subscriber{
		Username: "burst@isp", RateLimitStr: "50M/50M", PlanID: 7,
	}, &radius.Request{})

	// Session-Timeout is a standard attribute, so it can be read back
	// through the typed accessor — the strongest possible evidence that it
	// arrived correctly encoded rather than merely present.
	if got := rfc2865.SessionTimeout_Get(resp); got != 3600 {
		t.Errorf("Session-Timeout arrived as %d, want 3600", got)
	}

	// The MikroTik address list is a VSA, so it is checked by looking for
	// its value in the vendor-specific attributes.
	var sawAddressList bool
	for _, a := range resp.Attributes {
		if a.Type == 26 && strings.Contains(string(a.Attribute), "retail") {
			sawAddressList = true
		}
	}
	if !sawAddressList {
		t.Error("Mikrotik-Address-List did not reach the Access-Accept — a plan attribute " +
			"the console can set and RADIUS never sends configures nothing")
	}
}

// A NAS must only receive its own vendor's attributes. A plan may carry
// several vendors' at once because an estate is mixed.
func TestFR_CAT_004_AnotherVendorsAttributesAreNotSent(t *testing.T) {
	d := &RadiusDaemon{}
	d.SetNASResolver(resolverWith(t,
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Address-List", Value: "mikrotik-only", Enabled: true},
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorCisco,
			Name: "Cisco-AVPair", Value: "ip:addr-pool=cisco-only", Enabled: true},
	))

	// No nas_devices row, so the resolver serves the MikroTik fallback.
	resp := radius.New(radius.CodeAccessAccept, []byte("secret"))
	d.applyRateLimit(resp, &Subscriber{
		Username: "mixed@isp", RateLimitStr: "50M/50M", PlanID: 7,
	}, &radius.Request{})

	var body string
	for _, a := range resp.Attributes {
		body += string(a.Attribute)
	}
	if !strings.Contains(body, "mikrotik-only") {
		t.Error("the MikroTik attribute was not sent to a MikroTik NAS")
	}
	if strings.Contains(body, "cisco-only") {
		t.Error("a Cisco attribute was sent to a MikroTik NAS")
	}
}

// Attributes for another plan must not leak into this subscriber's packet.
func TestFR_CAT_004_AnotherPlansAttributesAreNotSent(t *testing.T) {
	d := &RadiusDaemon{}
	d.SetNASResolver(resolverWith(t,
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Address-List", Value: "plan-seven", Enabled: true},
		nas.ReplyAttribute{PlanID: 8, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Address-List", Value: "plan-eight", Enabled: true},
	))

	resp := radius.New(radius.CodeAccessAccept, []byte("secret"))
	d.applyRateLimit(resp, &Subscriber{
		Username: "seven@isp", RateLimitStr: "50M/50M", PlanID: 7,
	}, &radius.Request{})

	var body string
	for _, a := range resp.Attributes {
		body += string(a.Attribute)
	}
	if !strings.Contains(body, "plan-seven") {
		t.Error("the plan's own attribute was not sent")
	}
	if strings.Contains(body, "plan-eight") {
		t.Error("another plan's attribute leaked into the packet")
	}
}

// A stored attribute the dictionary refuses must be dropped at refresh
// rather than reaching a packet, and must not take the valid ones with it.
func TestFR_CAT_004_AnUnsendableStoredAttributeIsDroppedNotFatal(t *testing.T) {
	d := &RadiusDaemon{}
	d.SetNASResolver(resolverWith(t,
		// Stored before the dictionary knew better, or edited by hand.
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Nonsense", Value: "x", Enabled: true},
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Session-Timeout", Value: "not-a-number", Enabled: true},
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Address-List", Value: "still-sent", Enabled: true},
	))

	resp := radius.New(radius.CodeAccessAccept, []byte("secret"))
	d.applyRateLimit(resp, &Subscriber{
		Username: "mixed@isp", RateLimitStr: "50M/50M", PlanID: 7,
	}, &radius.Request{})

	var body string
	for _, a := range resp.Attributes {
		body += string(a.Attribute)
	}
	if !strings.Contains(body, "still-sent") {
		t.Error("one unsendable attribute cost the subscriber their valid ones")
	}
	// And the subscriber still got their bandwidth attribute: a malformed
	// extra attribute must never cost somebody their speed.
	if !strings.Contains(body, "50M/50M") {
		t.Error("the bandwidth attribute was lost")
	}
}

// A disabled attribute is kept but not sent, so an operator can turn one off
// to test without losing what it was set to.
func TestFR_CAT_004_ADisabledAttributeNeverReachesThePacket(t *testing.T) {
	d := &RadiusDaemon{}
	// The resolver's query filters on enabled, so a disabled row would not
	// normally be returned at all; handing one over anyway checks the
	// in-memory path does not depend on the query for that.
	d.SetNASResolver(resolverWith(t,
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Address-List", Value: "should-not-appear", Enabled: false},
	))

	resp := radius.New(radius.CodeAccessAccept, []byte("secret"))
	d.applyRateLimit(resp, &Subscriber{
		Username: "off@isp", RateLimitStr: "50M/50M", PlanID: 7,
	}, &radius.Request{})

	for _, a := range resp.Attributes {
		if strings.Contains(string(a.Attribute), "should-not-appear") {
			t.Error("a disabled attribute was sent")
		}
	}
}

// With no resolver wired at all, authentication must behave exactly as
// before this feature existed.
func TestFR_CAT_004_NoResolverIsSafe(t *testing.T) {
	d := &RadiusDaemon{}
	resp := radius.New(radius.CodeAccessAccept, []byte("secret"))
	d.applyRateLimit(resp, &Subscriber{
		Username: "plain@isp", RateLimitStr: "50M/50M", PlanID: 7,
	}, &radius.Request{})

	if len(resp.Attributes) == 0 {
		t.Error("no attributes at all were added with no resolver wired")
	}
}

// The cached slice is shared by every concurrent authentication for a plan,
// so handing out the backing array would let one request's packet
// construction be visible to another.
func TestFR_CAT_004_TheCachedAttributesAreNotSharedWithCallers(t *testing.T) {
	r := resolverWith(t,
		nas.ReplyAttribute{PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Address-List", Value: "retail", Enabled: true},
	)

	first := r.ResolveReplyAttributes(7, nas.VendorMikrotik)
	if len(first) != 1 {
		t.Fatalf("got %d attributes", len(first))
	}
	first[0] = nas.Attr{Type: 99, Value: radius.Attribute("tampered")}

	second := r.ResolveReplyAttributes(7, nas.VendorMikrotik)
	if second[0].Type == 99 {
		t.Error("a caller mutating what it got back changed what the next authentication sends")
	}
}
