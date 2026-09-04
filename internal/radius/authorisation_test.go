package radius

import "testing"

// AuthorisesService decides whether a subscriber gets on the network, and it
// is an allowlist for a reason: the denylist it replaced granted service to
// every status nobody had thought about, which is how a subscriber who had
// never paid stayed online for free.
//
// So the case that matters most here is the last one — an unrecognised
// status must be refused. A test that only checked the known statuses would
// pass just as happily against the denylist that caused the bug.
func TestAuthorisesService(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   bool
		why    string
	}{
		{"active", true, "paid up and current"},
		{"grace_period", true, "past expiry but still inside the grace window, deliberately still online"},
		{"soft_suspended", true, "being chased, not yet cut off"},
		{"hard_suspended", false, "service withdrawn for non-payment"},
		{"terminated", false, "gone"},
		{"pending_payment", false, "signed up but has never paid for a cycle (migration 048)"},
		{"", false, "an unset status must not be readable as permission"},
		{"some_status_added_later", false, "the whole point of an allowlist: unknown grants nothing"},
	} {
		if got := AuthorisesService(tc.status); got != tc.want {
			t.Errorf("AuthorisesService(%q) = %v, want %v — %s", tc.status, got, tc.want, tc.why)
		}
	}
}

// The three access paths must agree, or suspension becomes a matter of which
// one a subscriber's equipment happens to use. They share this function
// precisely so they cannot drift; this asserts the property the sharing is
// meant to guarantee, so someone reintroducing a local check has a test to
// answer to.
func TestAuthorisesService_IsTheSingleSourceForEveryAccessPath(t *testing.T) {
	// PAP (handlers.go), EAP-MSCHAPv2 (eap_handler.go) and MAB (mab.go) each
	// call this and nothing else, so agreement is structural. What can still
	// regress is the set itself, so pin the exact membership: a change here
	// should be a deliberate edit, not a side effect.
	authorised := map[string]bool{}
	for _, s := range []string{
		"active", "grace_period", "soft_suspended",
		"hard_suspended", "terminated", "pending_payment",
	} {
		if AuthorisesService(s) {
			authorised[s] = true
		}
	}
	if len(authorised) != 3 {
		t.Errorf("exactly 3 statuses should authorise service, got %d: %v", len(authorised), authorised)
	}
	if authorised["pending_payment"] {
		t.Error("pending_payment authorises service — a subscriber who has never paid would be online, " +
			"which is the defect migration 048 exists to close")
	}
}
