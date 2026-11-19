package staffui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// Plan editing, duplication, retirement and CSV export — FR-CAT-001.

type fakeCatalogue struct {
	plans     []Plan
	rates     []GSTRate
	created   []Plan
	updated   []Plan
	activeSet map[int]bool
	actor     string
	err       error
	// setActiveErr fails only SetPlanActive. A blanket err would fail
	// ListPlans too, and the page would show "could not load plans"
	// instead of the message under test.
	setActiveErr error
	// activeListed records whether ListActivePlans was the one asked, so a
	// test can prove an assignment dropdown filters rather than trusting
	// that it does.
	activeListed bool
	allListed    bool
}

func newFakeCatalogue() *fakeCatalogue {
	return &fakeCatalogue{activeSet: map[int]bool{}}
}

func (f *fakeCatalogue) ListPlans(context.Context) ([]Plan, error) {
	f.allListed = true
	return f.plans, f.err
}

func (f *fakeCatalogue) ListActivePlans(context.Context) ([]Plan, error) {
	f.activeListed = true
	var out []Plan
	for _, p := range f.plans {
		if p.Active {
			out = append(out, p)
		}
	}
	return out, f.err
}

func (f *fakeCatalogue) CreatePlan(ctx context.Context, p Plan) (*Plan, error) {
	if f.err != nil {
		return nil, f.err
	}
	for _, existing := range f.plans {
		if existing.Active && strings.EqualFold(strings.TrimSpace(existing.Name), strings.TrimSpace(p.Name)) {
			return nil, ErrPlanNameTaken
		}
	}
	f.created, f.actor = append(f.created, p), middleware.SubjectFromContext(ctx)
	out := p
	out.ID = 100 + len(f.created)
	out.Active = true
	f.plans = append(f.plans, out)
	return &out, nil
}

func (f *fakeCatalogue) UpdatePlan(ctx context.Context, p Plan, _ string) (*Plan, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.updated, f.actor = append(f.updated, p), middleware.SubjectFromContext(ctx)
	out := p
	out.Active = true
	for _, existing := range f.plans {
		if existing.ID == p.ID {
			out.Subscribers = existing.Subscribers
		}
	}
	return &out, nil
}

func (f *fakeCatalogue) SetPlanActive(ctx context.Context, id int, active bool, _ string) error {
	if f.setActiveErr != nil {
		return f.setActiveErr
	}
	if f.err != nil {
		return f.err
	}
	f.activeSet[id], f.actor = active, middleware.SubjectFromContext(ctx)
	return nil
}

func (f *fakeCatalogue) ListGSTRates(context.Context) ([]GSTRate, error) { return f.rates, nil }

func (f *fakeCatalogue) CreateGSTRate(_ context.Context, r GSTRate) (*GSTRate, error) {
	return &r, nil
}

func samplePlan(id int, name string, active bool, subscribers int) Plan {
	return Plan{
		ID: id, Name: name, RateLimitString: "100M/100M", VolumeGB: 3300,
		FUPThresholdBytes: 0, Price: decimal.RequireFromString("799.00"),
		ValidityDays: 30, Active: active, Subscribers: subscribers,
		CreatedAt: time.Now(),
	}
}

// servePlan drives a catalogue handler with a session of the given role.
//
// Its own path parsing rather than servePR's: these URLs are
// /staff/catalogue/plans/{id}/..., so the id is the fourth segment, and
// servePR's "third segment" rule would hand the handler "plans" and every
// test would fail on a 400 that says nothing about the code.
func servePlan(h *Handler, role, method, target string, form url.Values, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if method == "GET" {
		r = httptest.NewRequest("GET", target, nil)
	} else {
		r = httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) > 3 {
		r.SetPathValue("id", parts[3])
	}
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}

