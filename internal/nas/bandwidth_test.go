package nas

import (
	"strings"
	"testing"
)

// Reusable bandwidth profiles — FR-CAT-002.

func TestFR_CAT_002_APlainProfileRendersExactlyWhatPlansAlreadyHeld(t *testing.T) {
	// The whole backwards-compatibility claim rests on this: adopting a
	// profile that only sets a ceiling must produce byte-for-byte what
	// plans.rate_limit_string already holds, so no live session changes
	// shape. If this fails, migrating a plan to a profile silently
	// re-shapes every subscriber on it.
	s := Shaping{Name: "Retail 50", MaxDownBPS: 50_000_000, MaxUpBPS: 50_000_000}
	got, err := s.MikrotikRateLimit()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "50M/50M" {
		t.Errorf("a ceiling-only profile rendered %q, want %q", got, "50M/50M")
	}
}

func TestFR_CAT_002_BurstRendersEveryPositionalField(t *testing.T) {
	s := Shaping{
		Name:               "Retail burst",
		MaxDownBPS:         50_000_000,
		MaxUpBPS:           25_000_000,
		BurstDownBPS:       100_000_000,
		BurstUpBPS:         50_000_000,
		BurstThresholdDown: 40_000_000,
		BurstThresholdUp:   20_000_000,
		BurstTimeSeconds:   10,
		Priority:           4,
		MinDownBPS:         10_000_000,
		MinUpBPS:           5_000_000,
	}
	got, err := s.MikrotikRateLimit()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "50M/25M 100M/50M 40M/20M 10/10 4 10M/5M"
	if got != want {
		t.Errorf("rendered\n got %q\nwant %q", got, want)
	}

	// The fields are positional, so the count is part of the contract: six
	// space-separated groups when everything is set. A missing group
	// shifts priority into the burst-time slot, which a NAS accepts and
	// silently misapplies.
	if n := len(strings.Fields(got)); n != 6 {
		t.Errorf("rendered %d positional groups, want 6: %q", n, got)
	}
}

// A floor or a priority with no burst still has to emit the burst group,
// because the fields are positional and a gap cannot be skipped.
func TestFR_CAT_002_AFloorWithoutBurstStillEmitsTheBurstPositions(t *testing.T) {
	s := Shaping{
		Name: "Business committed", MaxDownBPS: 100_000_000, MaxUpBPS: 100_000_000,
		MinDownBPS: 50_000_000, MinUpBPS: 50_000_000, Priority: 1,
	}
	got, err := s.MikrotikRateLimit()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	want := "100M/100M 0/0 0/0 0/0 1 50M/50M"
	if got != want {
		t.Errorf("rendered\n got %q\nwant %q", got, want)
	}
}

// Priority 8 is the column default and MikroTik's own default, so it is not
// worth lengthening every attribute for. Priority 0 is not a value MikroTik
// accepts and must never reach the wire.
func TestFR_CAT_002_DefaultPriorityIsNotEmittedAndZeroNeverIs(t *testing.T) {
	plain := Shaping{Name: "d", MaxDownBPS: 10_000_000, MaxUpBPS: 10_000_000, Priority: 8}
	got, err := plain.MikrotikRateLimit()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if got != "10M/10M" {
		t.Errorf("priority 8 lengthened the attribute: %q", got)
	}

	withFloor := Shaping{
		Name: "z", MaxDownBPS: 10_000_000, MaxUpBPS: 10_000_000,
		MinDownBPS: 1_000_000, MinUpBPS: 1_000_000, Priority: 0,
	}
	got, err = withFloor.MikrotikRateLimit()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The priority field is the fifth group.
	fields := strings.Fields(got)
	if len(fields) != 6 {
		t.Fatalf("got %d groups: %q", len(fields), got)
	}
	if fields[4] == "0" {
		t.Errorf("priority 0 reached the wire: %q", got)
	}
	if fields[4] != "8" {
		t.Errorf("priority rendered as %q, want the default 8", fields[4])
	}
}

// Rates that do not divide exactly must not be rounded into a suffix: an
// operator who entered 1,500,000 bps is not selling 1M or 2M.
func TestFR_CAT_002_InexactRatesAreNotRounded(t *testing.T) {
	for _, tc := range []struct {
		bps  int64
		want string
	}{
		{0, "0"},
		{512_000, "512k"},
		{1_000_000, "1M"},
		{1_500_000, "1500k"},
		{1_500_001, "1500001"},
		{2_000_000_000, "2G"},
	} {
		if got := rateToken(tc.bps); got != tc.want {
			t.Errorf("rateToken(%d) = %q, want %q", tc.bps, got, tc.want)
		}
	}
}

