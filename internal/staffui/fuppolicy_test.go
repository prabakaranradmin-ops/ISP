package staffui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
)

// The fair-use policies screen — FR-CAT-003.

const testGB = int64(1) << 30

type fakeFUPPolicies struct {
	policies []fup.Policy
	saved    fup.Policy
	gotID    int
	gotActiv bool
	deleted  int
	planID   int
	policyID *int
	attached bool
	err      error
}

func (f *fakeFUPPolicies) SaveFUPPolicy(_ context.Context, p fup.Policy) (*fup.Policy, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.saved = p
	p.ID = 1
	return &p, nil
}

func (f *fakeFUPPolicies) SetFUPPolicyActive(_ context.Context, id int, active bool) error {
	f.gotID, f.gotActiv = id, active
	return f.err
}

func (f *fakeFUPPolicies) DeleteFUPPolicy(_ context.Context, id int) error {
	f.deleted = id
	return f.err
}

func (f *fakeFUPPolicies) ListFUPPolicies(_ context.Context, _ bool) ([]fup.Policy, error) {
	return f.policies, nil
}

func (f *fakeFUPPolicies) SetPlanFUPPolicy(_ context.Context, planID int, policyID *int) error {
	f.planID, f.policyID, f.attached = planID, policyID, true
	return f.err
}

func threeTier() fup.Policy {
	return fup.Policy{
		ID: 7, Name: "Retail fair use", Active: true, InUseByPlans: 3,
		Tiers: []fup.Tier{
			{ThresholdBytes: 500 * testGB, ThrottleRate: "10M/10M"},
			{ThresholdBytes: 1000 * testGB, ThrottleRate: "2M/2M"},
			{ThresholdBytes: 2000 * testGB, ThrottleRate: "512k/512k"},
		},
	}
}

func fairUseScreen(policies ...fup.Policy) (*Handler, *fakeFUPPolicies) {
	f := &fakeFUPPolicies{policies: policies}
	cat := newFakeCatalogue()
	cat.plans = []Plan{{ID: 1, Name: "Fibre 100"}}
	return &Handler{fupPolicies: f, catalogue: cat}, f
}

func TestFR_CAT_003_TheScreenShowsEveryStepAndWhereThrottlingStarts(t *testing.T) {
	h, _ := fairUseScreen(threeTier())

	w := servePR(h, "billing_admin", "GET", "/staff/fair-use", nil, h.FairUse)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"Retail fair use",
		// Every step, in the unit an operator sells in.
		"500", "10M/10M", "1000", "2M/2M", "2000", "512k/512k",
		// The consequence of an edit, before they make one.
		"re-shapes every plan on it",
		"Save, re-shaping 3 plan(s)",
		// The first threshold is what the 80% warning measures against,
		// which is not obvious and is easy to get wrong. Matched on a
		// fragment that does not span the template's line break.
		"warning is measured against",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

// A policy plans still use must not offer a delete: the foreign key would
// refuse it and the operator wants the reason, not a failure.
func TestFR_CAT_003_APolicyInUseOffersRetireRatherThanDelete(t *testing.T) {
	inUse := threeTier()
	free := threeTier()
	free.ID, free.Name, free.InUseByPlans = 8, "Unused", 0

	h, _ := fairUseScreen(inUse, free)
	body := servePR(h, "isp_owner", "GET", "/staff/fair-use", nil, h.FairUse).Body.String()

	if !strings.Contains(body, "/staff/fair-use/8/delete") {
		t.Error("a policy no plan uses offers no delete")
	}
	if strings.Contains(body, "/staff/fair-use/7/delete") {
		t.Error("a policy 3 plans use offers a delete the database would refuse")
	}
	if !strings.Contains(body, "Retire") {
		t.Error("no retire action is offered")
	}
}

// A policy with no steps throttles nobody. The screen must say so rather
// than render a blank cell that looks like a display bug.
func TestFR_CAT_003_APolicyWithNoStepsSaysItThrottlesNobody(t *testing.T) {
	empty := fup.Policy{ID: 9, Name: "Empty", Active: true}
	h, _ := fairUseScreen(empty)
	body := servePR(h, "isp_owner", "GET", "/staff/fair-use", nil, h.FairUse).Body.String()
	if !strings.Contains(body, "throttles nobody") {
		t.Error("a policy with no steps is not flagged")
	}
}

// Thresholds are typed in GB and stored in bytes. A unit mistake here is a
// factor of a billion, so the conversion is pinned in both directions.
func TestFR_CAT_003_TheFormConvertsGBToBytes(t *testing.T) {
	h, f := fairUseScreen()

	form := url.Values{
		"name":          {"From the form"},
		"threshold_gb":  {"500", "1000", "", "2000"},
		"throttle_rate": {"10M/10M", "2M/2M", "", "512k/512k"},
	}
	w := servePR(h, "billing_admin", "POST", "/staff/fair-use", form, h.SaveFUPPolicyForm)
	if w.Code != 303 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}

	got := f.saved
	if len(got.Tiers) != 3 {
		t.Fatalf("stored %d tiers, want 3 — the blank row should be skipped, not stored", len(got.Tiers))
	}
	wants := []struct {
		bytes int64
		rate  string
	}{
		{500 * testGB, "10M/10M"},
		{1000 * testGB, "2M/2M"},
		{2000 * testGB, "512k/512k"},
	}
	for i, want := range wants {
		if got.Tiers[i].ThresholdBytes != want.bytes {
			t.Errorf("tier %d stored %d bytes, want %d", i+1, got.Tiers[i].ThresholdBytes, want.bytes)
		}
		if got.Tiers[i].ThrottleRate != want.rate {
			t.Errorf("tier %d stored rate %q, want %q", i+1, got.Tiers[i].ThrottleRate, want.rate)
		}
	}

	// And back: what the screen shows must be typeable straight back in.
	if got := gbLabel(500 * testGB); got != "500" {
		t.Errorf("gbLabel(500GB) = %q, want %q", got, "500")
	}
	if got := gbLabel(0); got != "" {
		t.Errorf("gbLabel(0) = %q, want empty", got)
	}
	// A fractional quota survives rather than rounding to a different size.
	if got := gbLabel(testGB + testGB/2); got != "1.50" {
		t.Errorf("gbLabel(1.5GB) = %q, want %q", got, "1.50")
	}
}