func TestFR_CAT_001_EditingAPlanReachesTheStoreAndSaysWhoIsAffected(t *testing.T) {
	f := newFakeCatalogue()
	f.plans = []Plan{samplePlan(1, "Fibre 100", true, 42)}
	h := &Handler{catalogue: f}

	form := url.Values{
		"name": {"Fibre 100"}, "rate_limit_string": {"200M/200M"},
		"volume_gb": {"4000"}, "price": {"999.00"}, "validity_days": {"30"},
		"fup_threshold_gb": {""}, "fup_throttle_string": {""},
	}
	w := servePlan(h, "billing_admin", "POST", "/staff/catalogue/plans/1", form, h.UpdatePlanForm)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if len(f.updated) != 1 {
		t.Fatalf("store got %+v", f.updated)
	}
	got := f.updated[0]
	if got.ID != 1 || got.RateLimitString != "200M/200M" ||
		!got.Price.Equal(decimal.RequireFromString("999.00")) {
		t.Errorf("store got %+v", got)
	}
	if f.actor != "billing_admin_user" {
		t.Errorf("attributed to %q", f.actor)
	}

	// The operator is told what the edit costs: a price change re-prices
	// everyone on the plan from their next renewal, and that is the single
	// most surprising thing about editing a tariff.
	body := w.Body.String()
	if !strings.Contains(body, "42 subscriber(s)") {
		t.Error("the page does not say how many subscribers the edit affects")
	}
	if !strings.Contains(body, "next renewal") {
		t.Error("the page does not say when the new price takes effect")
	}
}

func TestFR_CAT_001_EditValidationRefusesNonsense(t *testing.T) {
	f := newFakeCatalogue()
	f.plans = []Plan{samplePlan(1, "Fibre 100", true, 0)}
	h := &Handler{catalogue: f}

	valid := url.Values{
		"name": {"Fibre 100"}, "rate_limit_string": {"100M/100M"},
		"volume_gb": {"3300"}, "price": {"799.00"}, "validity_days": {"30"},
	}
	cases := []struct {
		name string
		mut  func(url.Values)
		want string
	}{
		{"no name", func(v url.Values) { v.Set("name", "  ") }, "name is required"},
		{"a speed with no slash", func(v url.Values) { v.Set("rate_limit_string", "100M") }, "rate-limit form"},
		{"a non-numeric price", func(v url.Values) { v.Set("price", "free") }, "Price must be"},
		{"zero validity", func(v url.Values) { v.Set("validity_days", "0") }, "Validity must be"},
		{"a FUP threshold with no throttle", func(v url.Values) { v.Set("fup_threshold_gb", "1000") }, "needs a throttle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := url.Values{}
			for k, vs := range valid {
				form[k] = append([]string(nil), vs...)
			}
			tc.mut(form)
			before := len(f.updated)
			w := servePlan(h, "isp_owner", "POST", "/staff/catalogue/plans/1", form, h.UpdatePlanForm)
			if w.Code != 200 {
				t.Fatalf("got %d", w.Code)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("the page does not explain the problem (wanted %q)", tc.want)
			}
			if len(f.updated) != before {
				t.Error("an invalid edit reached the store")
			}
		})
	}
}

// Retiring is the whole point of the feature: it must stop the plan being
// offered and must not move anybody.
func TestFR_CAT_001_RetiringAPlanKeepsItsSubscribers(t *testing.T) {
	f := newFakeCatalogue()
	f.plans = []Plan{samplePlan(1, "Fibre 100", true, 42)}
	h := &Handler{catalogue: f}

	w := servePlan(h, "isp_owner", "POST", "/staff/catalogue/plans/1/active",
		url.Values{"active": {"false"}}, h.SetPlanActiveForm)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if active, ok := f.activeSet[1]; !ok || active {
		t.Errorf("retire reached the store as %v/%v", active, ok)
	}
	body := w.Body.String()
	if !strings.Contains(body, "no longer offered") {
		t.Error("the page does not say the plan is no longer offered")
	}
	// The reassurance that matters: nobody is moved.
	if !strings.Contains(body, "keep renewing at its price") {
		t.Error("the page does not say existing subscribers keep the plan")
	}

	t.Run("and it can be offered again", func(t *testing.T) {
		w := servePlan(h, "isp_owner", "POST", "/staff/catalogue/plans/1/active",
			url.Values{"active": {"true"}}, h.SetPlanActiveForm)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		if !f.activeSet[1] {
			t.Error("reinstate did not reach the store")
		}
	})

	t.Run("reinstating into a name collision is explained, not a 500", func(t *testing.T) {
		g := newFakeCatalogue()
		g.plans = []Plan{samplePlan(1, "Fibre 100", false, 0)}
		g.setActiveErr = ErrPlanNameTaken
		hh := &Handler{catalogue: g}
		w := servePlan(hh, "isp_owner", "POST", "/staff/catalogue/plans/1/active",
			url.Values{"active": {"true"}}, hh.SetPlanActiveForm)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "already uses its name") {
			t.Errorf("unhelpful page")
		}
	})
}

