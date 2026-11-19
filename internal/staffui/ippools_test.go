package staffui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/shopspring/decimal"
)

// IP pools and static addresses in the console — FR-NET-005.

type fakeIPPools struct {
	err         error
	created     []nas.Pool
	deleted     int
	excluded    []nas.PoolExclusion
	unexcluded  int
	allocated   []nas.AllocateRequest
	releasedID  int
	reason      string
	actor       string
	pools       []nas.Pool
	assignments []nas.StaticAssignment
}

func (f *fakeIPPools) CreatePool(ctx context.Context, in nas.Pool) (*nas.Pool, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.created, f.actor = append(f.created, in.Normalise()), middleware.SubjectFromContext(ctx)
	out := in.Normalise()
	out.ID = len(f.created)
	return &out, nil
}

func (f *fakeIPPools) DeletePool(_ context.Context, id int) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = id
	return nil
}

func (f *fakeIPPools) ListPools(_ context.Context, _ nas.PoolFilter) ([]nas.Pool, error) {
	return f.pools, nil
}

func (f *fakeIPPools) AddExclusion(ctx context.Context, poolID int, r nas.AddrRange, reason string) (*nas.PoolExclusion, error) {
	if f.err != nil {
		return nil, f.err
	}
	e := nas.PoolExclusion{ID: 1, PoolID: poolID, Range: r, Reason: reason}
	f.excluded, f.actor = append(f.excluded, e), middleware.SubjectFromContext(ctx)
	return &e, nil
}

func (f *fakeIPPools) RemoveExclusion(_ context.Context, id int) error {
	if f.err != nil {
		return f.err
	}
	f.unexcluded = id
	return nil
}

func (f *fakeIPPools) AllocateStaticIP(ctx context.Context, req nas.AllocateRequest) (*nas.StaticAssignment, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.allocated, f.actor = append(f.allocated, req), middleware.SubjectFromContext(ctx)
	return &nas.StaticAssignment{ID: 9, SubscriberID: req.SubscriberID}, nil
}

func (f *fakeIPPools) ReleaseStaticIP(ctx context.Context, id int, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return nas.ErrPoolInvalid
	}
	if f.err != nil {
		return f.err
	}
	f.releasedID, f.reason, f.actor = id, reason, middleware.SubjectFromContext(ctx)
	return nil
}

func (f *fakeIPPools) ListSubscriberStaticIPs(context.Context, int) ([]nas.StaticAssignment, error) {
	return f.assignments, nil
}

func (f *fakeIPPools) ListPoolAssignments(context.Context, int) ([]nas.StaticAssignment, error) {
	return f.assignments, nil
}

func testPool(t *testing.T) nas.Pool {
	t.Helper()
	r, err := nas.ParseAddrRange("203.0.113.0/29", "")
	if err != nil {
		t.Fatal(err)
	}
	return nas.Pool{ID: 1, Name: "Customer statics", Purpose: nas.PoolStatic, Range: r,
		CreatedBy: "meena", CreatedAt: time.Now()}
}

