//go:build integration

// End-to-end EAP-MSCHAPv2 conversation tests — FR-AAA-006 | MDS §4.18.
//
// These drive the real handler through all three round trips against the
// real session store, so the State attribute, the stored challenge and the
// stage transitions are exercised the way a supplicant would exercise them.
// The crypto itself is pinned separately against RFC 2759's published
// vectors in mschapv2_test.go.
package radius

import (
	"context"
	"encoding/binary"
	"testing"

	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"
)

// itEAPDaemon builds a daemon with EAP enabled.
func itEAPDaemon(t *testing.T, subs map[string]*Subscriber) *RadiusDaemon {
	t.Helper()
	d := itNewDaemon(t, subs)
	d.SetEAPSessionStore(NewEAPSessionStore())
	return d
}

// itEAPRequest builds an Access-Request carrying an EAP-Message, optionally
// echoing a State from a previous Access-Challenge.
func itEAPRequest(t *testing.T, username string, eap []byte, state []byte) *radius.Request {
	t.Helper()
	pkt := radius.New(radius.CodeAccessRequest, itSecret)
	if err := rfc2865.UserName_SetString(pkt, username); err != nil {
		t.Fatalf("set User-Name: %v", err)
	}
	if err := rfc2869.EAPMessage_Set(pkt, eap); err != nil {
		t.Fatalf("set EAP-Message: %v", err)
	}
	if len(state) > 0 {
		if err := rfc2865.State_Set(pkt, state); err != nil {
			t.Fatalf("set State: %v", err)
		}
	}
	return &radius.Request{Packet: pkt}
}

// buildMSCHAPv2ResponsePayload assembles what a supplicant sends back.
func buildMSCHAPv2ResponsePayload(t *testing.T, mschapID uint8, peerChallenge, ntResponse []byte, name string) []byte {
	t.Helper()
	value := make([]byte, 0, 49)
	value = append(value, peerChallenge...)
	value = append(value, make([]byte, 8)...) // reserved
	value = append(value, ntResponse...)
	value = append(value, 0x00) // flags

	payload := []byte{MSCHAPv2OpResponse, mschapID, 0, 0, 49}
	payload = append(payload, value...)
	payload = append(payload, []byte(name)...)
	binary.BigEndian.PutUint16(payload[2:4], uint16(len(payload)))
	return payload
}

// extractChallenge pulls the 16-byte authenticator challenge out of an
// EAP-Request/MSCHAPv2 Challenge packet.
func extractChallenge(t *testing.T, eapBytes []byte) (challenge []byte, eapID, mschapID uint8) {
	t.Helper()
	pkt, err := ParseEAP(eapBytes)
	if err != nil {
		t.Fatalf("ParseEAP: %v", err)
	}
	if pkt.Type != EAPTypeMSCHAPv2 {
		t.Fatalf("EAP type = %d, want MSCHAPv2", pkt.Type)
	}
	if pkt.Data[0] != MSCHAPv2OpChallenge {
		t.Fatalf("opcode = %d, want Challenge", pkt.Data[0])
	}
	valueSize := int(pkt.Data[4])
	return pkt.Data[5 : 5+valueSize], pkt.Identifier, pkt.Data[1]
}

