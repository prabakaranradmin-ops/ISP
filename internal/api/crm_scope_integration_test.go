//go:build integration

// Franchise isolation on every lead route, and the lead routes' failure
// paths — FR-CRM-001..003, CRD-FRN-001.
//
// crm_inventory_integration_test.go proves a franchise-scoped caller's list
// shows only their own leads. The list is one of six routes. A partner who
// cannot list a competitor's leads but can fetch, move or convert one by id
// has the same access with one more step; each route decides scope from the
// token, and each is checked here. Also covered: a franchise-scoped token
// with no franchise binding (refused, never treated as ISP-wide), the funnel
// and list filters reaching the store, and the console's service functions
// (LeadList, LeadByID, LeadFunnelReport), which had no tests.
package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/crm"
)

// scopedLeads records the scope the store was asked under and fails on
// demand; everything else is the shared stub.
type scopedLeads struct {
	*stubLeads

	mu          sync.Mutex
	funnelScope *int
	funnelAsked bool
	getErr      error
	listErr     error
	funnelErr   error
	updateErr   error
}

func (s *scopedLeads) GetLead(ctx context.Context, id int) (*crm.Lead, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.stubLeads.GetLead(ctx, id)
}

func (s *scopedLeads) ListLeads(ctx context.Context, status, source *string, scope *int) ([]crm.Lead, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.stubLeads.ListLeads(ctx, status, source, scope)
}

func (s *scopedLeads) UpdateLead(ctx context.Context, id int, status, assignedTo, notes, lostReason *string) (*crm.Lead, error) {
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	return s.stubLeads.UpdateLead(ctx, id, status, assignedTo, notes, lostReason)
}

func (s *scopedLeads) GetFunnel(ctx context.Context, scope *int) (*crm.FunnelReport, error) {
	s.mu.Lock()
	s.funnelScope, s.funnelAsked = scope, true
	s.mu.Unlock()
	if s.funnelErr != nil {
		return nil, s.funnelErr
	}
	return s.stubLeads.GetFunnel(ctx, scope)
}

type leadRig struct {
	mux    *http.ServeMux
	h      *api.Handler
	leads  *scopedLeads
	mine   int // a lead in franchise 2
	theirs int // a lead in franchise 3
	ours   int // an ISP-wide lead, no franchise
}

func newLeadRig(t *testing.T) *leadRig {
	t.Helper()
	two, three := 2, 3
	r := &leadRig{leads: &scopedLeads{stubLeads: newStubLeads()}}
	r.mine = r.leads.seed(crm.Lead{FullName: "Anitha", MobileNumber: "+919800000101", Source: "walk_in", FranchiseID: &two}).ID
	r.theirs = r.leads.seed(crm.Lead{FullName: "Bharat", MobileNumber: "+919800000102", Source: "referral", FranchiseID: &three}).ID
	r.ours = r.leads.seed(crm.Lead{FullName: "Chitra", MobileNumber: "+919800000103", Source: "website"}).ID
	r.h = api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{},
		Wallet: billing.NewWalletService(&stubWallet{}), Leads: r.leads})
	r.mux = http.NewServeMux()
	r.h.RegisterRoutes(r.mux, itJWTSecret)
	return r
}

func (r *leadRig) as(t *testing.T, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}

func leadPath(id int, suffix string) string { return "/api/v1/leads/" + itoa(id) + suffix }

