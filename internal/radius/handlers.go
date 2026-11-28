package radius

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// handleRequest dispatches Access-Request or Accounting-Request packets.
//
// parent is the daemon lifetime, so a shutdown cancels in-flight backend calls
// rather than leaving workers blocked on Redis or PostgreSQL.
func (d *RadiusDaemon) handleRequest(parent context.Context, w radius.ResponseWriter, r *radius.Request) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	switch r.Code {
	case radius.CodeAccessRequest:
		d.handleAuth(ctx, signingWriter{w}, r)
	case radius.CodeAccountingRequest:
		d.handleAccounting(ctx, w, r)
	default:
		// Ignore unknown packet types
	}
}

// handleAuth validates username/password and checks subscriber status.
//
// FR: FR-AAA-001..002 | DDS §5.1
func (d *RadiusDaemon) handleAuth(ctx context.Context, w radius.ResponseWriter, r *radius.Request) {
	// Timed here rather than in the worker loop so radius_auth_duration_seconds
	// measures only Access-Request handling, as its name promises.
	timer := prometheus.NewTimer(radiusAuthDuration)
	defer timer.ObserveDuration()

	// EAP first: an Access-Request carrying an EAP-Message is a
	// challenge-response conversation with no User-Password to compare, so
	// the PAP path below cannot answer it (FR-AAA-006, MDS §4.18).
	// handleEAP reports whether it took ownership of the packet.
	if d.handleEAP(ctx, w, r) {
		return
	}

	// MAC Auth Bypass (FR-HSP-002, MDS §4.23). Like EAP, it reports whether it
	// took ownership. Checked before the PAP path because a MAB request has no
	// password to compare and would otherwise be rejected for a missing
	// subscriber rather than handled.
	if d.handleMAB(ctx, w, r) {
		return
	}

	username := rfc2865.UserName_GetString(r.Packet)
	password := rfc2865.UserPassword_GetString(r.Packet)

	// Brute-force lockout is checked before any credential work so that a banned
	// username costs neither a DB round-trip nor a bcrypt comparison.
	blocked, hasFailures, err := d.guard.Check(ctx, username)
	if err != nil {
		log.Error().Err(err).Str("username", username).Msg("radius: brute-force check failed")
	} else if blocked {
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r, RejectLockedOut, MethodPAP)
		return
	}

	sub, err := d.db.GetSubscriberByUsername(ctx, username)
	if err != nil || sub == nil {
		d.recordAuthFailure(ctx, username)
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r, RejectUnknownUser, MethodPAP)
		return
	}

	// Allowlist, not a denylist: an unrecognised status grants nothing.
	if !AuthorisesService(sub.Status) {
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r, RejectNotAuthorised, MethodPAP)
		return
	}

	// Fast path: skip bcrypt cost=12 (~280ms, ~19x the 15ms p99 budget) if this
	// exact password was already bcrypt-verified against this exact password
	// hash recently. Binding to sub.PasswordHash (not just the password) means
	// a password change self-invalidates immediately: the old password no
	// longer matches the cached verifier once the hash changes, even within
	// the cache's TTL. A miss or mismatch here is NOT a rejection — it only
	// means "pay the full bcrypt cost below", same as if the cache did not
	// exist.
	authenticated, err := d.verifierCache.Check(ctx, username, password, sub.PasswordHash)
	if err != nil {
		log.Warn().Err(err).Str("username", username).Msg("radius: verifier cache check failed")
	}

	if !authenticated {
		// bcrypt password check (cost=12 per spec) — the authoritative check.
		if err := bcrypt.CompareHashAndPassword([]byte(sub.PasswordHash), []byte(password)); err != nil {
			d.recordAuthFailure(ctx, username)
			w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
			d.authRejected(r, RejectBadPassword, MethodPAP)
			return
		}
		if err := d.verifierCache.Store(ctx, sub.ID, username, password, sub.PasswordHash); err != nil {
			log.Warn().Err(err).Str("username", username).Msg("radius: verifier cache store failed")
		}
	}

	// Successful auth clears the failure counter so a later typo does not inherit
	// attempts from an old burst. Skipped when the Check above found no counter,
	// which is the common case and saves a Redis round-trip on the hot path.
	if hasFailures {
		if err := d.guard.Reset(ctx, username); err != nil {
			log.Warn().Err(err).Str("username", username).Msg("radius: brute-force reset failed")
		}
	}

	// The connection limit (connlimit.go). Checked after the password, so
	// the refusal confirms nothing to somebody guessing, and not counted as
	// a failed attempt: the credentials were right.
	if d.overConnectionLimit(ctx, sub, r) {
		w.Write(connectionLimitReject(r)) //nolint:errcheck,gosec
		d.authRejected(r, RejectConnectionLimit, MethodPAP)
		return
	}

	// Build Accept response with vendor-appropriate bandwidth attributes
	// (FR-NAS-001..004, MDS §4.11): resolved per requesting NAS when a
	// resolver is wired, falling back to the MikroTik VSA unconditionally
	// (today's exact, unchanged behavior) when it is not.
	resp := r.Response(radius.CodeAccessAccept)
	if err := d.applyRateLimit(resp, sub, r); err != nil {
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r, RejectNotAuthorised, MethodPAP)
		return
	}
	if d.withholdStaticAddress(ctx, sub, r) {
		rfc2865.FramedIPAddress_Del(resp)
	}

	w.Write(resp) //nolint:errcheck,gosec
	d.authAccepted(r, MethodPAP)
}

