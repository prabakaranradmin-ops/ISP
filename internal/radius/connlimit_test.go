package radius

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

// The connection limit at login — FR-AAA-002, FR-BIL-022. See connlimit.go.

// fakeConnections answers from a list of the subscriber's open password
// sessions, by device, the way the store does.
type fakeConnections struct {
	devices []string // one entry per open session; "" for no device recorded
	holder  string   // the device whose session holds the static address
	err     error
}

func (f *fakeConnections) OpenConnections(_ context.Context, _ int, except string) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n := 0
	for _, d := range f.devices {
		if except == "" || d != except {
			n++
		}
	}
	return n, nil
}

func (f *fakeConnections) StaticAddressHeldElsewhere(_ context.Context, _ int, _ string, except string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.holder != "" && f.holder != except, nil
}

const (
	homeRouter   = "D4:CA:6D:00:00:01"
	secondRouter = "D4:CA:6D:00:00:02"
)

// limitPAP logs in over PAP from a router with the given Calling-Station-Id.
func limitPAP(t *testing.T, d *RadiusDaemon, device string) (*radius.Packet, AuthAttempt) {
	t.Helper()
	l := NewAuthLogger(nil)
	d.SetAuthLogger(l)
	req := mabRequest(t, wgMikrotikIP, wgUser, wgPassword)
	if device != "" {
		if err := rfc2865.CallingStationID_SetString(req.Packet, device); err != nil {
			t.Fatal(err)
		}
	}
	w := &mabWriter{}
	d.handleAuth(context.Background(), w, req)
	if len(w.packets) != 1 {
		t.Fatalf("%d packets", len(w.packets))
	}
	select {
	case a := <-l.ch:
		return w.packets[0], a
	default:
		t.Fatal("the attempt was not logged")
		return nil, AuthAttempt{}
	}
}

func limitDaemon(t *testing.T, limit int, mode string, conns *fakeConnections) *RadiusDaemon {
	t.Helper()
	sub := wgSubscriber(t, "active")
	sub.ConnectionLimit = limit
	d := wgDaemon(t, sub)
	d.SetConnectionLimit(mode, conns)
	return d
}

func TestFR_AAA_002_ASecondConnectionIsRefusedAtTheLimit(t *testing.T) {
	before := testutil.ToFloat64(connectionLimitRefused)
	d := limitDaemon(t, 1, ConnectionLimitEnforce, &fakeConnections{devices: []string{homeRouter}})

	resp, attempt := limitPAP(t, d, secondRouter)
	if resp.Code != radius.CodeAccessReject {
		t.Fatalf("a second router on a one-connection account: %v", resp.Code)
	}
	if attempt.RejectReason != RejectConnectionLimit {
		t.Errorf("logged as %q, want %q", attempt.RejectReason, RejectConnectionLimit)
	}
	if msg := rfc2865.ReplyMessage_GetString(resp); msg == "" {
		t.Error("the reject carries no Reply-Message, so the router's log will not say why")
	}
	if got := testutil.ToFloat64(connectionLimitRefused) - before; got != 1 {
		t.Errorf("radius_connection_limit_refused_total rose by %v", got)
	}
	// Not a failed password: the credentials were right.
	if n := d.guard.counter.Len(); n != 0 {
		t.Errorf("the refusal counted as %d failed attempts", n)
	}
}

// Under the limit, and the router that already holds the connection
// redialling it, are both let in.
func TestFR_AAA_002_UnderTheLimitAndRedialsAreLetIn(t *testing.T) {
	for name, tc := range map[string]struct {
		limit   int
		devices []string
		from    string
	}{
		"nobody online":                 {1, nil, homeRouter},
		"the same router redialling":    {1, []string{homeRouter}, homeRouter},
		"a second line they paid for":   {2, []string{homeRouter}, secondRouter},
		"a hotspot phone is not a line": {1, nil, secondRouter}, // MAB sessions never reach the count
	} {
		d := limitDaemon(t, tc.limit, ConnectionLimitEnforce, &fakeConnections{devices: tc.devices})
		if resp, _ := limitPAP(t, d, tc.from); resp.Code != radius.CodeAccessAccept {
			t.Errorf("%s: %v", name, resp.Code)
		}
	}
	// Paid for two, holding two: the third is refused.
	d := limitDaemon(t, 2, ConnectionLimitEnforce, &fakeConnections{devices: []string{homeRouter, secondRouter}})
	if resp, _ := limitPAP(t, d, "D4:CA:6D:00:00:03"); resp.Code != radius.CodeAccessReject {
		t.Errorf("a third connection on a two-connection account: %v", resp.Code)
	}
	// A router that sends no Calling-Station-Id cannot be recognised as a
	// redial, so it counts.
	d = limitDaemon(t, 1, ConnectionLimitEnforce, &fakeConnections{devices: []string{homeRouter}})
	if resp, _ := limitPAP(t, d, ""); resp.Code != radius.CodeAccessReject {
		t.Errorf("an unidentifiable second login: %v", resp.Code)
	}
}