// A partner reaches their own leads by id, and nobody else's — fetching,
// moving and converting all decided by the token.
func TestFR_CRM_001_APartnerCannotReachAnotherPartnersLeadById(t *testing.T) {
	r := newLeadRig(t)
	lco := itFranchiseToken(t, "lco", 2)

	if rec := r.as(t, lco, http.MethodGet, leadPath(r.mine, ""), ""); rec.Code != http.StatusOK {
		t.Errorf("own lead: %d %s", rec.Code, rec.Body.String())
	}
	for _, id := range []int{r.theirs, r.ours} {
		if rec := r.as(t, lco, http.MethodGet, leadPath(id, ""), ""); rec.Code != http.StatusForbidden {
			t.Errorf("GET lead %d from franchise 2: %d", id, rec.Code)
		}
		if rec := r.as(t, lco, http.MethodPatch, leadPath(id, ""), `{"status":"contacted"}`); rec.Code != http.StatusForbidden {
			t.Errorf("PATCH lead %d from franchise 2: %d", id, rec.Code)
		}
	}
	if got, _ := r.leads.stubLeads.GetLead(context.Background(), r.theirs); got.Status != crm.StatusNew {
		t.Errorf("another partner's lead was moved to %q", got.Status)
	}

	// The funnel is the partner's own.
	if rec := r.as(t, lco, http.MethodGet, "/api/v1/leads/funnel", ""); rec.Code != http.StatusOK {
		t.Fatalf("funnel: %d", rec.Code)
	}
	if !r.leads.funnelAsked || r.leads.funnelScope == nil || *r.leads.funnelScope != 2 {
		t.Errorf("the funnel was computed under scope %v, not the token's franchise", r.leads.funnelScope)
	}

	// ISP-wide staff see every lead and an unscoped funnel.
	owner := itRoleTokenWithSubject(t, "isp_owner", "owner1")
	for _, id := range []int{r.mine, r.theirs, r.ours} {
		if rec := r.as(t, owner, http.MethodGet, leadPath(id, ""), ""); rec.Code != http.StatusOK {
			t.Errorf("owner, lead %d: %d", id, rec.Code)
		}
	}
	r.as(t, owner, http.MethodGet, "/api/v1/leads/funnel", "")
	if r.leads.funnelScope != nil {
		t.Errorf("the owner's funnel was scoped to %v", *r.leads.funnelScope)
	}
}

// A franchise-scoped role whose token names no franchise is refused on every
// route, never treated as ISP-wide: the absence of a binding is not a binding
// to everything.
func TestFR_CRM_001_AnUnboundPartnerTokenSeesNothing(t *testing.T) {
	r := newLeadRig(t)
	unbound := itFranchiseToken(t, "franchise_staff", 0)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/leads", ""},
		{http.MethodPost, "/api/v1/leads", `{"full_name":"x","mobile_number":"+919800000999","source":"walk_in"}`},
		{http.MethodGet, "/api/v1/leads/funnel", ""},
		{http.MethodGet, leadPath(r.ours, ""), ""},
		{http.MethodPatch, leadPath(r.ours, ""), `{"status":"contacted"}`},
	} {
		if rec := r.as(t, unbound, tc.method, tc.path, tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s with no franchise binding: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if r.leads.funnelAsked {
		t.Error("an unbound token reached the funnel query")
	}
}

func TestFR_CRM_001_LeadListFiltersAndFailures(t *testing.T) {
	r := newLeadRig(t)
	owner := itRoleTokenWithSubject(t, "isp_owner", "owner1")

	rec := r.as(t, owner, http.MethodGet, "/api/v1/leads?source=referral", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Bharat") || strings.Contains(rec.Body.String(), "Anitha") {
		t.Errorf("source filter: %d %s", rec.Code, rec.Body.String())
	}
	if rec := r.as(t, owner, http.MethodGet, "/api/v1/leads?status=lost", ""); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("an empty list is [] not null: %s", rec.Body.String())
	}

	for name, tc := range map[string]struct {
		setup        func(*scopedLeads)
		method, path string
		body         string
		code         int
	}{
		"list fails":         {func(s *scopedLeads) { s.listErr = errors.New("db down") }, http.MethodGet, "/api/v1/leads", "", http.StatusInternalServerError},
		"funnel fails":       {func(s *scopedLeads) { s.funnelErr = errors.New("db down") }, http.MethodGet, "/api/v1/leads/funnel", "", http.StatusInternalServerError},
		"get fails":          {func(s *scopedLeads) { s.getErr = errors.New("db down") }, http.MethodGet, "/api/v1/leads/1", "", http.StatusInternalServerError},
		"get, bad id":        {nil, http.MethodGet, "/api/v1/leads/x", "", http.StatusBadRequest},
		"get, missing":       {nil, http.MethodGet, "/api/v1/leads/99", "", http.StatusNotFound},
		"move, bad id":       {nil, http.MethodPatch, "/api/v1/leads/x", `{}`, http.StatusBadRequest},
		"move, malformed":    {nil, http.MethodPatch, "/api/v1/leads/1", `{`, http.StatusBadRequest},
		"move, missing":      {nil, http.MethodPatch, "/api/v1/leads/99", `{"status":"contacted"}`, http.StatusNotFound},
		"move to convert":    {nil, http.MethodPatch, "/api/v1/leads/1", `{"status":"converted"}`, http.StatusUnprocessableEntity},
		"move fails":         {func(s *scopedLeads) { s.updateErr = errors.New("db down") }, http.MethodPatch, "/api/v1/leads/1", `{"status":"contacted"}`, http.StatusInternalServerError},
		"create, malformed":  {nil, http.MethodPost, "/api/v1/leads", `{`, http.StatusBadRequest},
		"create fails":       {func(s *scopedLeads) { s.createErr = errors.New("db down") }, http.MethodPost, "/api/v1/leads", `{"full_name":"Devi","mobile_number":"+919800000104","source":"walk_in"}`, http.StatusInternalServerError},
		"convert, bad id":    {nil, http.MethodPost, "/api/v1/leads/x/convert", `{}`, http.StatusBadRequest},
		"convert, malformed": {nil, http.MethodPost, "/api/v1/leads/1/convert", `{`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLeadRig(t)
			if tc.setup != nil {
				tc.setup(r.leads)
			}
			rec := r.as(t, owner, tc.method, tc.path, tc.body)
			if rec.Code != tc.code {
				t.Errorf("got %d %s, want %d", rec.Code, rec.Body.String(), tc.code)
			}
			if strings.Contains(rec.Body.String(), "db down") {
				t.Error("the store's error reached the client")
			}
		})
	}
}