// connectionLimitReject is the Access-Reject for a login over the
// subscriber's connection limit. The Reply-Message reaches the router's log,
// so whoever is looking at the second router can see why it will not
// connect without ringing the ISP.
func connectionLimitReject(r *radius.Request) *radius.Packet {
	resp := r.Response(radius.CodeAccessReject)
	_ = rfc2865.ReplyMessage_SetString(resp, "Connection limit reached for this account")
	return resp
}

// applyRateLimit attaches the vendor-appropriate bandwidth attributes for a
// subscriber's plan (FR-NAS-001..004, MDS §4.11), and for a soft-suspended
// subscriber the walled garden (FR-BIL-004).
//
// Extracted so every authentication method — PAP, EAP-MSCHAPv2 and MAB —
// shapes a session through the same code rather than three copies that can
// drift. That shared path is what makes FR-HSP-003 true: a hotspot session is
// rate-limited and later throttled by the identical machinery as PPPoE,
// because it is literally the same Access-Accept construction. It is also
// what makes the walled garden impossible to step around by choosing a
// different access method.
//
// It returns an error, with nothing added to resp, only when the subscriber
// is soft-suspended and the NAS has no walled garden (nas.ErrNoWalledGarden).
// The caller must then refuse the login: accepting it would hand a
// subscriber suspended for not paying their full service, which is what soft
// suspension did before the walled garden existed.
func (d *RadiusDaemon) applyRateLimit(resp *radius.Packet, sub *Subscriber, r *radius.Request) error {
	vendor := nas.VendorMikrotik
	profileName := ""
	if d.nasResolver != nil {
		device := d.nasResolver.ResolveAddr(r.RemoteAddr)
		vendor = device.Vendor
		profileName = d.nasResolver.ResolveProfile(sub.PlanID, vendor)
	}

	walled := InWalledGarden(sub.Status)
	var garden []nas.Attr
	if walled {
		var err error
		if garden, err = nas.WalledGardenAttrs(vendor); err != nil {
			walledGardenRefused.WithLabelValues(string(vendor)).Inc()
			log.Warn().Err(err).Str("username", sub.Username).Str("vendor", string(vendor)).
				Msg("radius: soft-suspended login refused: this NAS has no walled garden, " +
					"and accepting it would give full service")
			return err
		}
	}

	applyStaticIP(resp, sub)

	rateLimit, shaping := effectiveRateLimit(sub)

	attrs, err := nas.BuildAcceptAttrs(vendor, nas.RateProfile{
		RateLimitString: rateLimit, ProfileName: profileName, Shaping: shaping})
	if err != nil {
		// Not a reject: the subscriber is legitimate and should still get
		// online. They connect without a bandwidth attribute — the same
		// silent-no-enforcement outcome an unclassified NAS already has
		// today — logged and metered so it is found, not just tolerated.
		log.Warn().Err(err).Str("username", sub.Username).Str("vendor", string(vendor)).
			Msg("radius: vendor attribute build failed, Accept sent without a bandwidth attribute")
	}
	for _, a := range attrs {
		resp.Add(a.Type, a.Value)
	}

	// The plan's extra reply attributes (FR-CAT-004), added after the
	// bandwidth attribute rather than before. Order is not significant to a
	// NAS, but it keeps the packet readable in a capture: the shaping first,
	// then whatever the operator added on top.
	//
	// Nothing here can fail: the resolver validated and encoded these at
	// refresh time, so a bad value was logged and dropped a minute ago
	// rather than costing this subscriber their authentication. The
	// dictionary also refuses any attribute a bandwidth builder sends, so
	// these cannot compete with the attributes added above.
	//
	// Behind the walled garden, the plan's address list and IPv6 delegation
	// give way to the garden's own list; see nas.SupersededByWalledGarden.
	if d.nasResolver != nil {
		for _, a := range d.nasResolver.ResolveReplyAttributes(sub.PlanID, vendor) {
			if walled && nas.SupersededByWalledGarden(a) {
				continue
			}
			resp.Add(a.Type, a.Value)
		}
	}
	for _, a := range garden {
		resp.Add(a.Type, a.Value)
	}
	return nil
}

