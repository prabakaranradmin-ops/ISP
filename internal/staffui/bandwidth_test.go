package staffui

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// The bandwidth profiles screen — FR-CAT-002.

type fakeBandwidth struct {
	profiles []nas.BandwidthProfile
	created  nas.BandwidthProfile
	gotID    int
	gotActiv bool
	err      error
}

func (f *fakeBandwidth) CreateBandwidthProfile(_ context.Context, in nas.BandwidthProfile) (*nas.BandwidthProfile, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.created = in
	in.ID = 1
	return &in, nil
}

func (f *fakeBandwidth) UpdateBandwidthProfile(_ context.Context, id int, in nas.BandwidthProfile) (*nas.BandwidthProfile, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.gotID, f.created = id, in
	in.ID = id
	return &in, nil
}

func (f *fakeBandwidth) SetBandwidthProfileActive(_ context.Context, id int, active bool) error {
	f.gotID, f.gotActiv = id, active
	return f.err
}

func (f *fakeBandwidth) DeleteBandwidthProfile(_ context.Context, id int) error {
	f.gotID = id
	return f.err
}

func (f *fakeBandwidth) ListBandwidthProfiles(_ context.Context, _ bool) ([]nas.BandwidthProfile, error) {
	return f.profiles, nil
}

func burstProfile() nas.BandwidthProfile {
	return nas.BandwidthProfile{
		ID: 7,
		Shaping: nas.Shaping{
			Name: "Retail burst", MaxDownBPS: 50_000_000, MaxUpBPS: 25_000_000,
			MinDownBPS: 10_000_000, MinUpBPS: 5_000_000,
			BurstDownBPS: 100_000_000, BurstUpBPS: 50_000_000,
			BurstThresholdDown: 40_000_000, BurstThresholdUp: 20_000_000,
			BurstTimeSeconds: 10, Priority: 4,
		},
		Active: true, InUseByPlans: 3,
	}
}

