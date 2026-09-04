package radius

import (
	"context"
	"errors"

	"github.com/rs/zerolog/log"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2869"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// EAP-MSCHAPv2 conversation driver — FR-AAA-006 | MDS §4.18.
//
// The state machine, in the order a supplicant walks it:
//
//	Access-Request(EAP-Identity)              -> Access-Challenge(MSCHAPv2 Challenge)  [challenge_issued]
//	Access-Request(EAP-Response/MSCHAPv2)     -> Access-Challenge(MSCHAPv2 Success)    [awaiting_success_ack]
//	Access-Request(EAP-Response ack)          -> Access-Accept(+ rate-limit VSAs)
//
// Every step other than the last carries a State attribute the NAS echoes
// back, which is how three independent UDP exchanges become one
// authentication.

// handleEAP processes an Access-Request carrying an EAP-Message.
//
// Returns true when the packet was an EAP packet and has been answered, so
// the caller can fall through to the PAP path when it was not — the two must
// not both respond.
func (d *RadiusDaemon) handleEAP(ctx context.Context, w radius.ResponseWriter, r *radius.Request) bool {
	eapBytes := rfc2869.EAPMessage_Get(r.Packet)
	if len(eapBytes) == 0 {
		return false // not an EAP conversation; PAP handles it
	}
	if d.eapSessions == nil {
		// EAP is not configured on this daemon. Reject rather than silently
		// falling through to PAP, which would try to bcrypt-compare an
		// absent User-Password and reject anyway with a confusing reason.
		log.Warn().Msg("radius: EAP request received but EAP session store is not configured")
		d.writeEAPReject(w, r, 0)
		return true
	}

	pkt, err := ParseEAP(eapBytes)
	if err != nil {
		log.Warn().Err(err).Msg("radius: malformed EAP packet")
		d.writeEAPReject(w, r, 0)
		return true
	}

	// A State attribute means this is a continuation; its absence means a
	// fresh conversation.
	state := string(rfc2865.State_Get(r.Packet))
	if state == "" {
		d.eapStart(ctx, w, r, pkt)
		return true
	}

	sess, err := d.eapSessions.Load(ctx, state)
	if err != nil {
		log.Error().Err(err).Msg("radius: EAP session load failed")
		d.writeEAPReject(w, r, pkt.Identifier)
		return true
	}
	if sess == nil {
		// Expired or unknown State. The supplicant restarts from Identity,
		// which is the normal EAP recovery path.
		log.Info().Msg("radius: EAP response for an unknown or expired session")
		d.writeEAPReject(w, r, pkt.Identifier)
		return true
	}

	switch sess.Stage {
	case StageChallengeIssued:
		d.eapVerifyResponse(ctx, w, r, pkt, state, sess)
	case StageAwaitingSuccessAck:
		d.eapFinish(ctx, w, r, state, sess)
	default:
		log.Warn().Str("stage", string(sess.Stage)).Msg("radius: EAP response arrived at an unexpected stage")
		d.eapFail(ctx, w, r, pkt.Identifier, state, "unexpected_stage")
	}
	return true
}

// eapStart answers the opening packet with an MS-CHAPv2 challenge.
//
// Identity is taken from the EAP packet when present, falling back to
// User-Name: some supplicants send an anonymous EAP identity and put the
// real one in the RADIUS attribute.
func (d *RadiusDaemon) eapStart(ctx context.Context, w radius.ResponseWriter, r *radius.Request, pkt *EAPPacket) {
	username := rfc2865.UserName_GetString(r.Packet)
	if pkt.Type == EAPTypeIdentity && len(pkt.Data) > 0 {
		username = string(pkt.Data)
	}
	username = StripDomain(username)

	if username == "" {
		d.writeEAPReject(w, r, pkt.Identifier)
		return
	}

	// Brute-force lockout applies to EAP exactly as it does to PAP —
	// otherwise an attacker locked out of one method simply switches.
	blocked, _, err := d.guard.Check(ctx, username)
	if err != nil {
		log.Error().Err(err).Str("username", username).Msg("radius: EAP brute-force check failed")
	} else if blocked {
		d.writeEAPReject(w, r, pkt.Identifier)
		d.authRejected(r)
		return
	}

	challenge, err := NewChallenge()
	if err != nil {
		log.Error().Err(err).Msg("radius: could not generate an EAP challenge")
		d.writeEAPReject(w, r, pkt.Identifier)
		return
	}
	state, err := NewState()
	if err != nil {
		log.Error().Err(err).Msg("radius: could not generate an EAP state")
		d.writeEAPReject(w, r, pkt.Identifier)
		return
	}

	sess := &EAPSession{
		Stage:                  StageChallengeIssued,
		Username:               username,
		AuthenticatorChallenge: challenge,
		EAPIdentifier:          pkt.Identifier + 1,
	}
	if err := d.eapSessions.Save(ctx, state, sess); err != nil {
		log.Error().Err(err).Msg("radius: could not store the EAP session")
		d.writeEAPReject(w, r, pkt.Identifier)
		return
	}

	eapSessionsStarted.Inc()

	challengePayload := (&MSCHAPv2Challenge{
		MSCHAPv2ID: pkt.Identifier,
		Challenge:  challenge,
		Name:       eapServerName,
	}).Encode()

	d.writeEAPChallenge(w, r, state, &EAPPacket{
		Code:       EAPCodeRequest,
		Identifier: sess.EAPIdentifier,
		Type:       EAPTypeMSCHAPv2,
		Data:       challengePayload,
	})
}

// eapVerifyResponse checks the peer's NT response against the stored
// challenge and the subscriber's enrolled NT hash.
func (d *RadiusDaemon) eapVerifyResponse(ctx context.Context, w radius.ResponseWriter, r *radius.Request,
	pkt *EAPPacket, state string, sess *EAPSession,
) {
	resp, err := ParseMSCHAPv2Response(pkt.Data)
	if err != nil {
		log.Warn().Err(err).Msg("radius: malformed MS-CHAPv2 response")
		d.eapFail(ctx, w, r, pkt.Identifier, state, "malformed_response")
		return
	}

	sub, err := d.db.GetSubscriberByUsername(ctx, sess.Username)
	if err != nil || sub == nil {
		d.recordAuthFailure(ctx, sess.Username)
		d.eapFail(ctx, w, r, pkt.Identifier, state, "unknown_subscriber")
		return
	}
	if !AuthorisesService(sub.Status) {
		d.eapFail(ctx, w, r, pkt.Identifier, state, "suspended")
		return
	}

	authenticatorResponse, err := VerifyMSCHAPv2(
		sub.NTHash, sess.AuthenticatorChallenge, resp.PeerChallenge, resp.NTResponse, sess.Username)
	if err != nil {
		if errors.Is(err, ErrNoNTHash) {
			// A real and common operational state, not an attack: this
			// subscriber has never been enrolled for EAP. Logged distinctly
			// so "nobody enrolled them" is not investigated as "wrong
			// password".
			log.Info().Str("username", sess.Username).
				Msg("radius: EAP attempted for a subscriber with no enrolled NT hash")
			d.eapFail(ctx, w, r, pkt.Identifier, state, "not_enrolled")
			return
		}
		d.recordAuthFailure(ctx, sess.Username)
		d.eapFail(ctx, w, r, pkt.Identifier, state, "bad_response")
		return
	}

	// Verified. MS-CHAPv2 still requires the peer to acknowledge our Success
	// before EAP-Success, so the conversation is not over yet.
	sess.Stage = StageAwaitingSuccessAck
	sess.PeerChallenge = resp.PeerChallenge
	sess.NTResponse = resp.NTResponse
	sess.MSCHAPv2ID = resp.MSCHAPv2ID
	sess.EAPIdentifier = pkt.Identifier + 1
	sess.AuthenticatorResponse = authenticatorResponse

	if err := d.eapSessions.Save(ctx, state, sess); err != nil {
		log.Error().Err(err).Msg("radius: could not update the EAP session")
		d.eapFail(ctx, w, r, pkt.Identifier, state, "session_store")
		return
	}

	d.writeEAPChallenge(w, r, state, &EAPPacket{
		Code:       EAPCodeRequest,
		Identifier: sess.EAPIdentifier,
		Type:       EAPTypeMSCHAPv2,
		Data:       EncodeMSCHAPv2Success(resp.MSCHAPv2ID, authenticatorResponse),
	})
}

// eapFinish sends Access-Accept once the peer has acknowledged Success.
func (d *RadiusDaemon) eapFinish(ctx context.Context, w radius.ResponseWriter, r *radius.Request,
	state string, sess *EAPSession,
) {
	// The session is finished either way; drop it before writing so a
	// duplicate ack cannot re-authenticate against a live challenge.
	if err := d.eapSessions.Delete(ctx, state); err != nil {
		log.Warn().Err(err).Msg("radius: could not delete the completed EAP session")
	}

	sub, err := d.db.GetSubscriberByUsername(ctx, sess.Username)
	if err != nil || sub == nil {
		d.writeEAPReject(w, r, sess.EAPIdentifier)
		return
	}

	if err := d.guard.Reset(ctx, sess.Username); err != nil {
		log.Warn().Err(err).Str("username", sess.Username).Msg("radius: EAP brute-force reset failed")
	}

	resp := r.Response(radius.CodeAccessAccept)
	success := (&EAPPacket{Code: EAPCodeSuccess, Identifier: sess.EAPIdentifier}).Encode()
	if err := rfc2869.EAPMessage_Set(resp, success); err != nil {
		log.Error().Err(err).Msg("radius: could not set EAP-Success on the accept")
		d.writeEAPReject(w, r, sess.EAPIdentifier)
		return
	}

	// The same vendor-aware rate-limit attributes PAP gets: an EAP
	// subscriber must land on their plan's speed, not an unshaped default.
	rateLimit := sub.RateLimitStr
	if sub.FUPActive && sub.FUPThrottle != "" {
		rateLimit = sub.FUPThrottle
	}
	vendor := nas.VendorMikrotik
	profileName := ""
	if d.nasResolver != nil {
		device := d.nasResolver.ResolveAddr(r.RemoteAddr)
		vendor = device.Vendor
		profileName = d.nasResolver.ResolveProfile(sub.PlanID, vendor)
	}
	if attrs, err := nas.BuildAcceptAttrs(vendor, nas.RateProfile{
		RateLimitString: rateLimit, ProfileName: profileName,
	}); err == nil {
		for _, a := range attrs {
			resp.Add(a.Type, a.Value)
		}
	} else {
		log.Warn().Err(err).Str("vendor", string(vendor)).
			Msg("radius: EAP accept without bandwidth attributes")
	}

	d.writeWithMessageAuthenticator(w, resp)
	eapSessionsCompleted.WithLabelValues("accept").Inc()
	d.authAccepted(r)
}

// eapFail ends a conversation with an MS-CHAPv2 Failure inside an
// Access-Reject, and clears the session.
func (d *RadiusDaemon) eapFail(ctx context.Context, w radius.ResponseWriter, r *radius.Request,
	eapID uint8, state, reason string,
) {
	if state != "" {
		if err := d.eapSessions.Delete(ctx, state); err != nil {
			log.Warn().Err(err).Msg("radius: could not delete the failed EAP session")
		}
	}
	eapSessionsCompleted.WithLabelValues(reason).Inc()
	d.writeEAPReject(w, r, eapID)
}

// writeEAPChallenge sends an Access-Challenge carrying an EAP request and the
// State that ties the next packet back to this conversation.
//
// The setter errors are checked rather than ignored: a RADIUS attribute caps
// at 253 bytes, so an EAP payload above that fails here. Today's challenge
// and success packets are ~37 and ~51 bytes so it cannot trigger, but a
// silently dropped attribute would present as a supplicant that hangs
// forever rather than as an error anybody could find.
func (d *RadiusDaemon) writeEAPChallenge(w radius.ResponseWriter, r *radius.Request, state string, pkt *EAPPacket) {
	resp := r.Response(radius.CodeAccessChallenge)
	if err := rfc2869.EAPMessage_Set(resp, pkt.Encode()); err != nil {
		log.Error().Err(err).Msg("radius: could not set EAP-Message on the challenge")
		d.writeEAPReject(w, r, pkt.Identifier)
		return
	}
	if err := rfc2865.State_Set(resp, []byte(state)); err != nil {
		log.Error().Err(err).Msg("radius: could not set State on the challenge")
		d.writeEAPReject(w, r, pkt.Identifier)
		return
	}
	d.writeWithMessageAuthenticator(w, resp)
}

// writeEAPReject sends an Access-Reject carrying EAP-Failure.
func (d *RadiusDaemon) writeEAPReject(w radius.ResponseWriter, r *radius.Request, eapID uint8) {
	resp := r.Response(radius.CodeAccessReject)
	failure := (&EAPPacket{Code: EAPCodeFailure, Identifier: eapID}).Encode()
	if err := rfc2869.EAPMessage_Set(resp, failure); err != nil {
		// The reject still goes out: refusing to answer at all would leave
		// the supplicant retrying against a server that looks dead.
		log.Error().Err(err).Msg("radius: could not set EAP-Message on the reject")
	}
	d.writeWithMessageAuthenticator(w, resp)
	d.authRejected(r)
}

// writeWithMessageAuthenticator adds the Message-Authenticator attribute
// before sending.
//
// RFC 3579 §3.2 requires it on every RADIUS packet carrying EAP-Message, and
// NAS devices routinely discard EAP packets that lack it. The attribute must
// be present and zeroed while the HMAC is computed over the packet, which is
// what layeh's Write does when the attribute already exists — so it is added
// here rather than left to chance.
func (d *RadiusDaemon) writeWithMessageAuthenticator(w radius.ResponseWriter, resp *radius.Packet) {
	if err := rfc2869.MessageAuthenticator_Set(resp, make([]byte, 16)); err != nil {
		// Send anyway: many NAS devices accept the packet without it, and a
		// response that might be discarded beats no response at all.
		log.Warn().Err(err).Msg("radius: could not set Message-Authenticator")
	}
	if err := w.Write(resp); err != nil {
		log.Error().Err(err).Msg("radius: failed to write EAP response")
	}
}

// eapServerName is the identity this server presents in its MS-CHAPv2
// challenge. Cosmetic to the protocol, but it is what a Windows supplicant
// shows the user when asking whether to trust the network.
const eapServerName = "isp-bss-oss"
