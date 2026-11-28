package radius

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// InWalledGarden reports whether a subscriber in this status is let onto the
// network only as far as the payment page: soft suspension, FR-BIL-004.
//
// AuthorisesService still admits them, because the walled garden is how they
// are meant to reach the payment page; this decides what the Accept carries.
// See internal/nas/walledgarden.go for what that is and where it is enforced.
func InWalledGarden(status string) bool { return status == "soft_suspended" }

// LiveSessionMustEnd reports whether moving a subscriber from one status to
// another has to end the session they already have.
//
// A session's terms are fixed at login. The router applies the Accept's
// attributes when the session starts and does not ask again. So any change
// to what the Accept would now say only reaches a live session by ending it:
// the router redials, and the new Accept carries the new terms. Three changes
// qualify:
//
//   - to a status RADIUS refuses (hard suspension, termination), which is
//     the cut-off;
//   - into soft suspension, or the session keeps full service behind a
//     walled garden that only applies at login;
//   - out of soft suspension, or a subscriber who has just paid stays walled
//     in until their line happens to drop, which on always-on PPPoE can be
//     weeks.
//
// Moves between statuses that are let on with the same terms (active and
// grace_period) leave the session alone.
//
// from == "" means the previous status is not known, and counts as a change:
// an unnecessary reconnect costs the subscriber a few seconds, and a missed
// one leaves a paying subscriber walled in or a suspended one online.
func LiveSessionMustEnd(from, to string) bool {
	if from == "" {
		return true
	}
	if from == to {
		return false
	}
	if !AuthorisesService(to) {
		return true
	}
	return InWalledGarden(from) != InWalledGarden(to)
}

// walledGardenRefused counts soft-suspended logins refused because the NAS
// has no walled garden this system can drive. Each one is a subscriber cut
// off days before the ladder says, so it is counted where an alert can see
// it rather than only logged.
var walledGardenRefused = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "radius_walled_garden_refused_total",
	Help: "Soft-suspended logins refused because the NAS vendor has no walled garden",
}, []string{"vendor"})