func TestFR_CAT_001_DuplicatingAPlanCopiesEveryFieldUnderANewName(t *testing.T) {
	f := newFakeCatalogue()
	source := samplePlan(1, "Fibre 100", true, 42)
	source.FUPThresholdBytes = 3300 * 1024 * 1024 * 1024
	source.FUPThrottleString = "10M/10M"
	f.plans = []Plan{source}
	h := &Handler{catalogue: f}

	w := servePlan(h, "isp_owner", "POST", "/staff/catalogue/plans/1/duplicate", nil, h.DuplicatePlanForm)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if len(f.created) != 1 {
		t.Fatalf("store got %+v", f.created)
	}
	got := f.created[0]

	// Every billable field copied, so the operator changes one thing
	// rather than retyping six.
	if got.RateLimitString != source.RateLimitString || got.VolumeGB != source.VolumeGB ||
		!got.Price.Equal(source.Price) || got.ValidityDays != source.ValidityDays ||
		got.FUPThresholdBytes != source.FUPThresholdBytes || got.FUPThrottleString != source.FUPThrottleString {
		t.Errorf("the copy differs from its source: %+v", got)
	}
	// A new name, because uq_plan_name_active forbids two offered plans
	// sharing one.
	if got.Name != "Fibre 100 (copy)" {
		t.Errorf("the copy is named %q", got.Name)
	}
	// And the copy starts with nobody on it.
	if got.Subscribers != 0 {
		t.Errorf("the copy claims %d subscribers", got.Subscribers)
	}
	if !strings.Contains(w.Body.String(), "Rename it and set its price") {
		t.Error("the page does not say what to do with the copy")
	}

	t.Run("a second copy gets its own name", func(t *testing.T) {
		w := servePlan(h, "isp_owner", "POST", "/staff/catalogue/plans/1/duplicate", nil, h.DuplicatePlanForm)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.created) != 2 {
			t.Fatalf("store got %d copies", len(f.created))
		}
		if f.created[1].Name != "Fibre 100 (copy 2)" {
			t.Errorf("the second copy is named %q", f.created[1].Name)
		}
	})

	t.Run("duplicating something that does not exist is explained", func(t *testing.T) {
		w := servePlan(h, "isp_owner", "POST", "/staff/catalogue/plans/999/duplicate", nil, h.DuplicatePlanForm)
		if !strings.Contains(w.Body.String(), "No plan with that id") {
			t.Error("unhelpful page")
		}
	})
}

// duplicateName only has to avoid *offered* plans, because
// uq_plan_name_active is a partial index over active rows.
func TestFR_CAT_001_DuplicateNameIgnoresRetiredPlans(t *testing.T) {
	existing := []Plan{
		samplePlan(1, "Fibre 100", true, 0),
		samplePlan(2, "Fibre 100 (copy)", false, 0), // retired: no collision
	}
	if got := duplicateName("Fibre 100", existing); got != "Fibre 100 (copy)" {
		t.Errorf("got %q, want the name a retired plan holds to be reusable", got)
	}

	existing[1].Active = true
	if got := duplicateName("Fibre 100", existing); got != "Fibre 100 (copy 2)" {
		t.Errorf("got %q, want it to step past an offered copy", got)
	}
}