func TestFR_NET_005_IPPoolScreenGates(t *testing.T) {
	t.Run("a NOC engineer sees the management forms", func(t *testing.T) {
		f := &fakeIPPools{pools: []nas.Pool{testPool(t)}}
		h := &Handler{ipPools: f}
		w := servePR(h, "noc_engineer", "GET", "/staff/ip-pools", nil, h.IPPools)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "Customer statics") || !strings.Contains(body, "203.0.113.0–203.0.113.7") {
			t.Error("the pool list did not render")
		}
		if !strings.Contains(body, "Add a pool") {
			t.Error("a NOC engineer was not served the management forms")
		}
	})

	t.Run("billing staff cannot reach the screen at all", func(t *testing.T) {
		// Not in the section's role list: a pool is network estate.
		// Billing staff can still assign a subscriber an address from the
		// subscriber's own page, which is gated separately.
		for _, sec := range AllowedSections("billing_admin", false) {
			if sec.Key == "ip-pools" {
				t.Error("billing_admin can reach the IP pools section")
			}
		}
	})

	t.Run("a technician cannot post any pool action", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		cases := []struct {
			name   string
			target string
			fn     func(http.ResponseWriter, *http.Request)
			form   url.Values
		}{
			{"create", "/staff/ip-pools", h.CreatePoolForm,
				url.Values{"name": {"X"}, "purpose": {"static"}, "first_address": {"10.0.0.1"}}},
			{"delete", "/staff/ip-pools/1/delete", h.DeletePoolForm, nil},
			{"exclude", "/staff/ip-pool-exclusions", h.AddExclusionForm,
				url.Values{"pool_id": {"1"}, "first_address": {"10.0.0.1"}, "reason": {"x"}}},
			{"unexclude", "/staff/ip-pool-exclusions/1/delete", h.RemoveExclusionForm, nil},
		}
		for _, c := range cases {
			if code := servePR(h, "technician", "POST", c.target, c.form, c.fn).Code; code != 403 {
				t.Errorf("%s: got %d, want 403", c.name, code)
			}
		}
		if len(f.created) != 0 || f.deleted != 0 || len(f.excluded) != 0 || f.unexcluded != 0 {
			t.Error("a refused request still reached the store")
		}
	})

	t.Run("an unconfigured store says so rather than failing", func(t *testing.T) {
		h := &Handler{}
		if code := servePR(h, "isp_owner", "GET", "/staff/ip-pools", nil, h.IPPools).Code; code != 503 {
			t.Errorf("got %d, want 503", code)
		}
	})
}

func TestFR_NET_005_PoolFormsReachTheStore(t *testing.T) {
	t.Run("creating a pool from a CIDR block", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		form := url.Values{"name": {"  Customer   statics "}, "purpose": {"static"},
			"first_address": {"203.0.113.0/29"}, "description": {"Business ring"}}
		w := servePR(h, "noc_engineer", "POST", "/staff/ip-pools", form, h.CreatePoolForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.created) != 1 {
			t.Fatalf("store got %+v", f.created)
		}
		got := f.created[0]
		if got.Name != "Customer statics" {
			t.Errorf("name reached the store as %q", got.Name)
		}
		if got.Range.First.String() != "203.0.113.0" || got.Range.Last.String() != "203.0.113.7" {
			t.Errorf("range reached the store as %s", got.Range)
		}
		if got.NASID != nil {
			t.Errorf("no router was chosen but nas_id is %v", got.NASID)
		}
		if f.actor != "noc_engineer_user" {
			t.Errorf("attributed to %q", f.actor)
		}
	})

	t.Run("a pool tied to a router", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		form := url.Values{"name": {"Edge statics"}, "purpose": {"static"},
			"first_address": {"203.0.113.10"}, "last_address": {"203.0.113.20"}, "nas_id": {"7"}}
		if w := servePR(h, "isp_owner", "POST", "/staff/ip-pools", form, h.CreatePoolForm); w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if f.created[0].NASID == nil || *f.created[0].NASID != 7 {
			t.Errorf("nas_id reached the store as %v", f.created[0].NASID)
		}
	})

	t.Run("a malformed range comes back worded, and nothing is saved", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		form := url.Values{"name": {"X"}, "purpose": {"static"}, "first_address": {"the office"}}
		w := servePR(h, "isp_owner", "POST", "/staff/ip-pools", form, h.CreatePoolForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "err=") {
			t.Errorf("redirect %q carries no reason", loc)
		}
		if len(f.created) != 0 {
			t.Error("an unparseable range reached the store")
		}
	})

	t.Run("a pool in use cannot be deleted, and the operator is told why", func(t *testing.T) {
		f := &fakeIPPools{err: nas.ErrPoolInUse}
		h := &Handler{ipPools: f}
		w := servePR(h, "isp_owner", "POST", "/staff/ip-pools/1/delete", nil, h.DeletePoolForm)
		if loc := w.Header().Get("Location"); !strings.Contains(loc, "p=inuse") {
			t.Errorf("redirect %q", loc)
		}
	})

	t.Run("excluding addresses takes the pool from the form, not the path", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		form := url.Values{"pool_id": {"3"}, "first_address": {"203.0.113.0"},
			"last_address": {"203.0.113.1"}, "reason": {"network and gateway"}}
		w := servePR(h, "noc_engineer", "POST", "/staff/ip-pool-exclusions", form, h.AddExclusionForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.excluded) != 1 || f.excluded[0].PoolID != 3 {
			t.Fatalf("store got %+v", f.excluded)
		}
		if f.excluded[0].Reason != "network and gateway" {
			t.Errorf("reason reached the store as %q", f.excluded[0].Reason)
		}
	})

	t.Run("excluding with no pool chosen is refused", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		form := url.Values{"pool_id": {""}, "first_address": {"203.0.113.0"}, "reason": {"x"}}
		w := servePR(h, "noc_engineer", "POST", "/staff/ip-pool-exclusions", form, h.AddExclusionForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.excluded) != 0 {
			t.Error("an exclusion with no pool reached the store")
		}
	})
}

