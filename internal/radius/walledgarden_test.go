package radius

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"golang.org/x/crypto/bcrypt"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

// Soft suspension walls the session in at login, whichever way the
// subscriber authenticates — FR-BIL-004.
//
// Until 2026-11-28 a soft-suspended login was accepted exactly like an active
// one: full speed, nothing marked. The dunning screen said "Browsing
// redirected to the payment page" and nothing redirected anything. These
// drive real PAP and MAB requests through the daemon from a MikroTik NAS,
// which can wall a session in, and from a Cisco one, which this system
// cannot, and check what each Accept carries.

const (
	wgMikrotikIP = "203.0.113.30"
	wgCiscoIP    = "203.0.113.31"
	wgPlan       = 9
	wgUser       = "wg@isp"
	wgPassword   = "wg-password"
	wgMAC        = "AA:BB:CC:DD:EE:31"
	wgStaticIP   = "100.64.0.9"
)

var wgVerifierSecret = []byte("walled-garden-verifier-secret-of-length")

type wgStore struct {
	devices []nas.DeviceRow
	attrs   []nas.ReplyAttribute
}

func (s *wgStore) ListNASDevices(context.Context) ([]nas.DeviceRow, error) { return s.devices, nil }
func (s *wgStore) ListPlanNASProfiles(context.Context) ([]nas.PlanProfileRow, error) {
	return nil, nil
}
func (s *wgStore) ListEnabledReplyAttributes(context.Context) ([]nas.ReplyAttribute, error) {
	return s.attrs, nil
}