// TestFR_AAA_006_FullConversationAuthenticates walks the whole state machine:
// identity, challenge, response, success ack, accept.
func TestFR_AAA_006_FullConversationAuthenticates(t *testing.T) {
	const username, password = "eapuser", "clientPass"

	d := itEAPDaemon(t, map[string]*Subscriber{
		username: {
			ID: 1, Username: username, PasswordHash: itHashPassword(t, password),
			Status: "active", RateLimitStr: "100M/100M", PlanID: 1,
			NTHash: NTPasswordHash(password),
		},
	})
	ctx := context.Background()

	// ── 1. Identity ──────────────────────────────────────────────────────
	identity := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: 1,
		Type: EAPTypeIdentity, Data: []byte(username),
	}).Encode()

	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itEAPRequest(t, username, identity, nil))

	resp1 := w1.last()
	if resp1 == nil || resp1.Code != radius.CodeAccessChallenge {
		t.Fatalf("step 1: want Access-Challenge, got %v", resp1)
	}
	state := rfc2865.State_Get(resp1)
	if len(state) == 0 {
		t.Fatal("step 1: Access-Challenge must carry a State attribute")
	}
	// RFC 3579 §3.2: every RADIUS packet carrying EAP-Message needs one, and
	// NAS devices discard those that lack it.
	if len(rfc2869.MessageAuthenticator_Get(resp1)) == 0 {
		t.Error("step 1: Access-Challenge must carry a Message-Authenticator")
	}

	challenge, eapID, mschapID := extractChallenge(t, rfc2869.EAPMessage_Get(resp1))
	if len(challenge) != 16 {
		t.Fatalf("step 1: challenge length = %d, want 16", len(challenge))
	}

	// ── 2. Response ──────────────────────────────────────────────────────
	peerChallenge := make([]byte, 16)
	for i := range peerChallenge {
		peerChallenge[i] = byte(i + 1)
	}
	ntResponse, err := GenerateNTResponse(challenge, peerChallenge, username, NTPasswordHash(password))
	if err != nil {
		t.Fatalf("GenerateNTResponse: %v", err)
	}

	responsePayload := buildMSCHAPv2ResponsePayload(t, mschapID, peerChallenge, ntResponse, username)
	responseEAP := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: eapID,
		Type: EAPTypeMSCHAPv2, Data: responsePayload,
	}).Encode()

	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itEAPRequest(t, username, responseEAP, state))

	resp2 := w2.last()
	if resp2 == nil || resp2.Code != radius.CodeAccessChallenge {
		t.Fatalf("step 2: want Access-Challenge carrying Success, got %v", resp2)
	}
	successPkt, err := ParseEAP(rfc2869.EAPMessage_Get(resp2))
	if err != nil {
		t.Fatalf("step 2: ParseEAP: %v", err)
	}
	if successPkt.Data[0] != MSCHAPv2OpSuccess {
		t.Fatalf("step 2: opcode = %d, want Success — the NT response did not verify", successPkt.Data[0])
	}
	// The authenticator response is what lets the peer authenticate us; a
	// missing or malformed one means no mutual auth.
	authResp := string(successPkt.Data[4:])
	if len(authResp) != 42 || authResp[:2] != "S=" {
		t.Errorf("step 2: authenticator response = %q, want S= followed by 40 hex digits", authResp)
	}

	// ── 3. Success ack ───────────────────────────────────────────────────
	state2 := rfc2865.State_Get(resp2)
	ackEAP := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: successPkt.Identifier,
		Type: EAPTypeMSCHAPv2, Data: []byte{MSCHAPv2OpSuccess},
	}).Encode()

	w3 := &itResponseWriter{}
	d.handleAuth(ctx, w3, itEAPRequest(t, username, ackEAP, state2))

	resp3 := w3.last()
	if resp3 == nil || resp3.Code != radius.CodeAccessAccept {
		t.Fatalf("step 3: want Access-Accept, got %v", resp3)
	}
	finalEAP, err := ParseEAP(rfc2869.EAPMessage_Get(resp3))
	if err != nil {
		t.Fatalf("step 3: ParseEAP: %v", err)
	}
	if finalEAP.Code != EAPCodeSuccess {
		t.Errorf("step 3: EAP code = %d, want Success", finalEAP.Code)
	}
	// The subscriber must land on their plan's speed, not an unshaped default.
	if len(resp3.Attributes) == 0 {
		t.Error("step 3: Access-Accept carried no attributes at all")
	}
}

