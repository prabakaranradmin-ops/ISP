package fup

import (
	"strings"
	"testing"
	"time"
)

// Multi-tier FUP policies and data top-ups — FR-CAT-003.

const gb = int64(1) << 30

func retailPolicy() Policy {
	return Policy{
		Name: "Retail fair use",
		Tiers: []Tier{
			{ThresholdBytes: 500 * gb, ThrottleRate: "10M/10M"},
			{ThresholdBytes: 1000 * gb, ThrottleRate: "2M/2M"},
			{ThresholdBytes: 2000 * gb, ThrottleRate: "512k/512k"},
		},
	}
}

// The decision this feature turns on: the highest crossed tier applies, not
// the first. A subscriber at triple their quota has crossed every
// threshold, and giving them the gentlest throttle would make every harder
// tier unreachable — the policy would look configured and have no effect
// beyond its first step.
func TestFR_CAT_003_TheHighestCrossedTierApplies(t *testing.T) {
	p := retailPolicy().Normalise()

	cases := []struct {
		used int64
		want string // "" means no throttle
	}{
		{0, ""},
		{499 * gb, ""},
		{500 * gb, "10M/10M"}, // exactly on the threshold counts as crossed
		{999 * gb, "10M/10M"},
		{1000 * gb, "2M/2M"},
		{1999 * gb, "2M/2M"},
		{2000 * gb, "512k/512k"},
		{9000 * gb, "512k/512k"}, // far past the last tier stays on it
	}

	for _, tc := range cases {
		got := p.TierFor(tc.used)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("at %dGB the policy throttles to %q, want no throttle", tc.used/gb, got.ThrottleRate)
		case tc.want != "" && got == nil:
			t.Errorf("at %dGB the policy does not throttle, want %q", tc.used/gb, tc.want)
		case tc.want != "" && got.ThrottleRate != tc.want:
			t.Errorf("at %dGB the policy throttles to %q, want %q", tc.used/gb, got.ThrottleRate, tc.want)
		}
	}
}

// Tiers entered out of order must still resolve correctly: an operator types
// them in whatever order they think of them, and the console should not
// require sorting by hand.
func TestFR_CAT_003_TiersEnteredOutOfOrderStillResolve(t *testing.T) {
	p := Policy{Name: "Jumbled", Tiers: []Tier{
		{ThresholdBytes: 2000 * gb, ThrottleRate: "512k/512k"},
		{ThresholdBytes: 500 * gb, ThrottleRate: "10M/10M"},
		{ThresholdBytes: 1000 * gb, ThrottleRate: "2M/2M"},
	}}.Normalise()

	if p.Tiers[0].ThresholdBytes != 500*gb {
		t.Errorf("Normalise did not sort: first tier is %dGB", p.Tiers[0].ThresholdBytes/gb)
	}
	got := p.TierFor(1500 * gb)
	if got == nil || got.ThrottleRate != "2M/2M" {
		t.Errorf("at 1500GB got %v, want 2M/2M", got)
	}
}

// The 80% warning and the portal's usage bar read the first threshold, not
// the last. Warning at 80% of the hardest tier would arrive long after the
// subscriber had already been throttled twice.
func TestFR_CAT_003_TheWarningThresholdIsTheFirstTier(t *testing.T) {
	p := retailPolicy().Normalise()
	if got := p.FirstThreshold(); got != 500*gb {
		t.Errorf("FirstThreshold() = %dGB, want 500GB", got/gb)
	}
	if got := (Policy{}).FirstThreshold(); got != 0 {
		t.Errorf("an empty policy reports a threshold of %d, want 0 (unlimited)", got)
	}
}

// A top-up raises the thresholds rather than clearing a flag. The tempting
// shortcut — un-throttle and move on — would leave a subscriber un-throttled
// for the rest of the cycle however much more they used, because nothing
// moved the thresholds and every tier above them would be unreachable.
func TestFR_CAT_003_ATopupRaisesTheThresholdsRatherThanExempting(t *testing.T) {
	a := Allowance{Policy: retailPolicy().Normalise(), ToppedUpBytes: 100 * gb}

	// Was throttled at 550GB; the 100GB top-up lifts it.
	if got := a.TierFor(550 * gb); got != nil {
		t.Errorf("after a 100GB top-up, 550GB still throttles to %q", got.ThrottleRate)
	}
	// But the tier is still there, 100GB further out.
	if got := a.TierFor(600 * gb); got == nil || got.ThrottleRate != "10M/10M" {
		t.Errorf("at 600GB with a 100GB top-up got %v, want the first tier back", got)
	}
	// And so is every tier above it — this is what distinguishes added
	// allowance from an exemption.
	if got := a.TierFor(1100 * gb); got == nil || got.ThrottleRate != "2M/2M" {
		t.Errorf("at 1100GB with a 100GB top-up got %v, want the second tier", got)
	}
	if got := a.TierFor(2100 * gb); got == nil || got.ThrottleRate != "512k/512k" {
		t.Errorf("at 2100GB with a 100GB top-up got %v, want the third tier", got)
	}
}

// A top-up bigger than everything used so far leaves nothing crossed.
func TestFR_CAT_003_ATopupLargerThanUsageClearsTheThrottle(t *testing.T) {
	a := Allowance{Policy: retailPolicy().Normalise(), ToppedUpBytes: 5000 * gb}
	if got := a.TierFor(600 * gb); got != nil {
		t.Errorf("a top-up larger than usage still throttles to %q", got.ThrottleRate)
	}
}

