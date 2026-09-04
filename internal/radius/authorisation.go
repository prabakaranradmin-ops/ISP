package radius

// AuthorisesService reports whether a subscriber in this status may be put on
// the network. PAP, EAP-MSCHAPv2 and MAB all consult it, so suspension cannot
// be bypassed by choosing a different access method.
//
// This is an allowlist, and that is the entire point of the function.
//
// The three call sites previously each carried their own denylist:
//
//	if sub.Status == "hard_suspended" || sub.Status == "terminated" { reject }
//
// which grants service to every status not named, including any status added
// later. That is not hypothetical — it is how a subscriber created with no
// plan_expiry stayed online for free indefinitely: nothing in the billing
// path could reach them, and the access path had no opinion about a
// subscriber who had never paid because "never paid" was not on the list.
// Adding 'pending_payment' to a denylist would have fixed that one case and
// left the next one waiting.
//
// Inverted, the default flips: a status nobody has thought about yet grants
// nothing, and enabling access becomes a deliberate edit here rather than an
// omission somewhere else.
//
// grace_period and soft_suspended stay authorised, unchanged from the
// denylist they replace. Both are stages where the subscriber is meant to
// still be reachable while being chased for payment; only hard suspension
// cuts the line.
func AuthorisesService(status string) bool {
	switch status {
	case "active", "grace_period", "soft_suspended":
		return true
	default:
		// pending_payment, hard_suspended, terminated, and anything added
		// later until someone decides otherwise here.
		return false
	}
}