func TestFR_CAT_002_TheScreenShowsWhatTheRouterReceives(t *testing.T) {
	f := &fakeBandwidth{profiles: []nas.BandwidthProfile{burstProfile()}}
	h := &Handler{bandwidth: f}

	w := servePR(h, "billing_admin", "GET", "/staff/bandwidth", nil, h.Bandwidth)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"Retail burst",
		// The rendered attribute, which is the one thing an operator cannot
		// work out from the form fields.
		"50M/25M 100M/50M 40M/20M 10/10 4 10M/5M",
		// The consequence of an edit, stated before they make one.
		"re-shapes every plan on it",
		// And how many plans that is.
		"Save, re-shaping 3 plan(s)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

// A profile plans still use must not offer a delete, because the foreign key
// would refuse it anyway and the operator wants the reason, not a failure.
func TestFR_CAT_002_AProfileInUseOffersRetireRatherThanDelete(t *testing.T) {
	inUse := burstProfile()
	free := burstProfile()
	free.ID, free.Name, free.InUseByPlans = 8, "Unused", 0

	h := &Handler{bandwidth: &fakeBandwidth{profiles: []nas.BandwidthProfile{inUse, free}}}
	body := servePR(h, "isp_owner", "GET", "/staff/bandwidth", nil, h.Bandwidth).Body.String()

	if !strings.Contains(body, "/staff/bandwidth/8/delete") {
		t.Error("a profile no plan uses offers no delete")
	}
	if strings.Contains(body, "/staff/bandwidth/7/delete") {
		t.Error("a profile 3 plans use offers a delete the database would refuse")
	}
	if !strings.Contains(body, "Retire") {
		t.Error("no retire action is offered")
	}
}

// A stored profile that cannot render means every plan on it authenticates
// with no bandwidth attribute, so the screen must say so rather than show a
// blank cell.
func TestFR_CAT_002_AProfileThatCannotRenderIsFlagged(t *testing.T) {
	broken := burstProfile()
	broken.MaxDownBPS = 0 // refused by the database; reachable only if one slipped through

	h := &Handler{bandwidth: &fakeBandwidth{profiles: []nas.BandwidthProfile{broken}}}
	body := servePR(h, "isp_owner", "GET", "/staff/bandwidth", nil, h.Bandwidth).Body.String()

	if !strings.Contains(body, "cannot be sent") {
		t.Error("an unrenderable profile is not flagged on the screen")
	}
}

// Rates are typed in Mbps and stored in bits per second. A unit mistake here
// is a factor of a million, so the conversion is pinned.
func TestFR_CAT_002_TheFormConvertsMbpsToBitsPerSecond(t *testing.T) {
	f := &fakeBandwidth{}
	h := &Handler{bandwidth: f}

	form := url.Values{
		"name": {"From the form"}, "max_down": {"50"}, "max_up": {"25"},
		"min_down": {"10"}, "min_up": {"5"},
		"burst_down": {"100"}, "burst_up": {"50"},
		"burst_threshold_down": {"40"}, "burst_threshold_up": {"20"},
		"burst_time_seconds": {"10"}, "priority": {"4"},
	}
	w := servePR(h, "billing_admin", "POST", "/staff/bandwidth", form, h.CreateBandwidthForm)
	if w.Code != 303 {
		t.Fatalf("got %d, want a redirect: %s", w.Code, w.Body.String())
	}

	got := f.created
	for _, c := range []struct {
		field    string
		got, exp int64
	}{
		{"max down", got.MaxDownBPS, 50_000_000},
		{"max up", got.MaxUpBPS, 25_000_000},
		{"min down", got.MinDownBPS, 10_000_000},
		{"burst down", got.BurstDownBPS, 100_000_000},
		{"burst threshold down", got.BurstThresholdDown, 40_000_000},
	} {
		if c.got != c.exp {
			t.Errorf("%s: stored %d bps, want %d", c.field, c.got, c.exp)
		}
	}
	if got.BurstTimeSeconds != 10 {
		t.Errorf("burst time: %d, want 10", got.BurstTimeSeconds)
	}
	if got.Priority != 4 {
		t.Errorf("priority: %d, want 4", got.Priority)
	}

	// A fractional Mbps must survive rather than truncate: 1.5 Mbps is a
	// real retail upload rate.
	f2 := &fakeBandwidth{}
	h2 := &Handler{bandwidth: f2}
	servePR(h2, "billing_admin", "POST", "/staff/bandwidth",
		url.Values{"name": {"Fractional"}, "max_down": {"1.5"}, "max_up": {"1.5"}},
		h2.CreateBandwidthForm)
	if f2.created.MaxDownBPS != 1_500_000 {
		t.Errorf("1.5 Mbps stored as %d bps, want 1500000", f2.created.MaxDownBPS)
	}
}

// An incoherent profile is refused with the sentence explaining why, not a
// generic failure and not a 500.
func TestFR_CAT_002_AnIncoherentProfileIsRefusedWithAReason(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"a burst slower than the ceiling", url.Values{
			"name": {"bad"}, "max_down": {"50"}, "max_up": {"50"},
			"burst_down": {"20"}, "burst_threshold_down": {"10"}, "burst_time_seconds": {"10"},
		}, "not a burst"},
		{"a floor above the ceiling", url.Values{
			"name": {"bad"}, "max_down": {"50"}, "max_up": {"50"}, "min_down": {"60"},
		}, "cannot exceed"},
		{"a priority MikroTik does not accept", url.Values{
			"name": {"bad"}, "max_down": {"50"}, "max_up": {"50"}, "priority": {"9"},
		}, "between 1"},
		{"no name", url.Values{
			"name": {"  "}, "max_down": {"50"}, "max_up": {"50"},
		}, "needs a name"},
		{"a rate that is not a number", url.Values{
			"name": {"bad"}, "max_down": {"fast"}, "max_up": {"50"},
		}, "not a number"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBandwidth{}
			h := &Handler{bandwidth: f}
			w := servePR(h, "billing_admin", "POST", "/staff/bandwidth", tc.form, h.CreateBandwidthForm)

			if w.Code != 303 {
				t.Fatalf("got %d, want a redirect carrying the reason", w.Code)
			}
			loc := w.Header().Get("Location")
			decoded, err := url.QueryUnescape(loc)
			if err != nil {
				t.Fatalf("undecodable redirect %q: %v", loc, err)
			}
			if !strings.Contains(decoded, tc.want) {
				t.Errorf("the operator is told %q, which does not explain the problem (wanted %q)", decoded, tc.want)
			}
			if f.created.Name != "" {
				t.Error("an invalid profile reached the store")
			}
		})
	}
}

func TestFR_CAT_002_RetireAndRestoreReachTheStore(t *testing.T) {
	f := &fakeBandwidth{}
	h := &Handler{bandwidth: f}

	servePR(h, "billing_admin", "POST", "/staff/bandwidth/7/active",
		url.Values{"active": {"false"}}, h.SetBandwidthActiveForm)
	if f.gotID != 7 || f.gotActiv {
		t.Errorf("retire reached the store as id=%d active=%v", f.gotID, f.gotActiv)
	}

	servePR(h, "billing_admin", "POST", "/staff/bandwidth/7/active",
		url.Values{"active": {"true"}}, h.SetBandwidthActiveForm)
	if !f.gotActiv {
		t.Error("restore did not reach the store")
	}
}

// Same reach as Catalogue, and withheld from everyone else.
func TestFR_CAT_002_BandwidthProfilesReach(t *testing.T) {
	has := func(role string) bool {
		for _, sec := range AllowedSections(role, true) {
			if sec.Key == "bandwidth" {
				return true
			}
		}
		return false
	}
	for _, role := range []string{"isp_owner", "billing_admin"} {
		if !has(role) {
			t.Errorf("%s cannot reach bandwidth profiles", role)
		}
	}
	for _, role := range []string{"csr", "noc_engineer", "technician"} {
		if has(role) {
			t.Errorf("%s can reach bandwidth profiles", role)
		}
	}
}

func TestFR_CAT_002_AnUnconfiguredStoreSaysSoRatherThanFailing(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/bandwidth", nil, h.Bandwidth)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}