// The service functions the console calls apply the same scope.
func TestFR_CRM_001_TheConsolesLeadFunctionsKeepScope(t *testing.T) {
	r := newLeadRig(t)
	ctx := context.Background()
	two := 2

	list, err := r.h.LeadList(ctx, nil, nil, &two)
	if err != nil || len(list) != 1 || list[0].ID != r.mine {
		t.Errorf("LeadList under franchise 2: %v %v", list, err)
	}
	if _, err := r.h.LeadByID(ctx, r.theirs, &two); !errors.Is(err, api.ErrLeadNotEntitled) {
		t.Errorf("LeadByID across franchises: %v", err)
	}
	if l, err := r.h.LeadByID(ctx, r.mine, &two); err != nil || l.ID != r.mine {
		t.Errorf("LeadByID, own: %v %v", l, err)
	}
	if _, err := r.h.LeadByID(ctx, 99, nil); !errors.Is(err, api.ErrLeadNotFound) {
		t.Errorf("LeadByID, missing: %v", err)
	}
	if rep, err := r.h.LeadFunnelReport(ctx, &two); err != nil || rep == nil || r.leads.funnelScope == nil || *r.leads.funnelScope != 2 {
		t.Errorf("LeadFunnelReport: %v %v scope=%v", rep, err, r.leads.funnelScope)
	}
	if _, err := r.h.MoveLead(ctx, r.theirs, ptr("contacted"), nil, nil, nil, &two); !errors.Is(err, api.ErrLeadNotEntitled) {
		t.Errorf("MoveLead across franchises: %v", err)
	}

	bare := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{})})
	for name, call := range map[string]func() error{
		"LeadList":         func() error { _, err := bare.LeadList(ctx, nil, nil, nil); return err },
		"LeadByID":         func() error { _, err := bare.LeadByID(ctx, 1, nil); return err },
		"LeadFunnelReport": func() error { _, err := bare.LeadFunnelReport(ctx, nil); return err },
		"MoveLead":         func() error { _, err := bare.MoveLead(ctx, 1, nil, nil, nil, nil, nil); return err },
	} {
		if err := call(); !errors.Is(err, api.ErrLeadsUnavailable) {
			t.Errorf("%s unconfigured: %v", name, err)
		}
	}
}

func ptr(s string) *string { return &s }
