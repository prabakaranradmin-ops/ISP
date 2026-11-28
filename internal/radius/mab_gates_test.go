package radius

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

// MAC Auth Bypass, request by request through its three gates — FR-HSP-002.
//
// mab.go calls the three gates load-bearing: MAB only from a NAS that opted
// in, only for a MAC registered to that NAS, and only for a subscriber whose
// status authorises service. mab_test.go covered MAC normalisation and the
// unregistered-NAS default; nothing drove a request through handleMAB, so
// none of the three was tested where it is enforced.

const (
	mabNASIP    = "203.0.113.10" // registered, allow_mab on
	plainNASIP  = "203.0.113.20" // registered, allow_mab off
	strangerIP  = "198.51.100.7" // not registered at all
	mabNASID    = 41
	plainNASID  = 42
	registered  = "AA:BB:CC:DD:EE:01"
	unknownMAC  = "AA:BB:CC:DD:EE:99"
	mabSecretPT = "per-nas-secret"
)

type mabDevices struct{ rows []nas.DeviceRow }

func (s *mabDevices) ListNASDevices(context.Context) ([]nas.DeviceRow, error) { return s.rows, nil }
func (s *mabDevices) ListPlanNASProfiles(context.Context) ([]nas.PlanProfileRow, error) {
	return nil, nil
}
func (s *mabDevices) ListEnabledReplyAttributes(context.Context) ([]nas.ReplyAttribute, error) {
	return nil, nil
}

// fakeMAB answers AuthorizeMAC from a fixed registration, and records what
// it was asked so a test can tell "refused at the NAS gate" from "looked up
// and refused".
type fakeMAB struct {
	sub    *Subscriber
	err    error
	calls  int
	gotMAC string
	gotNAS int
}

func (f *fakeMAB) AuthorizeMAC(_ context.Context, mac string, nasID int) (*Subscriber, error) {
	f.calls++
	f.gotMAC, f.gotNAS = mac, nasID
	if f.err != nil {
		return nil, f.err
	}
	// Registered to the MAB NAS only: the binding gate.
	if mac == registered && nasID == mabNASID {
		return f.sub, nil
	}
	return nil, nil
}

func mabResolver(t *testing.T) *nas.Resolver {
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
	r := nas.NewResolver(&mabDevices{rows: []nas.DeviceRow{
		{ID: mabNASID, IP: mabNASIP, Vendor: "mikrotik", RadiusSecretEncrypted: secret, AllowMAB: true},
		{ID: plainNASID, IP: plainNASIP, Vendor: "mikrotik", RadiusSecretEncrypted: secret},
	}}, ks, []byte("global-secret"), 3799)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return r
}

type mabWriter struct{ packets []*radius.Packet }

func (w *mabWriter) Write(p *radius.Packet) error { w.packets = append(w.packets, p); return nil }

func mabRequest(t *testing.T, fromIP, username, password string) *radius.Request {
	t.Helper()
	p := radius.New(radius.CodeAccessRequest, []byte(mabSecretPT))
	if err := rfc2865.UserName_SetString(p, username); err != nil {
		t.Fatal(err)
	}
	if password != "" {
		if err := rfc2865.UserPassword_SetString(p, password); err != nil {
			t.Fatal(err)
		}
	}
	return &radius.Request{Packet: p, RemoteAddr: &net.UDPAddr{IP: net.ParseIP(fromIP), Port: 1812}}
}

func activeHotspotSub() *Subscriber {
	return &Subscriber{ID: 7, Username: "cafe-guest", Status: "active", RateLimitStr: "5M/5M", PlanID: 3}
}

func runMAB(t *testing.T, d *RadiusDaemon, req *radius.Request) (handled bool, code radius.Code) {
	t.Helper()
	w := &mabWriter{}
	handled = d.handleMAB(context.Background(), w, req)
	if handled {
		if len(w.packets) != 1 {
			t.Fatalf("handled with %d responses", len(w.packets))
		}
		code = w.packets[0].Code
	} else if len(w.packets) != 0 {
		t.Fatal("not handled, yet a response was written")
	}
	return handled, code
}

// A registered MAC on the NAS it is registered to, for an active subscriber,
// is accepted with the plan's rate limit — the one path that admits anyone.
func TestFR_HSP_002_ARegisteredMACOnItsNASIsAccepted(t *testing.T) {
	db := &fakeMAB{sub: activeHotspotSub()}
	d := &RadiusDaemon{}
	d.SetNASResolver(mabResolver(t))
	d.SetMABQuerier(db)

	// The spellings NAS vendors send all reach the same registration.
	for _, spelling := range []string{"aa:bb:cc:dd:ee:01", "AA-BB-CC-DD-EE-01", "aabb.ccdd.ee01", "aabbccddee01"} {
		handled, code := runMAB(t, d, mabRequest(t, mabNASIP, spelling, ""))
		if !handled || code != radius.CodeAccessAccept {
			t.Errorf("%q: handled=%v code=%v", spelling, handled, code)
		}
		if db.gotMAC != registered || db.gotNAS != mabNASID {
			t.Errorf("%q was looked up as %q on NAS %d", spelling, db.gotMAC, db.gotNAS)
		}
	}
	// The MAC repeated as the password is what MikroTik and Cisco send.
	if handled, code := runMAB(t, d, mabRequest(t, mabNASIP, registered, "aa-bb-cc-dd-ee-01")); !handled || code != radius.CodeAccessAccept {
		t.Errorf("MAC-as-password: handled=%v code=%v", handled, code)
	}
}

