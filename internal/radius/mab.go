package radius

import (
	"context"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

// MAC Auth Bypass — FR-HSP-002 | MDS §4.23.
//
// MAB authenticates a device by its MAC address alone: the NAS sends an
// Access-Request whose User-Name is the MAC and whose User-Password is either
// absent or the MAC repeated. There is no secret involved, which is the whole
// point (a walk-up phone has no credentials to type) and also the whole
// problem — a MAC travels in the clear on every frame and any client can set
// its own.
//
// Three things bound that exposure, and all three are load-bearing:
//
//  1. **Per-NAS opt-in.** nas_devices.allow_mab defaults FALSE, and an
//     unregistered NAS resolves to a Device whose zero value is also false.
//     MAB is therefore unreachable unless an operator deliberately enabled it
//     on a specific, registered NAS.
//  2. **The MAC must be pre-registered** to a subscriber in hotspot_devices,
//     or backed by a live captive-portal grant. An unknown MAC is rejected;
//     MAB is not an open door onto an enabled NAS.
//  3. **The registration is bound to the NAS.** A device registered on the
//     café hotspot cannot authenticate on a different operator's NAS that
//     also happens to have MAB enabled.
//
// The subscriber status gate and rate-limit attributes apply exactly as they
// do to PAP, or suspension would be bypassable by switching auth method —
// the same property EAP had to preserve in §4.18.

var (
	mabAttemptsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "radius_mab_attempts_total",
		Help: "MAC Auth Bypass attempts, by outcome",
	}, []string{"outcome"})
	// A spike here is the signal that matters: an Access-Request shaped like
	// MAB arriving from a NAS that has not enabled it is either a
	// misconfiguration or somebody probing for one.
	mabRefusedNASTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_mab_refused_nas_total",
		Help: "MAB-shaped requests from a NAS without allow_mab",
	})
)

// MABQuerier resolves a MAC to the subscriber allowed to use it.
type MABQuerier interface {
	// AuthorizeMAC returns the subscriber a MAC may authenticate as on this
	// NAS, or nil when the MAC is unknown, inactive, bound to a different NAS,
	// or has no live grant.
	AuthorizeMAC(ctx context.Context, mac string, nasID int) (*Subscriber, error)
}

// SetMABQuerier enables MAC Auth Bypass handling. Without it, a MAB-shaped
// request falls through to the ordinary PAP path and is rejected for having
// no matching subscriber — which is the correct behaviour for a deployment
// that does not run a hotspot.
func (d *RadiusDaemon) SetMABQuerier(q MABQuerier) { d.mabDB = q }

// looksLikeMAB reports whether an Access-Request is a MAC-auth attempt.
//
// The heuristic is deliberately narrow: User-Name must normalise to a MAC, and
// the password must be either empty or the same MAC (which is what MikroTik,
// Cisco and Ubiquiti all send). A subscriber whose username happens to be a
// MAC-shaped string but who sends a real password is NOT treated as MAB, so a
// genuine credential is never silently downgraded to a weaker check.
func looksLikeMAB(username, password string) bool {
	mac, ok := NormaliseMAC(username)
	if !ok {
		return false
	}
	if password == "" {
		return true
	}
	pw, ok := NormaliseMAC(password)
	return ok && pw == mac
}

// NormaliseMAC canonicalises a MAC to uppercase colon-separated form.
//
// NAS vendors send every spelling there is — aabb.ccdd.eeff (Cisco),
// aa-bb-cc-dd-ee-ff (Windows), aabbccddeeff (some MikroTik firmwares). Storing
// and comparing one canonical form is what stops the same physical device
// being registered twice and authenticating under a spelling an operator
// never reviewed.
func NormaliseMAC(s string) (string, bool) {
	var hex strings.Builder
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'A' && c <= 'F':
			hex.WriteRune(c)
		case c >= 'a' && c <= 'f':
			hex.WriteRune(c - 32) // to upper
		case c == ':' || c == '-' || c == '.' || c == ' ':
			// separator, skip
		default:
			return "", false
		}
	}
	h := hex.String()
	if len(h) != 12 {
		return "", false
	}

	var out strings.Builder
	for i := 0; i < 12; i += 2 {
		if i > 0 {
			out.WriteByte(':')
		}
		out.WriteString(h[i : i+2])
	}
	return out.String(), true
}

// handleMAB answers a MAC-auth Access-Request. It reports whether it took
// ownership of the packet, mirroring handleEAP's contract.
func (d *RadiusDaemon) handleMAB(ctx context.Context, w radius.ResponseWriter, r *radius.Request) bool {
	username := rfc2865.UserName_GetString(r.Packet)
	password := rfc2865.UserPassword_GetString(r.Packet)
	if !looksLikeMAB(username, password) {
		return false
	}
	if d.mabDB == nil {
		return false // no hotspot configured; fall through to PAP
	}

	// Gate 1: is MAB enabled on the NAS this came from? Checked before any
	// database work, so a probe against a non-hotspot NAS costs nothing.
	//
	// With no resolver configured there is no per-NAS record at all, and
	// treating that as "enabled" would make MAB global on exactly the
	// deployments that never registered their NAS inventory — the opposite of
	// opt-in. So no resolver means no MAB.
	if d.nasResolver == nil {
		mabRefusedNASTotal.Inc()
		mabAttemptsTotal.WithLabelValues("nas_not_enabled").Inc()
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r)
		return true
	}
	device := d.nasResolver.ResolveAddr(r.RemoteAddr)
	if !device.AllowMAB {
		mabRefusedNASTotal.Inc()
		mabAttemptsTotal.WithLabelValues("nas_not_enabled").Inc()
		log.Warn().Str("nas", device.IP).Str("mac", username).
			Msg("radius: MAB request from a NAS without allow_mab — refused")
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r)
		return true
	}

	mac, _ := NormaliseMAC(username)

	// Gate 2: is this MAC known, active, and bound to this NAS?
	sub, err := d.mabDB.AuthorizeMAC(ctx, mac, device.ID)
	if err != nil {
		log.Error().Err(err).Str("mac", mac).Msg("radius: MAB lookup failed")
		mabAttemptsTotal.WithLabelValues("error").Inc()
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r)
		return true
	}
	if sub == nil {
		mabAttemptsTotal.WithLabelValues("unknown_mac").Inc()
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r)
		return true
	}

	// Gate 3: the same status check PAP and EAP apply. Without it, suspension
	// would be bypassable by connecting over the hotspot instead of PPPoE.
	if !AuthorisesService(sub.Status) {
		mabAttemptsTotal.WithLabelValues("suspended").Inc()
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		d.authRejected(r)
		return true
	}

	// Accept, carrying the same vendor rate-limit attributes a PPPoE session
	// would get — which is what makes FR-HSP-003 true rather than aspirational:
	// hotspot sessions are shaped by the same plan and policed by the same CoA
	// machinery, because they are the same Access-Accept.
	resp := r.Response(radius.CodeAccessAccept)
	d.applyRateLimit(resp, sub, r)

	mabAttemptsTotal.WithLabelValues("accepted").Inc()
	d.authAccepted(r)
	w.Write(resp) //nolint:errcheck,gosec
	return true
}