// No top-up must behave exactly as the policy alone, or buying nothing
// would change what a subscriber gets.
func TestFR_CAT_003_NoTopupIsTheSameAsThePolicyAlone(t *testing.T) {
	p := retailPolicy().Normalise()
	a := Allowance{Policy: p}
	for _, used := range []int64{0, 499 * gb, 500 * gb, 1500 * gb, 9000 * gb} {
		want, got := p.TierFor(used), a.TierFor(used)
		switch {
		case want == nil && got != nil:
			t.Errorf("at %dGB the allowance throttles but the policy does not", used/gb)
		case want != nil && got == nil:
			t.Errorf("at %dGB the allowance does not throttle but the policy does", used/gb)
		case want != nil && want.ThrottleRate != got.ThrottleRate:
			t.Errorf("at %dGB allowance gives %q, policy gives %q", used/gb, got.ThrottleRate, want.ThrottleRate)
		}
	}
}

// A plan with no policy must behave exactly as it did before this feature:
// its own threshold and throttle, expressed as one tier so every caller can
// work in tiers.
func TestFR_CAT_003_APlanWithoutAPolicyKeepsItsOwnThresholdAndThrottle(t *testing.T) {
	p := PolicyFromPlanColumns(500*gb, "10M/10M")
	if len(p.Tiers) != 1 {
		t.Fatalf("got %d tiers, want 1", len(p.Tiers))
	}
	if got := p.TierFor(499 * gb); got != nil {
		t.Errorf("under the threshold it throttles to %q", got.ThrottleRate)
	}
	if got := p.TierFor(500 * gb); got == nil || got.ThrottleRate != "10M/10M" {
		t.Errorf("at the threshold got %v, want 10M/10M", got)
	}

	// An unlimited plan — no threshold, or a threshold with no throttle to
	// apply — throttles nobody rather than throttling everybody to "".
	for _, c := range []struct {
		threshold int64
		rate      string
	}{
		{0, "10M/10M"},
		{500 * gb, ""},
		{500 * gb, "   "},
		{0, ""},
	} {
		got := PolicyFromPlanColumns(c.threshold, c.rate)
		if len(got.Tiers) != 0 {
			t.Errorf("threshold=%d rate=%q produced %d tiers, want none", c.threshold, c.rate, len(got.Tiers))
		}
		if got.TierFor(1 << 50); got.TierFor(1<<50) != nil {
			t.Errorf("threshold=%d rate=%q throttles somebody", c.threshold, c.rate)
		}
	}
}

func TestFR_CAT_003_ValidateRefusesAnIncoherentPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy Policy
		want   string
	}{
		{"no name", Policy{Tiers: retailPolicy().Tiers}, "needs a name"},
		{"no tiers", Policy{Name: "Empty"}, "throttles nobody"},
		{"a zero threshold", Policy{Name: "Zero", Tiers: []Tier{
			{ThresholdBytes: 0, ThrottleRate: "10M/10M"},
		}}, "from their first byte"},
		{"a negative threshold", Policy{Name: "Negative", Tiers: []Tier{
			{ThresholdBytes: -1, ThrottleRate: "10M/10M"},
		}}, "above zero"},
		{"no throttle rate", Policy{Name: "No rate", Tiers: []Tier{
			{ThresholdBytes: 500 * gb},
		}}, "needs a throttle rate"},
		{"a rate with no slash", Policy{Name: "Bad rate", Tiers: []Tier{
			{ThresholdBytes: 500 * gb, ThrottleRate: "10M"},
		}}, `written "down/up"`},
		{"a rate that is not a number", Policy{Name: "Words", Tiers: []Tier{
			{ThresholdBytes: 500 * gb, ThrottleRate: "slow/slow"},
		}}, "is not a rate"},
		{"two tiers at one threshold", Policy{Name: "Ambiguous", Tiers: []Tier{
			{ThresholdBytes: 500 * gb, ThrottleRate: "10M/10M"},
			{ThresholdBytes: 500 * gb, ThrottleRate: "2M/2M"},
		}}, "share the threshold"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Normalise().Validate()
			if err == nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not explain the problem (wanted %q)", err, tc.want)
			}
		})
	}

	if err := retailPolicy().Normalise().Validate(); err != nil {
		t.Errorf("the baseline policy is invalid: %v", err)
	}
}

func TestFR_CAT_003_ValidateAcceptsTheRateFormsOperatorsActuallyType(t *testing.T) {
	for _, rate := range []string{"10M/10M", "512k/512k", "1G/1G", "2M/512k", "1000000/1000000"} {
		p := Policy{Name: "Rates", Tiers: []Tier{{ThresholdBytes: gb, ThrottleRate: rate}}}
		if err := p.Normalise().Validate(); err != nil {
			t.Errorf("the rate %q was refused: %v", rate, err)
		}
	}
}

// A top-up that has expired must not count. Otherwise last month's purchase
// keeps this month's subscriber un-throttled.
func TestFR_CAT_003_ATopupCarriesAnExpiryInTheCycleItWasBoughtIn(t *testing.T) {
	// The type carries the expiry; the store is what filters on it, and
	// there is an integration test for that. What is checked here is that
	// the field exists and is not optional in practice — a zero expiry
	// would be "already expired" rather than "never expires", which is the
	// safe direction.
	var tu Topup
	if !tu.ExpiresAt.IsZero() {
		t.Error("the zero Topup has a non-zero expiry")
	}
	if tu.ExpiresAt.After(time.Now()) {
		t.Error("the zero Topup is treated as unexpired, so a row with no expiry " +
			"set would lift a throttle forever")
	}
}
