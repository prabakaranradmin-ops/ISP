package revenue

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestRecoveryRate(t *testing.T) {
	d := func(s string) decimal.Decimal { return decimal.RequireFromString(s) }

	cases := []struct {
		name           string
		current, prior decimal.Decimal
		wantPct        string
		wantComparable bool
	}{
		{"improvement", d("1500"), d("1000"), "50", true},
		{"decline", d("800"), d("1000"), "-20", true},
		{"flat", d("1000"), d("1000"), "0", true},
		// A first month of operation. The change from zero is
		// arithmetically infinite, and printing "+100%" would invent a
		// comparison that does not exist.
		{"no prior month", d("1000"), d("0"), "0", false},
		// Collections stopped entirely: -100% is a real, meaningful figure.
		{"collected nothing this month", d("0"), d("1000"), "-100", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, comparable := RecoveryRate(tc.current, tc.prior)
			if comparable != tc.wantComparable {
				t.Fatalf("comparable = %v, want %v", comparable, tc.wantComparable)
			}
			if comparable && got.String() != tc.wantPct {
				t.Errorf("pct = %s, want %s", got.String(), tc.wantPct)
			}
		})
	}
}

// ServiceStoppedIn encodes which stages mean the subscriber is actually
// off the network. grace_period is the trap: it sounds terminal and is
// not — the dunning scanner leaves those subscribers online.
func TestServiceStoppedIn(t *testing.T) {
	online := []string{"active", "remind_7d", "remind_3d", "remind_1d", "grace_period"}
	stopped := []string{"soft_suspended", "hard_suspended"}

	for _, s := range online {
		if ServiceStoppedIn(s) {
			t.Errorf("%s: reported as service-stopped, but the subscriber is still online", s)
		}
	}
	for _, s := range stopped {
		if !ServiceStoppedIn(s) {
			t.Errorf("%s: reported as online, but service has been restricted", s)
		}
	}
}