func TestFR_NET_005_AssignFromTheSubscriberPage(t *testing.T) {
	t.Run("billing staff may assign, and the next free address is the default", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		form := url.Values{"pool_id": {"1"}, "address": {""}, "product_id": {"4"}}
		w := servePR(h, "billing_admin", "POST", "/staff/subscribers/5/static-ip", form, h.AllocateStaticIPForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if len(f.allocated) != 1 {
			t.Fatalf("store got %+v", f.allocated)
		}
		got := f.allocated[0]
		if got.SubscriberID != 5 || got.PoolID != 1 || got.ProductID != 4 {
			t.Errorf("store got %+v", got)
		}
		if got.Address.IsValid() {
			t.Errorf("a blank address became %s instead of being left to the pool", got.Address)
		}
		if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/staff/subscribers/5?msg=") {
			t.Errorf("redirect %q", loc)
		}
	})

	t.Run("a specific address reaches the store", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		form := url.Values{"pool_id": {"1"}, "address": {"203.0.113.4"}}
		if w := servePR(h, "noc_engineer", "POST", "/staff/subscribers/5/static-ip", form, h.AllocateStaticIPForm); w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if f.allocated[0].Address != netip.MustParseAddr("203.0.113.4") {
			t.Errorf("address reached the store as %s", f.allocated[0].Address)
		}
	})

	t.Run("a CSR and a technician cannot assign or release", func(t *testing.T) {
		for _, role := range []string{"csr", "technician"} {
			f := &fakeIPPools{}
			h := &Handler{ipPools: f}
			if code := servePR(h, role, "POST", "/staff/subscribers/5/static-ip",
				url.Values{"pool_id": {"1"}}, h.AllocateStaticIPForm).Code; code != 403 {
				t.Errorf("%s assigning: got %d, want 403", role, code)
			}
			if code := servePR(h, role, "POST", "/staff/static-ips/9/release",
				url.Values{"reason": {"x"}, "subscriber_id": {"5"}}, h.ReleaseStaticIPForm).Code; code != 403 {
				t.Errorf("%s releasing: got %d, want 403", role, code)
			}
			if len(f.allocated) != 0 || f.releasedID != 0 {
				t.Errorf("%s: a refused request reached the store", role)
			}
		}
	})

	t.Run("an address already held comes back worded", func(t *testing.T) {
		f := &fakeIPPools{err: nas.ErrAddressTaken}
		h := &Handler{ipPools: f}
		w := servePR(h, "noc_engineer", "POST", "/staff/subscribers/5/static-ip",
			url.Values{"pool_id": {"1"}, "address": {"203.0.113.4"}}, h.AllocateStaticIPForm)
		if w.Code != 200 {
			t.Fatalf("got %d, want the page re-rendered", w.Code)
		}
		if !strings.Contains(w.Body.String(), "already assigned") {
			t.Error("the page does not say the address is taken")
		}
	})

	t.Run("an exhausted pool comes back worded", func(t *testing.T) {
		f := &fakeIPPools{err: nas.ErrPoolExhausted}
		h := &Handler{ipPools: f}
		w := servePR(h, "noc_engineer", "POST", "/staff/subscribers/5/static-ip",
			url.Values{"pool_id": {"1"}}, h.AllocateStaticIPForm)
		if !strings.Contains(w.Body.String(), "No free address") {
			t.Error("the page does not say the pool is full")
		}
	})

	t.Run("releasing carries its reason and returns to the subscriber", func(t *testing.T) {
		f := &fakeIPPools{}
		h := &Handler{ipPools: f}
		form := url.Values{"reason": {"customer downgraded"}, "subscriber_id": {"5"}}
		w := servePR(h, "billing_admin", "POST", "/staff/static-ips/9/release", form, h.ReleaseStaticIPForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if f.releasedID != 9 || f.reason != "customer downgraded" {
			t.Errorf("store got %d / %q", f.releasedID, f.reason)
		}
	})
}