func TestFR_CAT_001_PlansExportAsCSV(t *testing.T) {
	f := newFakeCatalogue()
	offered := samplePlan(1, "Fibre 100", true, 42)
	retired := samplePlan(2, "Old Fibre 50", false, 7)
	retired.UpdatedBy = "meena"
	when := time.Date(2026, time.March, 1, 10, 30, 0, 0, time.UTC)
	retired.UpdatedAt = &when
	retired.FUPThresholdBytes = 1000 * 1024 * 1024 * 1024
	retired.FUPThrottleString = "5M/5M"
	f.plans = []Plan{offered, retired}
	h := &Handler{catalogue: f}

	w := servePlan(h, "isp_owner", "GET", "/staff/catalogue/plans.csv", nil, h.ExportPlansCSV)
	if w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type is %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") ||
		!strings.Contains(cd, "plans-") {
		t.Errorf("Content-Disposition is %q", cd)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the CSV is served without nosniff")
	}

	body := w.Body.String()
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n")), "\n")
	if len(lines) != 3 {
		t.Fatalf("want a header and 2 rows, got %d lines:\n%s", len(lines), body)
	}
	if !strings.HasPrefix(lines[0], "id,name,status,speed") {
		t.Errorf("header is %q", lines[0])
	}
	// Retired plans are included: the reason to export is usually to
	// reconcile what subscribers are being billed, and some of them are
	// on plans no longer sold.
	if !strings.Contains(body, "Old Fibre 50") || !strings.Contains(body, "Retired") {
		t.Error("the export leaves out retired plans")
	}
	if !strings.Contains(body, "Offered") {
		t.Error("the export does not say which plans are offered")
	}
	// The FUP threshold is exported in GB, not bytes: a column of
	// 1073741824000 is not something anybody reconciles by eye.
	if !strings.Contains(body, ",1000,") {
		t.Errorf("the FUP threshold is not in GB:\n%s", body)
	}
	if !strings.Contains(body, "799.00") || !strings.Contains(body, ",42,") {
		t.Error("the export is missing the price or the subscriber count")
	}
	if !strings.Contains(body, "meena") {
		t.Error("the export does not say who last edited a plan")
	}
}

// The dropdowns that assign a plan must offer only the ones still sold,
// and the one that filters existing subscribers must not.
func TestFR_CAT_001_AssignmentDropdownsOfferOnlyOfferedPlans(t *testing.T) {
	t.Run("the change-plan dropdown filters", func(t *testing.T) {
		f := newFakeCatalogue()
		f.plans = []Plan{samplePlan(1, "Fibre 100", true, 0), samplePlan(2, "Old", false, 7)}
		h := &Handler{catalogue: f}
		// renderSubscriberDetail needs no subscriber store to reach the
		// plan lookup; it degrades past the missing panels.
		servePR(h, "isp_owner", "GET", "/staff/subscribers/5", nil, func(w http.ResponseWriter, r *http.Request) {
			s := testSession()
			s.Role = "isp_owner"
			h.renderSubscriberDetail(w, r, s, 5, "")
		})
		if !f.activeListed {
			t.Error("the change-plan dropdown read every plan instead of the offered ones")
		}
	})

	t.Run("the announcement audience filter does not", func(t *testing.T) {
		f := newFakeCatalogue()
		f.plans = []Plan{samplePlan(1, "Fibre 100", true, 0), samplePlan(2, "Old", false, 7)}
		// An announcer is required: renderAnnouncements bails before it
		// reads the plans without one, and the test would then pass by
		// never reaching the code it names.
		h := &Handler{catalogue: f, announcer: &fakeAnnouncer{}}
		servePR(h, "isp_owner", "GET", "/staff/announcements", nil, h.Announcements)
		if !f.allListed {
			t.Error("the announcement filter read only offered plans; subscribers on a retired plan would be unreachable")
		}
		if f.activeListed {
			t.Error("the announcement filter read the offered-only list")
		}
	})
}