// Report mode refuses nobody; a failed count lets the login through and is
// counted; and a subscriber whose limit never loaded is held to one.
func TestFR_AAA_002_ReportModeAndFailuresRefuseNobody(t *testing.T) {
	over := &fakeConnections{devices: []string{homeRouter, secondRouter}}
	if resp, _ := limitPAP(t, limitDaemon(t, 1, ConnectionLimitReport, over), "D4:CA:6D:00:00:03"); resp.Code != radius.CodeAccessAccept {
		t.Errorf("report mode refused a login: %v", resp.Code)
	}

	before := testutil.ToFloat64(connectionLimitCheckFailed)
	d := limitDaemon(t, 1, ConnectionLimitEnforce, &fakeConnections{err: errors.New("db: timeout")})
	if resp, _ := limitPAP(t, d, secondRouter); resp.Code != radius.CodeAccessAccept {
		t.Errorf("a failed count refused a paying subscriber: %v", resp.Code)
	}
	if got := testutil.ToFloat64(connectionLimitCheckFailed) - before; got != 1 {
		t.Errorf("radius_connection_limit_check_failed_total rose by %v", got)
	}

	d = limitDaemon(t, 0, ConnectionLimitEnforce, &fakeConnections{devices: []string{homeRouter}})
	if resp, _ := limitPAP(t, d, secondRouter); resp.Code != radius.CodeAccessReject {
		t.Errorf("an unloaded limit (0) was not held to one: %v", resp.Code)
	}
}

// Registered hotspot devices are not limited, whatever the count.
func TestFR_AAA_002_HotspotDevicesAreNotLimited(t *testing.T) {
	d := limitDaemon(t, 1, ConnectionLimitEnforce, &fakeConnections{devices: []string{homeRouter, secondRouter}})
	if resp := wgMABLogin(t, d, wgMikrotikIP); resp.Code != radius.CodeAccessAccept {
		t.Errorf("a registered hotspot device was refused at the connection limit: %v", resp.Code)
	}
}

// A static address goes to one connection only. A second connection from
// another router gets a dynamic one rather than a duplicate; the router that
// holds it keeps it on a redial; a hotspot device never gets it.
func TestFR_NET_005_AStaticAddressIsNeverHandedToTwoConnections(t *testing.T) {
	static := net.ParseIP(wgStaticIP)
	d := limitDaemon(t, 2, ConnectionLimitEnforce, &fakeConnections{devices: []string{homeRouter}, holder: homeRouter})

	if resp, _ := limitPAP(t, d, secondRouter); resp.Code != radius.CodeAccessAccept {
		t.Fatalf("second line: %v", resp.Code)
	} else if got := rfc2865.FramedIPAddress_Get(resp); got != nil {
		t.Errorf("the second connection was given %v, which the first already holds", got)
	}
	if resp, _ := limitPAP(t, d, homeRouter); !rfc2865.FramedIPAddress_Get(resp).Equal(static) {
		t.Errorf("the router holding the static address lost it on a redial")
	}
	// No device to compare: keep the address rather than guess.
	if resp, _ := limitPAP(t, d, ""); !rfc2865.FramedIPAddress_Get(resp).Equal(static) {
		t.Errorf("a login with no Calling-Station-Id lost the static address")
	}
	// A failed check keeps it too.
	d = limitDaemon(t, 2, ConnectionLimitReport, &fakeConnections{err: errors.New("db: timeout")})
	if resp, _ := limitPAP(t, d, secondRouter); !rfc2865.FramedIPAddress_Get(resp).Equal(static) {
		t.Errorf("a failed check withheld the static address")
	}
	// And a hotspot device never gets it, even with nobody else online.
	d = limitDaemon(t, 1, ConnectionLimitReport, &fakeConnections{})
	if resp := wgMABLogin(t, d, wgMikrotikIP); rfc2865.FramedIPAddress_Get(resp) != nil {
		t.Errorf("a hotspot device was handed the subscriber's static address")
	}
}