// The static-address panel on the subscriber page.
func TestFR_NET_005_SubscriberPageStaticIPCard(t *testing.T) {
	pool := testPool(t)
	productID := 4
	held := nas.StaticAssignment{
		ID: 9, PoolID: 1, PoolName: "Customer statics", SubscriberID: 5, Username: "biz",
		Address: netip.MustParseAddr("203.0.113.4"), Status: nas.AssignmentActive,
		SubscriberProductID: &productID, AssignedBy: "meena", AssignedAt: time.Now(),
	}

	render := func(role string, assign bool, current *nas.StaticAssignment, list []nas.StaticAssignment) string {
		rec := httptest.NewRecorder()
		s := testSession()
		s.Role = role
		(&Handler{}).render(rec, "subscriber_detail", pageData{Session: s, Data: subscriberDetailData{
			Subscriber:   &api.SubscriberRecord{ID: 5, Username: "biz", Status: "active", PlanID: 1},
			ShowBilling:  true,
			ShowStaticIP: true, ShowAssignStaticIP: assign,
			CurrentStaticIP: current, StaticIPs: list,
			StaticIPPools: []nas.Pool{pool},
			StaticIPProducts: []billing.Product{{
				ID: productID, Name: "Static IP address",
				Price: decimal.RequireFromString("200.00"), Kind: billing.ProductRecurring,
			}},
		}})
		return rec.Body.String()
	}

	t.Run("a held address shows with its pool and a release form", func(t *testing.T) {
		body := render("noc_engineer", true, &held, []nas.StaticAssignment{held})
		for _, want := range []string{"203.0.113.4", "Customer statics", "Billed as a catalogue charge",
			"meena", `action="/staff/static-ips/9/release"`} {
			if !strings.Contains(body, want) {
				t.Errorf("the card does not show %q", want)
			}
		}
		// With one held, the assign form must not also be offered.
		if strings.Contains(body, `action="/staff/subscribers/5/static-ip"`) {
			t.Error("the assign form is offered while an address is already held")
		}
	})

	t.Run("with none held the assign form is offered with pools and charges", func(t *testing.T) {
		body := render("billing_admin", true, nil, nil)
		if !strings.Contains(body, `action="/staff/subscribers/5/static-ip"`) {
			t.Fatal("the assign form is missing")
		}
		if !strings.Contains(body, "Customer statics") {
			t.Error("the pool is not offered")
		}
		if !strings.Contains(body, "Static IP address") || !strings.Contains(body, "200.00") {
			t.Error("the billing charge is not offered")
		}
		if !strings.Contains(body, "Not billed") {
			t.Error("there is no way to assign an address without billing it")
		}
	})

	t.Run("a CSR sees the address but gets no forms", func(t *testing.T) {
		body := render("csr", false, &held, []nas.StaticAssignment{held})
		if !strings.Contains(body, "203.0.113.4") {
			t.Error("a CSR cannot see the address")
		}
		if strings.Contains(body, `action="/staff/static-ips/9/release"`) ||
			strings.Contains(body, `action="/staff/subscribers/5/static-ip"`) {
			t.Error("a CSR was served the management forms")
		}
	})

	t.Run("with no pools set up the operator is pointed at the pools screen", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s := testSession()
		s.Role = "noc_engineer"
		(&Handler{}).render(rec, "subscriber_detail", pageData{Session: s, Data: subscriberDetailData{
			Subscriber:  &api.SubscriberRecord{ID: 5, Username: "biz", Status: "active"},
			ShowBilling: true, ShowStaticIP: true, ShowAssignStaticIP: true,
		}})
		if !strings.Contains(rec.Body.String(), `href="/staff/ip-pools"`) {
			t.Error("no pointer to the IP pools screen when none exist")
		}
	})
}