// Gate 1: a NAS that has not opted in refuses MAB before any lookup, and so
// does one that is not registered at all, and so does a daemon with no NAS
// inventory.
func TestFR_HSP_002_MABIsRefusedFromANASThatDidNotOptIn(t *testing.T) {
	for name, from := range map[string]string{"opted out": plainNASIP, "unregistered": strangerIP} {
		db := &fakeMAB{sub: activeHotspotSub()}
		d := &RadiusDaemon{}
		d.SetNASResolver(mabResolver(t))
		d.SetMABQuerier(db)
		handled, code := runMAB(t, d, mabRequest(t, from, registered, ""))
		if !handled || code != radius.CodeAccessReject {
			t.Errorf("%s NAS: handled=%v code=%v", name, handled, code)
		}
		if db.calls != 0 {
			t.Errorf("%s NAS: the MAC was looked up before the NAS gate refused it", name)
		}
	}

	db := &fakeMAB{sub: activeHotspotSub()}
	d := &RadiusDaemon{}
	d.SetMABQuerier(db)
	if handled, code := runMAB(t, d, mabRequest(t, mabNASIP, registered, "")); !handled || code != radius.CodeAccessReject || db.calls != 0 {
		t.Errorf("no NAS inventory: handled=%v code=%v lookups=%d; it must not mean MAB everywhere", handled, code, db.calls)
	}
}

// Gate 2: an unknown MAC, and a MAC registered to a different NAS, are
// refused; a lookup that fails refuses rather than admits.
func TestFR_HSP_002_OnlyAMACRegisteredToThisNASGetsIn(t *testing.T) {
	d := &RadiusDaemon{}
	d.SetNASResolver(mabResolver(t))
	d.SetMABQuerier(&fakeMAB{sub: activeHotspotSub()})
	if _, code := runMAB(t, d, mabRequest(t, mabNASIP, unknownMAC, "")); code != radius.CodeAccessReject {
		t.Errorf("unknown MAC: %v", code)
	}

	d.SetMABQuerier(&fakeMAB{err: errors.New("db: connection refused")})
	if _, code := runMAB(t, d, mabRequest(t, mabNASIP, registered, "")); code != radius.CodeAccessReject {
		t.Errorf("a failed lookup admitted the device: %v", code)
	}
}

// Gate 3: MAB admits exactly the statuses PAP and EAP do — no more, so
// suspension cannot be bypassed by switching auth method, and no fewer, so a
// hotspot user in grace is not cut off while their PPPoE login still works.
// Derived from AuthorisesService rather than listed, so the two cannot drift.
func TestFR_HSP_002_MABFollowsTheSameStatusRuleAsPAP(t *testing.T) {
	for _, status := range []string{
		"active", "grace_period", "soft_suspended",
		"hard_suspended", "terminated", "pending_payment", "some_future_status",
	} {
		sub := activeHotspotSub()
		sub.Status = status
		d := &RadiusDaemon{}
		d.SetNASResolver(mabResolver(t))
		d.SetMABQuerier(&fakeMAB{sub: sub})
		want := radius.CodeAccessReject
		if AuthorisesService(status) {
			want = radius.CodeAccessAccept
		}
		if _, code := runMAB(t, d, mabRequest(t, mabNASIP, registered, "")); code != want {
			t.Errorf("%s subscriber got %v over MAB, want %v (what PAP gives them)", status, code, want)
		}
	}
}

// Not MAB at all: a MAC-shaped username with a real password is a PPPoE
// login and must reach the password check, and an ordinary username never
// enters the MAB path. Both fall through untouched.
func TestFR_HSP_002_ARealCredentialIsNeverDowngradedToMAB(t *testing.T) {
	db := &fakeMAB{sub: activeHotspotSub()}
	d := &RadiusDaemon{}
	d.SetNASResolver(mabResolver(t))
	d.SetMABQuerier(db)
	for name, req := range map[string]*radius.Request{
		"MAC username, real password": mabRequest(t, mabNASIP, registered, "s3cret-pass"),
		"ordinary username":           mabRequest(t, mabNASIP, "ravi@isp", ""),
	} {
		if handled, _ := runMAB(t, d, req); handled {
			t.Errorf("%s was taken by the MAB path", name)
		}
	}
	if db.calls != 0 {
		t.Error("a non-MAB request reached the MAC lookup")
	}

	// With no hotspot configured, a MAB-shaped request falls through to PAP.
	bare := &RadiusDaemon{}
	bare.SetNASResolver(mabResolver(t))
	if handled, _ := runMAB(t, bare, mabRequest(t, mabNASIP, registered, "")); handled {
		t.Error("with no MAB store, a MAB-shaped request was answered instead of falling through")
	}
}
