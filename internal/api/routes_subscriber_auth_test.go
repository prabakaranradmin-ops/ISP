package api

import (
	"os"
	"strings"
	"testing"
)

// TestSubscriberRoutesVerifyThePortalKey reads this package's own route table
// and asserts that every subscriber-role route is wrapped in the middleware
// keyed on the PORTAL secret, never the staff one.
//
// This is a source check rather than a behavioural one on purpose. The two
// subscriber routes that exist have behavioural tests
// (TestFR_ANN_002_APortalSignedTokenReachesTheBannerFeed and the
// FR-NOTIF-013 pair), but those only cover the routes somebody remembered to
// write a test for. A *third* subscriber route wired to `auth` would be a
// 401 for every real customer with nothing failing anywhere — which is
// exactly how the first two shipped broken and stayed that way: the handler,
// the role gate and the tests were all correct, and the key was wrong.
//
// Same shape as TestEveryPageTemplateIsRegistered in internal/staffui, and
// for the same reason: the registry, not the behaviour, was the blind spot.
func TestSubscriberRoutesVerifyThePortalKey(t *testing.T) {
	src, err := os.ReadFile("routes.go")
	if err != nil {
		t.Fatalf("read routes.go: %v", err)
	}

	const roleGate = `RequireRole("subscriber")`
	found := 0
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, roleGate) {
			continue
		}
		found++
		switch {
		case strings.Contains(line, "subscriberAuth("):
			// Correct: verified against the portal secret.
		case strings.Contains(line, "auth("):
			t.Errorf("routes.go:%d wraps a subscriber route in the staff-key middleware:\n\t%s\n"+
				"Subscriber tokens are signed with PORTAL_JWT_SECRET, which can never equal "+
				"JWT_SECRET, so this route answers 401 to every real subscriber. Use "+
				"subscriberAuth.", i+1, strings.TrimSpace(line))
		default:
			t.Errorf("routes.go:%d gates on the subscriber role through neither subscriberAuth "+
				"nor auth, so which key it verifies cannot be told from here:\n\t%s",
				i+1, strings.TrimSpace(line))
		}
	}

	// Nothing to check would mean the role gate was renamed and this test had
	// quietly stopped looking — the failure mode it exists to prevent.
	if found == 0 {
		t.Errorf("no %s found in routes.go: either the subscriber routes are gone or this "+
			"check no longer matches how they are written, and it is asserting nothing", roleGate)
	}
}
