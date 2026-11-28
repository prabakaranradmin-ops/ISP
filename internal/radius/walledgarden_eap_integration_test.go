//go:build integration

package radius

import (
	"context"
	"net"
	"slices"
	"testing"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// EAP-MSCHAPv2 builds its Accept the way PAP and MAB do — FR-BIL-004,
// FR-AAA-006, FR-NET-005, FR-CAT-004.
//
// eapFinish carried its own copy of the rate-limit half of applyRateLimit,
// and the copy had drifted: no static address, no speed override, no
// bandwidth-profile burst and none of the plan's extra attributes. It would
// also have missed the walled garden, so a soft-suspended subscriber could
// have stepped around it by authenticating over EAP instead of PAP.

// eapLogin walks a whole conversation from one NAS and returns the final
// answer.
func eapLogin(t *testing.T, d *RadiusDaemon, username, password, from string) *radius.Packet {
	t.Helper()
	ctx := context.Background()
	addr := &net.UDPAddr{IP: net.ParseIP(from), Port: 1812}
	send := func(eap, state []byte) *radius.Packet {
		req := itEAPRequest(t, username, eap, state)
		req.RemoteAddr = addr
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, req)
		if w.last() == nil {
			t.Fatal("no answer")
		}
		return w.last()
	}

	identity := (&EAPPacket{Code: EAPCodeResponse, Identifier: 1, Type: EAPTypeIdentity, Data: []byte(username)}).Encode()
	resp1 := send(identity, nil)
	if resp1.Code != radius.CodeAccessChallenge {
		t.Fatalf("identity: %v", resp1.Code)
	}
	challenge, eapID, mschapID := extractChallenge(t, rfc2869.EAPMessage_Get(resp1))

	peer := make([]byte, 16)
	for i := range peer {
		peer[i] = byte(i + 7)
	}
	nt, err := GenerateNTResponse(challenge, peer, username, NTPasswordHash(password))
	if err != nil {
		t.Fatal(err)
	}
	response := (&EAPPacket{Code: EAPCodeResponse, Identifier: eapID, Type: EAPTypeMSCHAPv2,
		Data: buildMSCHAPv2ResponsePayload(t, mschapID, peer, nt, username)}).Encode()
	resp2 := send(response, rfc2865.State_Get(resp1))
	if resp2.Code != radius.CodeAccessChallenge {
		return resp2 // refused at the verify step
	}
	success, err := ParseEAP(rfc2869.EAPMessage_Get(resp2))
	if err != nil {
		t.Fatal(err)
	}
	ack := (&EAPPacket{Code: EAPCodeResponse, Identifier: success.Identifier, Type: EAPTypeMSCHAPv2,
		Data: []byte{MSCHAPv2OpSuccess}}).Encode()
	return send(ack, rfc2865.State_Get(resp2))
}

func eapWGDaemon(t *testing.T, status string) *RadiusDaemon {
	t.Helper()
	sub := wgSubscriber(t, status)
	sub.NTHash = NTPasswordHash(wgPassword)
	d := wgDaemon(t, sub)
	d.SetEAPSessionStore(NewEAPSessionStore())
	return d
}

func TestFR_BIL_004_EAPWallsInASoftSuspendedLoginToo(t *testing.T) {
	resp := eapLogin(t, eapWGDaemon(t, "soft_suspended"), wgUser, wgPassword, wgMikrotikIP)
	if resp.Code != radius.CodeAccessAccept {
		t.Fatalf("%v", resp.Code)
	}
	if got := mikrotikValues(resp, subAddressList); !slices.Equal(got, []string{nas.WalledGardenAddressList}) {
		t.Errorf("address lists %q over EAP, want only %q", got, nas.WalledGardenAddressList)
	}
	if got := mikrotikValues(resp, subIPv6Pool); len(got) != 0 {
		t.Errorf("IPv6 delegation %q sent to a walled EAP session", got)
	}

	// And where there is no garden, EAP refuses as PAP does.
	if resp := eapLogin(t, eapWGDaemon(t, "soft_suspended"), wgUser, wgPassword, wgCiscoIP); resp.Code != radius.CodeAccessReject {
		t.Errorf("EAP from a Cisco NAS for a soft-suspended subscriber: %v", resp.Code)
	}
}