// wgResolver knows a MikroTik and a Cisco NAS, both open to MAB, and gives
// the plan the attributes a walled session must drop alongside one it keeps.
func wgResolver(t *testing.T) *nas.Resolver {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ks, err := crypto.NewInMemoryKeyStore(map[string][]byte{"v1": key}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := crypto.NewAESEncryptor(ks)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := enc.Encrypt(mabSecretPT)
	if err != nil {
		t.Fatal(err)
	}
	r := nas.NewResolver(&wgStore{
		devices: []nas.DeviceRow{
			{ID: 51, IP: wgMikrotikIP, Vendor: "mikrotik", RadiusSecretEncrypted: secret, AllowMAB: true},
			{ID: 52, IP: wgCiscoIP, Vendor: "cisco", RadiusSecretEncrypted: secret, AllowMAB: true},
		},
		attrs: []nas.ReplyAttribute{
			{PlanID: wgPlan, Vendor: nas.VendorMikrotik, Name: "Mikrotik-Address-List", Value: "business", Enabled: true},
			{PlanID: wgPlan, Vendor: nas.VendorMikrotik, Name: "Mikrotik-Delegated-IPv6-Pool", Value: "v6-business", Enabled: true},
			{PlanID: wgPlan, Vendor: nas.VendorMikrotik, Name: "Session-Timeout", Value: "86400", Enabled: true},
		},
	}, ks, []byte("global-secret"), 3799)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return r
}

type wgDB struct{ sub *Subscriber }

func (d *wgDB) GetSubscriberByUsername(_ context.Context, username string) (*Subscriber, error) {
	if d.sub != nil && username == d.sub.Username {
		return d.sub, nil
	}
	return nil, nil
}

// wgMAB answers every MAC with the one subscriber; the gates themselves are
// mab_gates_test.go's subject.
type wgMAB struct{ sub *Subscriber }

func (m *wgMAB) AuthorizeMAC(context.Context, string, int) (*Subscriber, error) { return m.sub, nil }

func wgSubscriber(t *testing.T, status string) *Subscriber {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(wgPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return &Subscriber{ID: 21, Username: wgUser, PasswordHash: string(h), Status: status,
		RateLimitStr: "50M/50M", PlanID: wgPlan, StaticIP: wgStaticIP}
}

func wgDaemon(t *testing.T, sub *Subscriber) *RadiusDaemon {
	t.Helper()
	d := NewRadiusDaemon(":0", []byte(mabSecretPT), &wgDB{sub: sub}, wgVerifierSecret)
	d.SetNASResolver(wgResolver(t))
	d.SetMABQuerier(&wgMAB{sub: sub})
	return d
}

func wgPAP(t *testing.T, d *RadiusDaemon, from string) *radius.Packet {
	t.Helper()
	req := mabRequest(t, from, wgUser, wgPassword)
	w := &mabWriter{}
	d.handleAuth(context.Background(), w, req)
	if len(w.packets) != 1 {
		t.Fatalf("PAP wrote %d packets", len(w.packets))
	}
	return w.packets[0]
}

func wgMABLogin(t *testing.T, d *RadiusDaemon, from string) *radius.Packet {
	t.Helper()
	w := &mabWriter{}
	if !d.handleMAB(context.Background(), w, mabRequest(t, from, wgMAC, "")) {
		t.Fatal("a MAB-shaped request was not taken by the MAB path")
	}
	return w.packets[0]
}

// mikrotikValues decodes every MikroTik VSA of one sub-type in a packet.
func mikrotikValues(p *radius.Packet, subType byte) []string {
	var out []string
	for _, a := range p.Attributes {
		v := a.Attribute
		if a.Type != 26 || len(v) < 6 || binary.BigEndian.Uint32(v[:4]) != 14988 || v[4] != subType {
			continue
		}
		out = append(out, string(v[6:]))
	}
	return out
}

const (
	subRateLimit   = 8
	subAddressList = 19
	subIPv6Pool    = 22
)

func TestFR_BIL_004_ASoftSuspendedLoginIsWalledIn(t *testing.T) {
	resp := wgPAP(t, wgDaemon(t, wgSubscriber(t, "soft_suspended")), wgMikrotikIP)
	if resp.Code != radius.CodeAccessAccept {
		t.Fatalf("%v: a soft-suspended subscriber is meant to reach the payment page, so the login is accepted", resp.Code)
	}
	if got := mikrotikValues(resp, subAddressList); !slices.Equal(got, []string{nas.WalledGardenAddressList}) {
		t.Errorf("address lists %q, want only %q: the plan's own list would give the plan's firewall treatment", got, nas.WalledGardenAddressList)
	}
	if got := mikrotikValues(resp, subIPv6Pool); len(got) != 0 {
		t.Errorf("IPv6 delegation %q sent to a walled session; the garden's IPv4 rules do not see it", got)
	}
	// What grants no access is unchanged: the rate, the plan's timeout, the
	// subscriber's own address.
	if got := mikrotikValues(resp, subRateLimit); !slices.Equal(got, []string{"50M/50M"}) {
		t.Errorf("rate limit %q", got)
	}
	if got := rfc2865.SessionTimeout_Get(resp); got != 86400 {
		t.Errorf("Session-Timeout %d", got)
	}
	if got := rfc2865.FramedIPAddress_Get(resp); !got.Equal(net.ParseIP(wgStaticIP)) {
		t.Errorf("Framed-IP-Address %v", got)
	}
}

// The control: an active subscriber on the same plan gets the plan's own
// list and delegation, and nothing of the garden.
func TestFR_BIL_004_AnActiveLoginKeepsThePlansOwnTerms(t *testing.T) {
	resp := wgPAP(t, wgDaemon(t, wgSubscriber(t, "active")), wgMikrotikIP)
	if resp.Code != radius.CodeAccessAccept {
		t.Fatalf("%v", resp.Code)
	}
	if got := mikrotikValues(resp, subAddressList); !slices.Equal(got, []string{"business"}) {
		t.Errorf("address lists %q, want the plan's own", got)
	}
	if got := mikrotikValues(resp, subIPv6Pool); !slices.Equal(got, []string{"v6-business"}) {
		t.Errorf("IPv6 delegation %q", got)
	}
}

// Every status, over PAP and MAB: refused where AuthorisesService refuses,
// walled in exactly where InWalledGarden says, and the two methods agree.
// Derived from the two functions rather than listed, so they cannot drift
// from what this checks.
func TestFR_BIL_004_EveryMethodWallsInTheSameStatuses(t *testing.T) {
	for _, status := range []string{
		"active", "grace_period", "soft_suspended",
		"hard_suspended", "terminated", "pending_payment", "some_future_status",
	} {
		for method, login := range map[string]func(*testing.T, *RadiusDaemon, string) *radius.Packet{
			"PAP": wgPAP, "MAB": wgMABLogin,
		} {
			resp := login(t, wgDaemon(t, wgSubscriber(t, status)), wgMikrotikIP)
			if !AuthorisesService(status) {
				if resp.Code != radius.CodeAccessReject {
					t.Errorf("%s %s: %v, want a reject", method, status, resp.Code)
				}
				continue
			}
			if resp.Code != radius.CodeAccessAccept {
				t.Errorf("%s %s: %v, want an accept", method, status, resp.Code)
				continue
			}
			walled := slices.Contains(mikrotikValues(resp, subAddressList), nas.WalledGardenAddressList)
			if walled != InWalledGarden(status) {
				t.Errorf("%s %s: walled in = %v, want %v", method, status, walled, InWalledGarden(status))
			}
		}
	}
}

// Where this system has no walled garden, a soft-suspended login is refused
// rather than handed full service, and counted. Not as a bad password: the
// subscriber's credentials were right, and locking them out would stop the
// login they will need once they pay.
func TestFR_BIL_004_NoWalledGardenMeansNoSoftSuspendedLogin(t *testing.T) {
	before := testutil.ToFloat64(walledGardenRefused.WithLabelValues("cisco"))

	d := wgDaemon(t, wgSubscriber(t, "soft_suspended"))
	if resp := wgPAP(t, d, wgCiscoIP); resp.Code != radius.CodeAccessReject {
		t.Errorf("PAP from a Cisco NAS: %v, want a reject", resp.Code)
	}
	if resp := wgMABLogin(t, d, wgCiscoIP); resp.Code != radius.CodeAccessReject {
		t.Errorf("MAB from a Cisco NAS: %v, want a reject", resp.Code)
	}
	if got := testutil.ToFloat64(walledGardenRefused.WithLabelValues("cisco")) - before; got != 2 {
		t.Errorf("radius_walled_garden_refused_total{vendor=cisco} rose by %v, want 2", got)
	}
	if n := d.guard.counter.Len(); n != 0 {
		t.Errorf("the refusal was counted as %d failed password attempts", n)
	}

	// The control: an active subscriber on the same NAS is let on.
	if resp := wgPAP(t, wgDaemon(t, wgSubscriber(t, "active")), wgCiscoIP); resp.Code != radius.CodeAccessAccept {
		t.Errorf("an active subscriber on the Cisco NAS: %v", resp.Code)
	}
}

// LiveSessionMustEnd: which status changes reach a live session.
func TestFR_BIL_004_WhichStatusChangesEndALiveSession(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
		why      string
	}{
		{"active", "soft_suspended", true, "walled in at login only, so the session up has full service"},
		{"grace_period", "soft_suspended", true, "the dunning ladder's own step into soft suspension"},
		{"soft_suspended", "active", true, "paid, and still walled in until the session ends"},
		{"soft_suspended", "grace_period", true, "a grace extension lifts the garden"},
		{"active", "hard_suspended", true, "the cut-off"},
		{"soft_suspended", "hard_suspended", true, "the cut-off"},
		{"active", "terminated", true, "the cut-off"},
		{"active", "some_future_status", true, "a status nobody has decided about grants nothing"},
		{"", "active", true, "unknown before counts as a change"},
		{"", "soft_suspended", true, "unknown before counts as a change"},
		{"active", "grace_period", false, "same terms"},
		{"grace_period", "active", false, "same terms"},
		{"active", "active", false, "no change"},
		{"soft_suspended", "soft_suspended", false, "no change"},
		{"hard_suspended", "active", false, "refused until now, so no session to end"},
		{"pending_payment", "active", false, "refused until now, so no session to end"},
	} {
		if got := LiveSessionMustEnd(tc.from, tc.to); got != tc.want {
			t.Errorf("%q → %q: %v, want %v (%s)", tc.from, tc.to, got, tc.want, tc.why)
		}
	}
}
