package staffui

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// The per-plan RADIUS attributes screen — FR-CAT-004.

type fakeReplyAttrs struct {
	attrs    []nas.ReplyAttribute
	set      nas.ReplyAttribute
	gotID    int
	gotEnabl bool
	deleted  int
	err      error
}

func (f *fakeReplyAttrs) SetPlanReplyAttribute(_ context.Context, in nas.ReplyAttribute) (*nas.ReplyAttribute, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.set = in
	in.ID = 1
	return &in, nil
}

func (f *fakeReplyAttrs) DeletePlanReplyAttribute(_ context.Context, id int) error {
	f.deleted = id
	return f.err
}

func (f *fakeReplyAttrs) SetPlanReplyAttributeEnabled(_ context.Context, id int, enabled bool) error {
	f.gotID, f.gotEnabl = id, enabled
	return f.err
}

func (f *fakeReplyAttrs) ListPlanReplyAttributes(_ context.Context, _ int) ([]nas.ReplyAttribute, error) {
	return f.attrs, f.err
}

// attrScreen wires the screen with one plan. The plan dropdown is what the
// whole screen hangs off, and with no plans at all it correctly shows "no
// plans yet" instead of a table — true, but not what these tests are about.
func attrScreen(attrs ...nas.ReplyAttribute) (*Handler, *fakeReplyAttrs) {
	f := &fakeReplyAttrs{attrs: attrs}
	cat := newFakeCatalogue()
	cat.plans = []Plan{{ID: 7, Name: "Fibre 100"}}
	h := &Handler{replyAttrs: f, catalogue: cat}
	return h, f
}

func TestFR_CAT_004_TheScreenShowsAPlansAttributesWithTheirMeaning(t *testing.T) {
	h, _ := attrScreen(
		nas.ReplyAttribute{ID: 1, PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Mikrotik-Address-List", Value: "retail", Enabled: true},
		nas.ReplyAttribute{ID: 2, PlanID: 7, Vendor: nas.VendorMikrotik,
			Name: "Session-Timeout", Value: "3600", Enabled: false},
	)

	w := servePR(h, "noc_engineer", "GET", "/staff/plan-attributes?plan_id=7", nil, h.PlanAttributes)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		"Mikrotik-Address-List", "retail", "Session-Timeout", "3600",
		// The dictionary's explanation travels with the attribute, so an
		// operator is not reading a bare name.
		"address list",
		// The claim that justifies validating at all.
		"silently ignores an attribute it does not understand",
		// A switched-off attribute says it is kept rather than gone.
		"Kept, but not sent",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
}

// A stored attribute the dictionary no longer defines is skipped by the
// resolver, so the screen has to say so. Otherwise the row looks configured
// and does nothing — the failure the whole feature exists to prevent.
func TestFR_CAT_004_AnAttributeTheDictionaryDroppedIsFlagged(t *testing.T) {
	h, _ := attrScreen(nas.ReplyAttribute{
		ID: 1, PlanID: 7, Vendor: nas.VendorMikrotik,
		Name: "Mikrotik-Removed-Attribute", Value: "x", Enabled: true,
	})

	body := servePR(h, "isp_owner", "GET", "/staff/plan-attributes?plan_id=7", nil, h.PlanAttributes).Body.String()
	if !strings.Contains(body, "not sent") {
		t.Error("an attribute the dictionary no longer defines is not flagged")
	}
	if !strings.Contains(body, "no longer knows how to send") {
		t.Error("the page does not explain why it is not sent")
	}
}

// The add form must not offer an attribute the bandwidth builder sends, but
// the reference table must still list it with the reason — an operator who
// cannot see why it is missing assumes the dictionary is incomplete.
func TestFR_CAT_004_ReservedAttributesAreExplainedButNotOffered(t *testing.T) {
	h, _ := attrScreen()

	body := servePR(h, "isp_owner", "GET", "/staff/plan-attributes?plan_id=7&vendor=mikrotik",
		nil, h.PlanAttributes).Body.String()

	// Listed with its reason.
	if !strings.Contains(body, "Mikrotik-Rate-Limit") {
		t.Error("the reference table omits the reserved attribute entirely")
	}
	if !strings.Contains(body, "Not available here") {
		t.Error("the reserved attribute carries no explanation")
	}
	// But not selectable.
	if strings.Contains(body, `<option value="Mikrotik-Rate-Limit"`) {
		t.Error("the add form offers Mikrotik-Rate-Limit, which would compete with the plan's own shaping")
	}
	// While a legitimate one is selectable.
	if !strings.Contains(body, `<option value="Mikrotik-Address-List"`) {
		t.Error("the add form does not offer Mikrotik-Address-List")
	}
}

func TestFR_CAT_004_AValidAttributeReachesTheStoreNormalised(t *testing.T) {
	h, f := attrScreen()

	form := url.Values{
		"plan_id": {"7"}, "vendor": {"MikroTik"},
		"attribute": {"mikrotik-address-list"}, "value": {"  retail  "},
	}
	w := servePR(h, "noc_engineer", "POST", "/staff/plan-attributes", form, h.SetPlanAttributeForm)
	if w.Code != 303 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}

	if f.set.PlanID != 7 {
		t.Errorf("plan id reached the store as %d", f.set.PlanID)
	}
	if f.set.Vendor != nas.VendorMikrotik {
		t.Errorf("vendor reached the store as %q, want it lower-cased", f.set.Vendor)
	}
	// Canonicalised, or two spellings of one attribute both fit the unique
	// index and the router receives it twice.
	if f.set.Name != "Mikrotik-Address-List" {
		t.Errorf("attribute reached the store as %q, want the dictionary's spelling", f.set.Name)
	}
	if f.set.Value != "retail" {
		t.Errorf("value reached the store as %q, want it trimmed", f.set.Value)
	}
	if !f.set.Enabled {
		t.Error("a newly added attribute is not enabled")
	}
}