// applyStaticIP attaches Framed-IP-Address when the subscriber holds a
// static address from an IP pool (FR-NET-005), so the NAS gives them that
// address rather than one from its own pool.
//
// Attached inside applyRateLimit for the reason that function exists: PAP,
// EAP-MSCHAPv2 and MAB all shape their Access-Accept through it, and a
// subscriber's address must not depend on which method they authenticated
// with.
//
// A malformed stored address is logged and dropped rather than rejecting
// the authentication. The subscriber is legitimate; the worst outcome of
// carrying on is that they get a dynamic address, which is what they would
// have had before this feature, whereas refusing would put a customer
// offline over a bad row.
func applyStaticIP(resp *radius.Packet, sub *Subscriber) {
	if sub.StaticIP == "" {
		return
	}
	ip := net.ParseIP(sub.StaticIP)
	if ip == nil {
		log.Warn().Str("username", sub.Username).Str("static_ip", sub.StaticIP).
			Msg("radius: stored static address is not an IP, Accept sent without Framed-IP-Address")
		return
	}
	// Framed-IP-Address is an IPv4 attribute (RFC 2865 §5.8). An IPv6
	// address belongs in Framed-IPv6-Prefix, which this does not yet set —
	// so it is skipped rather than silently truncated to four bytes, which
	// is what To4() would hand the NAS.
	v4 := ip.To4()
	if v4 == nil {
		log.Warn().Str("username", sub.Username).Str("static_ip", sub.StaticIP).
			Msg("radius: static address is IPv6; Framed-IP-Address carries IPv4 only, Accept sent without it")
		return
	}
	if err := rfc2865.FramedIPAddress_Set(resp, v4); err != nil {
		log.Warn().Err(err).Str("username", sub.Username).
			Msg("radius: could not set Framed-IP-Address, Accept sent without it")
	}
}

// recordAuthFailure increments the brute-force counter for a rejected credential.
func (d *RadiusDaemon) recordAuthFailure(ctx context.Context, username string) {
	if err := d.guard.RecordFailure(ctx, username); err != nil {
		log.Error().Err(err).Str("username", username).Msg("radius: brute-force record failed")
	}
}

// RateLimitForSubscriber returns the effective rate-limit string (respects
// a speed override, then FUP, then the plan rate).
//
// Deliberately drops the bandwidth profile: this is the plain-string form
// for callers that only need a rate to display or log. A caller building an
// Access-Accept or CoA must use effectiveRateLimit and pass the profile on,
// or the subscriber silently loses their burst.
func RateLimitForSubscriber(sub *Subscriber) string {
	rl, _ := effectiveRateLimit(sub)
	return rl
}

// effectiveRateLimit picks the rate an Access-Accept or CoA should carry.
// A manual speed override — set from the console for a limited time or
// until cleared — wins over the automatic FUP throttle, which wins over the
// plan's own rate. This is the same three-tier precedence
// GetSubscriberNASSession applies for CoA sends (internal/db/fup.go); kept
// here too because Access-Accept is built from the cached *Subscriber, not
// a fresh DB round trip, so the expiry check has to happen against
// time.Now() rather than the database's own now().
// It returns the plain rate-limit string and, when the plan's own shaping
// applies, the bandwidth profile behind it (FR-CAT-002).
//
// A profile is returned only in the last case. An override and a FUP throttle
// are deliberate temporary states expressed as a plain "down/up" rate, and
// returning the plan's burst alongside one of them would hand the subscriber
// the burst their throttle exists to take away.
func effectiveRateLimit(sub *Subscriber) (string, *nas.Shaping) {
	if sub.SpeedOverrideRateLimit != "" &&
		(sub.SpeedOverrideExpiresAt == nil || sub.SpeedOverrideExpiresAt.After(time.Now())) {
		return sub.SpeedOverrideRateLimit, nil
	}
	if sub.FUPActive && sub.FUPThrottle != "" {
		return sub.FUPThrottle, nil
	}
	if sub.Shaping != nil {
		// RateLimitStr is still carried as the fallback the non-MikroTik
		// dialects parse if the profile cannot render.
		return sub.RateLimitStr, sub.Shaping
	}
	return sub.RateLimitStr, nil
}

// BruteForceKey returns the Redis key used for brute-force rate limiting.
func BruteForceKey(username string) string {
	return fmt.Sprintf("bf_attempts:%s", username)
}