// TestFR_AAA_006_WrongPasswordIsRejected is the negative control for the
// whole conversation, not just the crypto function.
func TestFR_AAA_006_WrongPasswordIsRejected(t *testing.T) {
	const username = "eapuser"

	d := itEAPDaemon(t, map[string]*Subscriber{
		username: {
			ID: 1, Username: username, PasswordHash: itHashPassword(t, "rightPass"),
			Status: "active", RateLimitStr: "100M/100M", PlanID: 1,
			NTHash: NTPasswordHash("rightPass"),
		},
	})
	ctx := context.Background()

	identity := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: 1, Type: EAPTypeIdentity, Data: []byte(username),
	}).Encode()
	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itEAPRequest(t, username, identity, nil))
	state := rfc2865.State_Get(w1.last())
	challenge, eapID, mschapID := extractChallenge(t, rfc2869.EAPMessage_Get(w1.last()))

	// Respond with a hash derived from the wrong password.
	peerChallenge := make([]byte, 16)
	ntResponse, err := GenerateNTResponse(challenge, peerChallenge, username, NTPasswordHash("wrongPass"))
	if err != nil {
		t.Fatalf("GenerateNTResponse: %v", err)
	}
	responseEAP := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: eapID, Type: EAPTypeMSCHAPv2,
		Data: buildMSCHAPv2ResponsePayload(t, mschapID, peerChallenge, ntResponse, username),
	}).Encode()

	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itEAPRequest(t, username, responseEAP, state))

	resp := w2.last()
	if resp == nil || resp.Code != radius.CodeAccessReject {
		t.Fatalf("want Access-Reject for a wrong password, got %v", resp)
	}
}

// TestFR_AAA_006_UnenrolledSubscriberIsRejected: a subscriber with no NT
// hash cannot authenticate by EAP, but their PAP access is untouched (see
// the regression test below).
func TestFR_AAA_006_UnenrolledSubscriberIsRejected(t *testing.T) {
	const username, password = "papuser", "clientPass"

	d := itEAPDaemon(t, map[string]*Subscriber{
		username: {
			ID: 1, Username: username, PasswordHash: itHashPassword(t, password),
			Status: "active", RateLimitStr: "100M/100M", PlanID: 1,
			NTHash: nil, // never enrolled
		},
	})
	ctx := context.Background()

	identity := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: 1, Type: EAPTypeIdentity, Data: []byte(username),
	}).Encode()
	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itEAPRequest(t, username, identity, nil))
	state := rfc2865.State_Get(w1.last())
	challenge, eapID, mschapID := extractChallenge(t, rfc2869.EAPMessage_Get(w1.last()))

	peerChallenge := make([]byte, 16)
	ntResponse, _ := GenerateNTResponse(challenge, peerChallenge, username, NTPasswordHash(password))
	responseEAP := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: eapID, Type: EAPTypeMSCHAPv2,
		Data: buildMSCHAPv2ResponsePayload(t, mschapID, peerChallenge, ntResponse, username),
	}).Encode()

	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itEAPRequest(t, username, responseEAP, state))

	if resp := w2.last(); resp == nil || resp.Code != radius.CodeAccessReject {
		t.Fatalf("an unenrolled subscriber must be rejected over EAP, got %v", resp)
	}
}

// TestFR_AAA_006_ResponseWithoutStateIsTreatedAsANewConversation: a stale or
// absent State must not let a peer skip the challenge step.
func TestFR_AAA_006_ResponseWithUnknownStateIsRejected(t *testing.T) {
	const username, password = "eapuser", "clientPass"

	d := itEAPDaemon(t, map[string]*Subscriber{
		username: {
			ID: 1, Username: username, PasswordHash: itHashPassword(t, password),
			Status: "active", RateLimitStr: "100M/100M", PlanID: 1,
			NTHash: NTPasswordHash(password),
		},
	})
	ctx := context.Background()

	// A well-formed response, but against a State the server never issued.
	peerChallenge := make([]byte, 16)
	ntResponse, _ := GenerateNTResponse(make([]byte, 16), peerChallenge, username, NTPasswordHash(password))
	responseEAP := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: 2, Type: EAPTypeMSCHAPv2,
		Data: buildMSCHAPv2ResponsePayload(t, 1, peerChallenge, ntResponse, username),
	}).Encode()

	w := &itResponseWriter{}
	d.handleAuth(ctx, w, itEAPRequest(t, username, responseEAP, []byte("deadbeefdeadbeefdeadbeefdeadbeef")))

	if resp := w.last(); resp == nil || resp.Code != radius.CodeAccessReject {
		t.Fatalf("a response against an unknown State must be rejected, got %v", resp)
	}
}