// The refusal must carry the dictionary's sentence, because that sentence
// names the attribute and says what is wrong with the value.
func TestFR_CAT_004_AnInvalidAttributeIsRefusedWithTheDictionarysReason(t *testing.T) {
	cases := []struct {
		name string
		form url.Values
		want string
	}{
		{"an attribute the dictionary does not know", url.Values{
			"plan_id": {"7"}, "vendor": {"mikrotik"},
			"attribute": {"Mikrotik-Rate-Limitt"}, "value": {"50M/50M"},
		}, "not an attribute this system can send"},
		{"an attribute the bandwidth builder sends", url.Values{
			"plan_id": {"7"}, "vendor": {"mikrotik"},
			"attribute": {"Mikrotik-Rate-Limit"}, "value": {"50M/50M"},
		}, "cannot be set per plan"},
		{"a wrongly typed value", url.Values{
			"plan_id": {"7"}, "vendor": {"mikrotik"},
			"attribute": {"Session-Timeout"}, "value": {"an hour"},
		}, "whole number"},
		{"no value", url.Values{
			"plan_id": {"7"}, "vendor": {"mikrotik"},
			"attribute": {"Mikrotik-Address-List"}, "value": {"   "},
		}, "needs a value"},
		{"an attribute for the wrong vendor", url.Values{
			"plan_id": {"7"}, "vendor": {"mikrotik"},
			"attribute": {"Cisco-AVPair"}, "value": {"ip:addr-pool=x"},
		}, "not an attribute this system can send"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, f := attrScreen()
			w := servePR(h, "noc_engineer", "POST", "/staff/plan-attributes", tc.form, h.SetPlanAttributeForm)
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
			if f.set.Name != "" {
				t.Errorf("the invalid attribute reached the store as %q", f.set.Name)
			}
		})
	}
}

func TestFR_CAT_004_SwitchingOffAndRemovingReachTheStore(t *testing.T) {
	h, f := attrScreen()

	servePR(h, "noc_engineer", "POST", "/staff/plan-attributes/3/enabled",
		url.Values{"enabled": {"false"}, "plan_id": {"7"}, "vendor": {"mikrotik"}},
		h.TogglePlanAttributeForm)
	if f.gotID != 3 || f.gotEnabl {
		t.Errorf("switch off reached the store as id=%d enabled=%v", f.gotID, f.gotEnabl)
	}

	servePR(h, "noc_engineer", "POST", "/staff/plan-attributes/3/enabled",
		url.Values{"enabled": {"true"}, "plan_id": {"7"}, "vendor": {"mikrotik"}},
		h.TogglePlanAttributeForm)
	if !f.gotEnabl {
		t.Error("switch on did not reach the store")
	}

	servePR(h, "noc_engineer", "POST", "/staff/plan-attributes/4/delete",
		url.Values{"plan_id": {"7"}, "vendor": {"mikrotik"}},
		h.DeletePlanAttributeForm)
	if f.deleted != 4 {
		t.Errorf("delete reached the store as id=%d", f.deleted)
	}
}

// An unknown vendor in the URL must be ignored rather than passed on, so a
// stale bookmark shows the screen instead of an error.
func TestFR_CAT_004_AnUnknownVendorInTheURLFallsBack(t *testing.T) {
	h, _ := attrScreen()
	w := servePR(h, "isp_owner", "GET", "/staff/plan-attributes?vendor=ubiquiti", nil, h.PlanAttributes)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "mikrotik") {
		t.Error("an unknown vendor did not fall back to a known one")
	}
}

// Network tier, same reach as Routers and IP Pools — and specifically not
// billing, because these attributes carry no price.
func TestFR_CAT_004_PlanAttributesReach(t *testing.T) {
	has := func(role string) bool {
		for _, sec := range AllowedSections(role, true) {
			if sec.Key == "plan-attributes" {
				return true
			}
		}
		return false
	}
	for _, role := range []string{"isp_owner", "noc_engineer"} {
		if !has(role) {
			t.Errorf("%s cannot reach the RADIUS attributes screen", role)
		}
	}
	for _, role := range []string{"billing_admin", "csr", "technician"} {
		if has(role) {
			t.Errorf("%s can reach the RADIUS attributes screen", role)
		}
	}
}

// A read-only role must not be offered the forms, and a write must be
// refused even when the URL is posted directly.
func TestFR_CAT_004_WritesAreRefusedForRolesThatOnlyRead(t *testing.T) {
	h, f := attrScreen()
	// billing_admin cannot reach the section at all, so requireSection
	// refuses before the manager check — which is the stronger outcome.
	w := servePR(h, "billing_admin", "POST", "/staff/plan-attributes",
		url.Values{"plan_id": {"7"}, "vendor": {"mikrotik"},
			"attribute": {"Mikrotik-Address-List"}, "value": {"retail"}},
		h.SetPlanAttributeForm)
	if w.Code != 403 {
		t.Errorf("billing_admin posting an attribute got %d, want 403", w.Code)
	}
	if f.set.Name != "" {
		t.Error("the attribute reached the store despite the refusal")
	}
}

func TestFR_CAT_004_AnUnconfiguredStoreSaysSoRatherThanFailing(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/plan-attributes", nil, h.PlanAttributes)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}