// An incoherent policy is refused with the sentence naming which step is
// wrong, not a generic failure and not a 500.
func TestFR_CAT_003_AnIncoherentPolicyIsRefusedWithAReason(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"no name", url.Values{
			"name": {"  "}, "threshold_gb": {"500"}, "throttle_rate": {"10M/10M"},
		}, "needs a name"},
		{"no steps at all", url.Values{
			"name": {"Empty"}, "threshold_gb": {""}, "throttle_rate": {""},
		}, "throttles nobody"},
		{"a threshold that is not a number", url.Values{
			"name": {"Bad"}, "threshold_gb": {"lots"}, "throttle_rate": {"10M/10M"},
		}, "not a number of GB"},
		{"a zero threshold", url.Values{
			"name": {"Zero"}, "threshold_gb": {"0"}, "throttle_rate": {"10M/10M"},
		}, "above zero"},
		{"a step with no rate", url.Values{
			"name": {"No rate"}, "threshold_gb": {"500"}, "throttle_rate": {""},
		}, "needs a throttle rate"},
		{"a rate with no slash", url.Values{
			"name": {"Bad rate"}, "threshold_gb": {"500"}, "throttle_rate": {"10M"},
		}, `written "down/up"`},
		{"two steps at one threshold", url.Values{
			"name": {"Ambiguous"}, "threshold_gb": {"500", "500"}, "throttle_rate": {"10M/10M", "2M/2M"},
		}, "share the threshold"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, f := fairUseScreen()
			w := servePR(h, "billing_admin", "POST", "/staff/fair-use", tc.form, h.SaveFUPPolicyForm)
			if w.Code != 303 {
				t.Fatalf("got %d, want a redirect carrying the reason", w.Code)
			}
			decoded, err := url.QueryUnescape(w.Header().Get("Location"))
			if err != nil {
				t.Fatalf("undecodable redirect: %v", err)
			}
			if !strings.Contains(decoded, tc.want) {
				t.Errorf("the operator is told %q, which does not explain the problem (wanted %q)",
					decoded, tc.want)
			}
			if f.saved.Name != "" {
				t.Error("the invalid policy reached the store")
			}
		})
	}
}

func TestFR_CAT_003_AttachingAndDetachingAPolicyReachesTheStore(t *testing.T) {
	h, f := fairUseScreen(threeTier())

	servePR(h, "billing_admin", "POST", "/staff/fair-use/attach",
		url.Values{"plan_id": {"1"}, "policy_id": {"7"}}, h.AttachFUPPolicyForm)
	if f.planID != 1 || f.policyID == nil || *f.policyID != 7 {
		t.Errorf("attach reached the store as plan=%d policy=%v", f.planID, f.policyID)
	}

	// An empty selection detaches, sending the plan back to its own
	// threshold — a supported setup, so it must be expressible.
	f.policyID, f.attached = nil, false
	servePR(h, "billing_admin", "POST", "/staff/fair-use/attach",
		url.Values{"plan_id": {"1"}, "policy_id": {""}}, h.AttachFUPPolicyForm)
	if !f.attached {
		t.Fatal("detach did not reach the store")
	}
	if f.policyID != nil {
		t.Errorf("detach passed policy %v, want nil", f.policyID)
	}
}