// The drift itself: an active EAP subscriber now gets their static address
// and the plan's attributes, which the copy in eapFinish never sent.
func TestFR_AAA_006_AnEAPAcceptCarriesWhatAPAPAcceptCarries(t *testing.T) {
	eap := eapLogin(t, eapWGDaemon(t, "active"), wgUser, wgPassword, wgMikrotikIP)
	pap := wgPAP(t, wgDaemon(t, wgSubscriber(t, "active")), wgMikrotikIP)
	if eap.Code != radius.CodeAccessAccept || pap.Code != radius.CodeAccessAccept {
		t.Fatalf("eap %v, pap %v", eap.Code, pap.Code)
	}
	if got := rfc2865.FramedIPAddress_Get(eap); !got.Equal(net.ParseIP(wgStaticIP)) {
		t.Errorf("EAP Framed-IP-Address %v; the subscriber's static address did not arrive", got)
	}
	if got := rfc2865.SessionTimeout_Get(eap); got != 86400 {
		t.Errorf("EAP Session-Timeout %d; the plan's attributes did not arrive", got)
	}
	for _, sub := range []byte{subRateLimit, subAddressList, subIPv6Pool} {
		if e, p := mikrotikValues(eap, sub), mikrotikValues(pap, sub); !slices.Equal(e, p) {
			t.Errorf("MikroTik sub-type %d: EAP sent %q, PAP sent %q", sub, e, p)
		}
	}
}

// eapFinish reads the subscriber again, and a suspension between the verify
// step and the acknowledgement must hold.
func TestFR_AAA_006_ASuspensionMidConversationIsHonoured(t *testing.T) {
	sub := wgSubscriber(t, "active")
	sub.NTHash = NTPasswordHash(wgPassword)
	d := wgDaemon(t, sub)
	d.SetEAPSessionStore(NewEAPSessionStore())

	ctx := context.Background()
	addr := &net.UDPAddr{IP: net.ParseIP(wgMikrotikIP), Port: 1812}
	send := func(eap, state []byte) *radius.Packet {
		req := itEAPRequest(t, wgUser, eap, state)
		req.RemoteAddr = addr
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, req)
		return w.last()
	}
	identity := (&EAPPacket{Code: EAPCodeResponse, Identifier: 1, Type: EAPTypeIdentity, Data: []byte(wgUser)}).Encode()
	resp1 := send(identity, nil)
	challenge, eapID, mschapID := extractChallenge(t, rfc2869.EAPMessage_Get(resp1))
	peer := make([]byte, 16)
	nt, _ := GenerateNTResponse(challenge, peer, wgUser, NTPasswordHash(wgPassword))
	resp2 := send((&EAPPacket{Code: EAPCodeResponse, Identifier: eapID, Type: EAPTypeMSCHAPv2,
		Data: buildMSCHAPv2ResponsePayload(t, mschapID, peer, nt, wgUser)}).Encode(), rfc2865.State_Get(resp1))
	if resp2.Code != radius.CodeAccessChallenge {
		t.Fatalf("verify: %v", resp2.Code)
	}

	sub.Status = "hard_suspended" // between the two packets

	success, _ := ParseEAP(rfc2869.EAPMessage_Get(resp2))
	resp3 := send((&EAPPacket{Code: EAPCodeResponse, Identifier: success.Identifier, Type: EAPTypeMSCHAPv2,
		Data: []byte{MSCHAPv2OpSuccess}}).Encode(), rfc2865.State_Get(resp2))
	if resp3.Code != radius.CodeAccessReject {
		t.Errorf("a subscriber hard-suspended mid-conversation was let on: %v", resp3.Code)
	}
}