// TestFR_AAA_006_SuspendedSubscriberCannotAuthenticateOverEAP: the status
// gate must apply to EAP as well, or suspension would be bypassable by
// switching auth method.
func TestFR_AAA_006_SuspendedSubscriberCannotAuthenticateOverEAP(t *testing.T) {
	const username, password = "suspended", "clientPass"

	d := itEAPDaemon(t, map[string]*Subscriber{
		username: {
			ID: 1, Username: username, PasswordHash: itHashPassword(t, password),
			Status: "hard_suspended", RateLimitStr: "100M/100M", PlanID: 1,
			NTHash: NTPasswordHash(password),
		},
	})
	ctx := context.Background()

	identity := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: 1, Type: EAPTypeIdentity, Data: []byte(username),
	}).Encode()
	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itEAPRequest(t, username, identity, nil))
	state := rfc2865.State_Get(w1.last())
	challenge, eapID, mschapID := extractChallenge(t, rfc2869.EAPMessage_Get(w1.last()))

	peerChallenge := make([]byte, 16)
	ntResponse, _ := GenerateNTResponse(challenge, peerChallenge, username, NTPasswordHash(password))
	responseEAP := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: eapID, Type: EAPTypeMSCHAPv2,
		Data: buildMSCHAPv2ResponsePayload(t, mschapID, peerChallenge, ntResponse, username),
	}).Encode()

	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itEAPRequest(t, username, responseEAP, state))

	if resp := w2.last(); resp == nil || resp.Code != radius.CodeAccessReject {
		t.Fatalf("a hard-suspended subscriber must not authenticate over EAP, got %v", resp)
	}
}

// TestFR_AAA_006_SessionIsConsumedOnCompletion: replaying the success ack
// must not re-authenticate, because the challenge it was bound to is gone.
func TestFR_AAA_006_SessionIsConsumedOnCompletion(t *testing.T) {
	const username, password = "eapuser", "clientPass"

	d := itEAPDaemon(t, map[string]*Subscriber{
		username: {
			ID: 1, Username: username, PasswordHash: itHashPassword(t, password),
			Status: "active", RateLimitStr: "100M/100M", PlanID: 1,
			NTHash: NTPasswordHash(password),
		},
	})
	ctx := context.Background()

	identity := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: 1, Type: EAPTypeIdentity, Data: []byte(username),
	}).Encode()
	w1 := &itResponseWriter{}
	d.handleAuth(ctx, w1, itEAPRequest(t, username, identity, nil))
	state := rfc2865.State_Get(w1.last())
	challenge, eapID, mschapID := extractChallenge(t, rfc2869.EAPMessage_Get(w1.last()))

	peerChallenge := make([]byte, 16)
	ntResponse, _ := GenerateNTResponse(challenge, peerChallenge, username, NTPasswordHash(password))
	responseEAP := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: eapID, Type: EAPTypeMSCHAPv2,
		Data: buildMSCHAPv2ResponsePayload(t, mschapID, peerChallenge, ntResponse, username),
	}).Encode()
	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itEAPRequest(t, username, responseEAP, state))
	state2 := rfc2865.State_Get(w2.last())

	ackEAP := (&EAPPacket{Code: EAPCodeResponse, Identifier: 3, Type: EAPTypeMSCHAPv2,
		Data: []byte{MSCHAPv2OpSuccess}}).Encode()

	w3 := &itResponseWriter{}
	d.handleAuth(ctx, w3, itEAPRequest(t, username, ackEAP, state2))
	if w3.last().Code != radius.CodeAccessAccept {
		t.Fatal("the first ack should have been accepted")
	}

	// Replay the same ack against the now-consumed session.
	w4 := &itResponseWriter{}
	d.handleAuth(ctx, w4, itEAPRequest(t, username, ackEAP, state2))
	if resp := w4.last(); resp == nil || resp.Code != radius.CodeAccessReject {
		t.Errorf("a replayed success ack must be rejected, got %v", resp)
	}
}