func TestFR_CAT_002_AProfileWithNoCeilingIsRefused(t *testing.T) {
	// Shaping with a zero ceiling would render "0/0", which MikroTik reads
	// as unlimited — the opposite of what a zero suggests. The database
	// refuses it too (CHECK max_down_bps > 0); this is the second gate, for
	// a profile assembled in Go without going through the table.
	for _, s := range []Shaping{
		{Name: "no down", MaxUpBPS: 1_000_000},
		{Name: "no up", MaxDownBPS: 1_000_000},
		{Name: "neither"},
	} {
		if _, err := s.MikrotikRateLimit(); err == nil {
			t.Errorf("%s: a profile with no ceiling rendered without error", s.Name)
		}
	}
}

// The trap this design exists to avoid. The Huawei and ZTE builders turn the
// rate-limit string into integer VSAs via parseRateLimitString, which splits
// on the first "/" and parses both halves as rate tokens. Handing them the
// full burst form breaks that parse — so a plan that merely gained a burst
// would stop authenticating on every Huawei and ZTE NAS in the estate.
//
// This test pins both halves of the rule: the burst form really is
// unparseable by that function, and PlainRateLimit really is parseable.
func TestFR_CAT_002_OnlyTheMikrotikDialectGetsTheBurstForm(t *testing.T) {
	s := Shaping{
		Name: "Retail burst", MaxDownBPS: 50_000_000, MaxUpBPS: 50_000_000,
		BurstDownBPS: 100_000_000, BurstUpBPS: 100_000_000,
		BurstThresholdDown: 40_000_000, BurstThresholdUp: 40_000_000,
		BurstTimeSeconds: 10, Priority: 4,
	}

	burst, err := s.MikrotikRateLimit()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, _, err := parseRateLimitString(burst); err == nil {
		t.Error("the burst form parsed as a plain rate limit — if this ever " +
			"succeeds, check it yields the ceiling and not a wrong number")
	}

	plain := s.PlainRateLimit()
	down, up, err := parseRateLimitString(plain)
	if err != nil {
		t.Fatalf("PlainRateLimit() = %q, which the integer-VSA vendors cannot parse: %v", plain, err)
	}
	if down != 50_000_000 || up != 50_000_000 {
		t.Errorf("PlainRateLimit() parsed to down=%d up=%d, want 50M/50M", down, up)
	}

	// And the plain form must carry no burst information at all, rather
	// than a truncated version of it.
	if strings.Contains(plain, " ") {
		t.Errorf("PlainRateLimit() = %q contains a positional group", plain)
	}
}