func TestFR_CAT_003_RetireAndRestoreReachTheStore(t *testing.T) {
	h, f := fairUseScreen(threeTier())

	servePR(h, "billing_admin", "POST", "/staff/fair-use/7/active",
		url.Values{"active": {"false"}}, h.SetFUPPolicyActiveForm)
	if f.gotID != 7 || f.gotActiv {
		t.Errorf("retire reached the store as id=%d active=%v", f.gotID, f.gotActiv)
	}

	servePR(h, "billing_admin", "POST", "/staff/fair-use/7/active",
		url.Values{"active": {"true"}}, h.SetFUPPolicyActiveForm)
	if !f.gotActiv {
		t.Error("restore did not reach the store")
	}
}

// Same reach as the catalogue and bandwidth profiles, and withheld from the
// network and front-desk roles.
func TestFR_CAT_003_FairUseReach(t *testing.T) {
	has := func(role string) bool {
		for _, sec := range AllowedSections(role, true) {
			if sec.Key == "fair-use" {
				return true
			}
		}
		return false
	}
	for _, role := range []string{"isp_owner", "billing_admin"} {
		if !has(role) {
			t.Errorf("%s cannot reach fair-use policies", role)
		}
	}
	for _, role := range []string{"csr", "noc_engineer", "technician"} {
		if has(role) {
			t.Errorf("%s can reach fair-use policies", role)
		}
	}
}

// Deleting a policy reports what happened, and the policy and plan ids are
// checked before anything reaches the store.
func TestFR_CAT_003_DeleteAndRefusals(t *testing.T) {
	f := &fakeFUPPolicies{}
	h := &Handler{fupPolicies: f}
	w := servePR(h, "isp_owner", "POST", "/staff/fair-use/4/delete", url.Values{}, h.DeleteFUPPolicyForm)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "m=deleted") || f.deleted != 4 {
		t.Errorf("delete: %q id=%d", loc, f.deleted)
	}

	f = &fakeFUPPolicies{err: fup.ErrPolicyInUse}
	h = &Handler{fupPolicies: f}
	w = servePR(h, "isp_owner", "POST", "/staff/fair-use/4/delete", url.Values{}, h.DeleteFUPPolicyForm)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "m=inuse") {
		t.Errorf("delete in use: %q", loc)
	}
	f = &fakeFUPPolicies{err: fup.ErrPolicyNameTaken}
	h = &Handler{fupPolicies: f}
	w = servePR(h, "isp_owner", "POST", "/staff/fair-use/4/active", url.Values{"active": {"true"}}, h.SetFUPPolicyActiveForm)
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "m=duplicate") {
		t.Errorf("restoring onto a taken name: %q", loc)
	}

	for _, tc := range []struct {
		target string
		form   url.Values
		fn     func(*Handler) func(http.ResponseWriter, *http.Request)
	}{
		{"/staff/fair-use/x/delete", url.Values{}, func(h *Handler) func(http.ResponseWriter, *http.Request) { return h.DeleteFUPPolicyForm }},
		{"/staff/fair-use/x/active", url.Values{}, func(h *Handler) func(http.ResponseWriter, *http.Request) { return h.SetFUPPolicyActiveForm }},
		{"/staff/fair-use/attach", url.Values{"plan_id": {"basic"}}, func(h *Handler) func(http.ResponseWriter, *http.Request) { return h.AttachFUPPolicyForm }},
	} {
		f := &fakeFUPPolicies{}
		h := &Handler{fupPolicies: f}
		if w := servePR(h, "isp_owner", "POST", tc.target, tc.form, tc.fn(h)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", tc.target, w.Code)
		}
		if f.deleted != 0 || f.attached || f.gotID != 0 {
			t.Errorf("%s reached the store", tc.target)
		}
	}

	// A garbled policy id on attach is refused, not read as "detach".
	f = &fakeFUPPolicies{}
	h = &Handler{fupPolicies: f}
	w = servePR(h, "isp_owner", "POST", "/staff/fair-use/attach", url.Values{"plan_id": {"3"}, "policy_id": {"tiered"}}, h.AttachFUPPolicyForm)
	if f.attached {
		t.Errorf("a garbled policy id reached the store as %v", f.policyID)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "m=failed") {
		t.Errorf("garbled policy id: %q", loc)
	}

	// A CSR cannot change fair-use policies.
	h = &Handler{fupPolicies: &fakeFUPPolicies{}}
	if w := servePR(h, "csr", "POST", "/staff/fair-use/4/delete", url.Values{}, h.DeleteFUPPolicyForm); w.Code != http.StatusForbidden {
		t.Errorf("csr delete: %d", w.Code)
	}
}

func TestFR_CAT_003_AnUnconfiguredStoreSaysSoRatherThanFailing(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/fair-use", nil, h.FairUse)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}