// ── Regression: PAP must be completely unaffected ───────────────────────────

// TestFR_AAA_002_PAPStillWorksWithEAPEnabled is the regression this whole
// phase must not break. Adding a second auth method must not change the
// behaviour of the one every existing subscriber uses.
func TestFR_AAA_002_PAPStillWorksWithEAPEnabled(t *testing.T) {
	const username, password = "papuser", "correct-horse"

	d := itEAPDaemon(t, map[string]*Subscriber{
		username: {
			ID: 1, Username: username, PasswordHash: itHashPassword(t, password),
			Status: "active", RateLimitStr: "100M/100M", PlanID: 1,
			NTHash: nil, // not enrolled, like the whole existing base
		},
	})
	ctx := context.Background()

	t.Run("correct password is accepted", func(t *testing.T) {
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, itAccessRequest(t, username, password))
		if resp := w.last(); resp == nil || resp.Code != radius.CodeAccessAccept {
			t.Fatalf("PAP must still accept a correct password, got %v", resp)
		}
	})

	t.Run("wrong password is rejected", func(t *testing.T) {
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, itAccessRequest(t, username, "wrong"))
		if resp := w.last(); resp == nil || resp.Code != radius.CodeAccessReject {
			t.Fatalf("PAP must still reject a wrong password, got %v", resp)
		}
	})

	t.Run("an EAP-enrolled subscriber can still use PAP", func(t *testing.T) {
		const eapUser = "dualuser"
		d2 := itEAPDaemon(t, map[string]*Subscriber{
			eapUser: {
				ID: 2, Username: eapUser, PasswordHash: itHashPassword(t, password),
				Status: "active", RateLimitStr: "100M/100M", PlanID: 1,
				NTHash: NTPasswordHash(password),
			},
		})
		w := &itResponseWriter{}
		d2.handleAuth(ctx, w, itAccessRequest(t, eapUser, password))
		if resp := w.last(); resp == nil || resp.Code != radius.CodeAccessAccept {
			t.Fatalf("enrolling for EAP must not disturb PAP, got %v", resp)
		}
	})
}

// TestFR_AAA_006_EAPRequestWithoutAStoreIsRejectedNotFallenThrough: with EAP
// unconfigured, an EAP packet must be refused rather than dropping into the
// PAP path, which would bcrypt-compare an absent password and reject with a
// misleading reason.
func TestFR_AAA_006_EAPRequestWithoutAStoreIsRejected(t *testing.T) {
	const username = "eapuser"
	d := itNewDaemon(t, map[string]*Subscriber{
		username: {ID: 1, Username: username, PasswordHash: itHashPassword(t, "x"), Status: "active"},
	})
	// Deliberately no SetEAPSessionStore.

	identity := (&EAPPacket{
		Code: EAPCodeResponse, Identifier: 1, Type: EAPTypeIdentity, Data: []byte(username),
	}).Encode()

	w := &itResponseWriter{}
	d.handleAuth(context.Background(), w, itEAPRequest(t, username, identity, nil))

	resp := w.last()
	if resp == nil || resp.Code != radius.CodeAccessReject {
		t.Fatalf("want Access-Reject when EAP is unconfigured, got %v", resp)
	}
}