// The wiring, at the level a caller actually uses: one RateProfile carrying
// a burst profile, handed to every vendor. MikroTik must receive the burst;
// the integer-VSA vendors must still build successfully, which they can only
// do if they were given the ceiling alone.
func TestFR_CAT_002_EveryVendorStillBuildsFromABurstProfile(t *testing.T) {
	p := RateProfile{
		// Set too, so the test also proves the profile wins over it rather
		// than the fallback being used by accident.
		RateLimitString: "1M/1M",
		ProfileName:     "PLAN_50M",
		Shaping: &Shaping{
			Name: "Retail burst", MaxDownBPS: 50_000_000, MaxUpBPS: 50_000_000,
			BurstDownBPS: 100_000_000, BurstUpBPS: 100_000_000,
			BurstThresholdDown: 40_000_000, BurstThresholdUp: 40_000_000,
			BurstTimeSeconds: 10, Priority: 4,
		},
	}

	for _, vendor := range Vendors() {
		v := Vendor(vendor)
		if _, err := BuildAcceptAttrs(v, p); err != nil {
			t.Errorf("%s: BuildAccept with a burst profile failed: %v", vendor, err)
		}
		if _, err := BuildCoAAttrs(v, p); err != nil {
			t.Errorf("%s: BuildCoA with a burst profile failed: %v", vendor, err)
		}
	}

	// And MikroTik must actually carry the burst, not quietly fall back to
	// the ceiling: a build that succeeds while sending the wrong speed is
	// the exact failure this package was created to stop.
	attrs, err := mikrotikBuilder{}.BuildAccept(p)
	if err != nil {
		t.Fatalf("mikrotik: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("mikrotik built %d attributes, want 1", len(attrs))
	}
	// The VSA payload is vendor-type, length, then the string.
	raw := string(attrs[0].Value)
	if !strings.Contains(raw, "100M/100M") {
		t.Errorf("the MikroTik attribute carries no burst rate: %q", raw)
	}
	if !strings.Contains(raw, "50M/50M") {
		t.Errorf("the MikroTik attribute carries no ceiling: %q", raw)
	}
	if strings.Contains(raw, "1M/1M") {
		t.Errorf("the MikroTik attribute fell back to rate_limit_string: %q", raw)
	}
}

// A plan with no profile must behave exactly as it did before this feature
// existed. This is the assertion that makes nil a supported state rather
// than a migration window.
func TestFR_CAT_002_APlanWithNoProfileIsUnchanged(t *testing.T) {
	p := RateProfile{RateLimitString: "20M/10M", ProfileName: "PLAN_20M"}

	if got, err := p.mikrotikRateLimit(); err != nil || got != "20M/10M" {
		t.Errorf("mikrotikRateLimit() = %q, %v; want %q, nil", got, err, "20M/10M")
	}
	if got := p.plainRateLimit(); got != "20M/10M" {
		t.Errorf("plainRateLimit() = %q, want %q", got, "20M/10M")
	}
	for _, vendor := range Vendors() {
		if _, err := BuildAcceptAttrs(Vendor(vendor), p); err != nil {
			t.Errorf("%s: a profile-less plan failed to build: %v", vendor, err)
		}
	}
}

// Validate restates the table's CHECK constraints. Each case here is a
// constraint that exists in migration 065, so a profile the console accepts
// can never be one the database then refuses — which would surface as a 500
// on a form the operator filled in correctly as far as they could tell.
func TestFR_CAT_002_ValidateRefusesWhatTheDatabaseWould(t *testing.T) {
	ok := func() BandwidthProfile {
		return BandwidthProfile{Shaping: Shaping{
			Name: "Retail", MaxDownBPS: 50_000_000, MaxUpBPS: 50_000_000, Priority: 8,
		}}
	}
	if err := ok().Validate(); err != nil {
		t.Fatalf("the baseline profile is invalid: %v", err)
	}

	cases := []struct {
		name   string
		break_ func(*BandwidthProfile)
		// The phrase an operator should see. Asserted so the message stays
		// an explanation rather than decaying into "invalid input".
		want string
	}{
		{"no name", func(p *BandwidthProfile) { p.Name = "   " }, "needs a name"},
		{"zero ceiling", func(p *BandwidthProfile) { p.MaxDownBPS = 0 }, "above zero"},
		{"floor above ceiling", func(p *BandwidthProfile) { p.MinDownBPS = 60_000_000 }, "cannot exceed"},
		{"priority out of range", func(p *BandwidthProfile) { p.Priority = 9 }, "between 1"},
		{"burst slower than the ceiling", func(p *BandwidthProfile) {
			p.BurstDownBPS, p.BurstThresholdDown, p.BurstTimeSeconds = 20_000_000, 10_000_000, 10
		}, "not a burst"},
		{"threshold that can never be crossed", func(p *BandwidthProfile) {
			p.BurstDownBPS, p.BurstThresholdDown, p.BurstTimeSeconds = 100_000_000, 60_000_000, 10
		}, "never engage"},
		{"burst with no duration", func(p *BandwidthProfile) {
			p.BurstDownBPS, p.BurstThresholdDown, p.BurstTimeSeconds = 100_000_000, 40_000_000, 0
		}, "never end"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := ok()
			tc.break_(&p)
			p = p.Normalise()
			err := p.Validate()
			if err == nil {
				t.Fatalf("accepted a profile the database would refuse")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("message %q does not explain the problem (wanted %q)", err, tc.want)
			}
		})
	}
}

// Normalise settles the defaults so that what Validate sees is what gets
// stored, and clears a half-set burst rather than storing something the
// database would refuse.
func TestFR_CAT_002_NormaliseClearsAHalfSetBurst(t *testing.T) {
	p := BandwidthProfile{Shaping: Shaping{
		Name: "  Retail  ", MaxDownBPS: 50_000_000, MaxUpBPS: 50_000_000,
		// A rate with no threshold: would burst forever.
		BurstDownBPS: 100_000_000, BurstTimeSeconds: 10,
	}}.Normalise()

	if p.Name != "Retail" {
		t.Errorf("name not trimmed: %q", p.Name)
	}
	if p.Priority != 8 {
		t.Errorf("priority not defaulted: %d", p.Priority)
	}
	if p.BurstDownBPS != 0 {
		t.Errorf("a burst rate with no threshold survived normalisation: %d", p.BurstDownBPS)
	}
	if p.BurstTimeSeconds != 0 {
		t.Errorf("burst time survived with no burst to apply it to: %d", p.BurstTimeSeconds)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("the normalised profile is invalid: %v", err)
	}
}

func TestFR_CAT_002_HasBurstAndHasFloorDescribeThePartialCases(t *testing.T) {
	// Burst in one direction only is legitimate: operators commonly burst
	// the download and leave the upload flat.
	downOnly := Shaping{
		MaxDownBPS: 10_000_000, MaxUpBPS: 10_000_000,
		BurstDownBPS: 20_000_000, BurstThresholdDown: 8_000_000, BurstTimeSeconds: 5,
	}
	if !downOnly.HasBurst() {
		t.Error("a download-only burst is not reported as a burst")
	}
	if downOnly.HasFloor() {
		t.Error("a profile with no min rate reports a floor")
	}

	none := Shaping{MaxDownBPS: 1, MaxUpBPS: 1}
	if none.HasBurst() || none.HasFloor() {
		t.Error("a ceiling-only profile reports burst or floor")
	}
}
