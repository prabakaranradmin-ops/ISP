package radius

import (
	"context"
	"fmt"
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
		d.handleAuth(ctx, w, r)
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
		d.authRejected(r)
		return
	}

	sub, err := d.db.GetSubscriberByUsername(ctx, username)
	if err != nil || sub == nil {
		d.recordAuthFailure(ctx, username)
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r)
		return
	}

	// Reject immediately for hard-suspended / terminated subscribers
	if sub.Status == "hard_suspended" || sub.Status == "terminated" {
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r)
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
			d.authRejected(r)
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

	// Build Accept response with vendor-appropriate bandwidth attributes
	// (FR-NAS-001..004, MDS §4.11): resolved per requesting NAS when a
	// resolver is wired, falling back to the MikroTik VSA unconditionally
	// (today's exact, unchanged behavior) when it is not.
	resp := r.Response(radius.CodeAccessAccept)
	d.applyRateLimit(resp, sub, r)

	w.Write(resp) //nolint:errcheck,gosec
	d.authAccepted(r)
}

// applyRateLimit attaches the vendor-appropriate bandwidth attributes for a
// subscriber's plan (FR-NAS-001..004, MDS §4.11).
//
// Extracted so every authentication method — PAP, EAP-MSCHAPv2 and MAB —
// shapes a session through the same code rather than three copies that can
// drift. That shared path is what makes FR-HSP-003 true: a hotspot session is
// rate-limited and later throttled by the identical machinery as PPPoE,
// because it is literally the same Access-Accept construction.
func (d *RadiusDaemon) applyRateLimit(resp *radius.Packet, sub *Subscriber, r *radius.Request) {
	rateLimit := effectiveRateLimit(sub)

	vendor := nas.VendorMikrotik
	profileName := ""
	if d.nasResolver != nil {
		device := d.nasResolver.ResolveAddr(r.RemoteAddr)
		vendor = device.Vendor
		profileName = d.nasResolver.ResolveProfile(sub.PlanID, vendor)
	}

	attrs, err := nas.BuildAcceptAttrs(vendor, nas.RateProfile{RateLimitString: rateLimit, ProfileName: profileName})
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
}

// recordAuthFailure increments the brute-force counter for a rejected credential.
func (d *RadiusDaemon) recordAuthFailure(ctx context.Context, username string) {
	if err := d.guard.RecordFailure(ctx, username); err != nil {
		log.Error().Err(err).Str("username", username).Msg("radius: brute-force record failed")
	}
}

// RateLimitForSubscriber returns the effective rate-limit string (respects
// a speed override, then FUP, then the plan rate).
func RateLimitForSubscriber(sub *Subscriber) string {
	return effectiveRateLimit(sub)
}

// effectiveRateLimit picks the rate an Access-Accept or CoA should carry.
// A manual speed override — set from the console for a limited time or
// until cleared — wins over the automatic FUP throttle, which wins over the
// plan's own rate. This is the same three-tier precedence
// GetSubscriberNASSession applies for CoA sends (internal/db/fup.go); kept
// here too because Access-Accept is built from the cached *Subscriber, not
// a fresh DB round trip, so the expiry check has to happen against
// time.Now() rather than the database's own now().
func effectiveRateLimit(sub *Subscriber) string {
	if sub.SpeedOverrideRateLimit != "" &&
		(sub.SpeedOverrideExpiresAt == nil || sub.SpeedOverrideExpiresAt.After(time.Now())) {
		return sub.SpeedOverrideRateLimit
	}
	if sub.FUPActive && sub.FUPThrottle != "" {
		return sub.FUPThrottle
	}
	return sub.RateLimitStr
}

// BruteForceKey returns the Redis key used for brute-force rate limiting.
func BruteForceKey(username string) string {
	return fmt.Sprintf("bf_attempts:%s", username)
}
