//go:build integration

// Scheduled plan changes — FR-BIL-023. Routes, role gates and date
// parsing; the behaviour is proven against a real database in internal/db.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

type stubPlanChanges struct {
	scheduled   billing.SchedulePlanChangeRequest
	cancelledID int
	reason      string
	scheduleErr error
	cancelErr   error
}

func samplePlanChange(req billing.SchedulePlanChangeRequest) *billing.ScheduledPlanChange {
	return &billing.ScheduledPlanChange{
		ID: 77, SubscriberID: req.SubscriberID, Username: "acme",
		FromPlanID: 1, FromPlanName: "Basic",
		NewPlanID: req.NewPlanID, NewPlanName: "Fibre 100",
		NewPlanPrice: decimal.RequireFromString("1200.00"),
		EffectiveOn:  req.EffectiveOn, Status: billing.PlanChangeScheduled,
		Reason: req.Reason, ScheduledBy: "meena", ScheduledAt: time.Now(),
	}
}

func (s *stubPlanChanges) SchedulePlanChange(_ context.Context, req billing.SchedulePlanChangeRequest, _ string) (*billing.ScheduledPlanChange, error) {
	if s.scheduleErr != nil {
		return nil, s.scheduleErr
	}
	norm, err := req.Normalise(time.Now())
	if err != nil {
		return nil, err
	}
	s.scheduled = norm
	return samplePlanChange(norm), nil
}

func (s *stubPlanChanges) CancelPlanChange(_ context.Context, id int, reason, _ string) error {
	if s.cancelErr != nil {
		return s.cancelErr
	}
	if strings.TrimSpace(reason) == "" {
		return billing.ErrPlanChangeInvalid
	}
	s.cancelledID, s.reason = id, reason
	return nil
}

func (s *stubPlanChanges) GetPlanChange(_ context.Context, id int) (*billing.ScheduledPlanChange, error) {
	if id != 77 {
		return nil, billing.ErrPlanChangeNotFound
	}
	return samplePlanChange(billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: time.Now().AddDate(0, 0, 10)}), nil
}

func (s *stubPlanChanges) ListPlanChanges(context.Context, billing.PlanChangeFilter) ([]billing.ScheduledPlanChange, error) {
	return []billing.ScheduledPlanChange{*samplePlanChange(billing.SchedulePlanChangeRequest{
		SubscriberID: 1, NewPlanID: 2, EffectiveOn: time.Now().AddDate(0, 0, 10)})}, nil
}

func planChangeHarness(t *testing.T, store api.PlanChangeStore) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{},
		Wallet: billing.NewWalletService(&stubWallet{}), PlanChanges: store})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func TestFR_BIL_023_PlanChangeAPIGates(t *testing.T) {
	when := time.Now().AddDate(0, 0, 20).Format("2006-01-02")
	body := `{"new_plan_id":2,"effective_on":"` + when + `","reason":"Downgrade"}`

	t.Run("billing and the owner can schedule", func(t *testing.T) {
		for _, role := range []string{"billing_admin", "isp_owner"} {
			store := &stubPlanChanges{}
			rec := httptestJSON(t, planChangeHarness(t, store), "POST",
				"/api/v1/subscribers/1/scheduled-plan-change", role, body)
			if rec.Code != 201 {
				t.Fatalf("%s: got %d: %s", role, rec.Code, rec.Body.String())
			}
			if store.scheduled.NewPlanID != 2 || store.scheduled.SubscriberID != 1 {
				t.Errorf("%s: store got %+v", role, store.scheduled)
			}
			if store.scheduled.EffectiveOn.Format("2006-01-02") != when {
				t.Errorf("%s: date reached the store as %v", role, store.scheduled.EffectiveOn)
			}
		}
	})

	t.Run("a CSR can read but not schedule or cancel", func(t *testing.T) {
		mux := planChangeHarness(t, &stubPlanChanges{})
		if rec := httptestGet(t, mux, "/api/v1/scheduled-plan-changes", "csr"); rec.Code != 200 {
			t.Errorf("csr reading the list: got %d", rec.Code)
		}
		if rec := httptestGet(t, mux, "/api/v1/scheduled-plan-changes/77", "csr"); rec.Code != 200 {
			t.Errorf("csr reading one: got %d", rec.Code)
		}
		if rec := httptestJSON(t, mux, "POST", "/api/v1/subscribers/1/scheduled-plan-change", "csr", body); rec.Code != 403 {
			t.Errorf("csr scheduling: got %d, want 403", rec.Code)
		}
		if rec := httptestJSON(t, mux, "POST", "/api/v1/scheduled-plan-changes/77/cancel", "csr", `{"reason":"x"}`); rec.Code != 403 {
			t.Errorf("csr cancelling: got %d, want 403", rec.Code)
		}
	})

	t.Run("a technician cannot even read", func(t *testing.T) {
		if rec := httptestGet(t, planChangeHarness(t, &stubPlanChanges{}),
			"/api/v1/scheduled-plan-changes", "technician"); rec.Code != 403 {
			t.Errorf("got %d, want 403", rec.Code)
		}
	})

	t.Run("an unconfigured store is a 503, not a 500", func(t *testing.T) {
		h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{},
			Wallet: billing.NewWalletService(&stubWallet{})})
		m := http.NewServeMux()
		h.RegisterRoutes(m, itJWTSecret)
		if rec := httptestGet(t, m, "/api/v1/scheduled-plan-changes", "isp_owner"); rec.Code != 503 {
			t.Errorf("got %d", rec.Code)
		}
	})
}

func TestFR_BIL_023_PlanChangeAPIValidation(t *testing.T) {
	when := time.Now().AddDate(0, 0, 20).Format("2006-01-02")

	t.Run("a malformed date is a 422 with a usable example", func(t *testing.T) {
		rec := httptestJSON(t, planChangeHarness(t, &stubPlanChanges{}), "POST",
			"/api/v1/subscribers/1/scheduled-plan-change", "billing_admin",
			`{"new_plan_id":2,"effective_on":"next Tuesday"}`)
		if rec.Code != 422 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "2026-04-01") {
			t.Errorf("no example in the message: %s", rec.Body.String())
		}
	})

	t.Run("a date already past is a 422, from the domain rules", func(t *testing.T) {
		past := time.Now().AddDate(0, 0, -5).Format("2006-01-02")
		rec := httptestJSON(t, planChangeHarness(t, &stubPlanChanges{}), "POST",
			"/api/v1/subscribers/1/scheduled-plan-change", "billing_admin",
			`{"new_plan_id":2,"effective_on":"`+past+`"}`)
		if rec.Code != 422 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		// The message points at the right tool for "move them now".
		if !strings.Contains(rec.Body.String(), "Change plan") {
			t.Errorf("message does not point to the immediate path: %s", rec.Body.String())
		}
	})

	t.Run("one already scheduled is a 409", func(t *testing.T) {
		rec := httptestJSON(t, planChangeHarness(t, &stubPlanChanges{scheduleErr: billing.ErrPlanChangeAlreadyScheduled}),
			"POST", "/api/v1/subscribers/1/scheduled-plan-change", "billing_admin",
			`{"new_plan_id":2,"effective_on":"`+when+`"}`)
		if rec.Code != 409 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("the same plan is a 409", func(t *testing.T) {
		rec := httptestJSON(t, planChangeHarness(t, &stubPlanChanges{scheduleErr: billing.ErrPlanChangeSamePlan}),
			"POST", "/api/v1/subscribers/1/scheduled-plan-change", "billing_admin",
			`{"new_plan_id":2,"effective_on":"`+when+`"}`)
		if rec.Code != 409 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("cancelling one already closed is a 409", func(t *testing.T) {
		rec := httptestJSON(t, planChangeHarness(t, &stubPlanChanges{cancelErr: billing.ErrPlanChangeNotPending}),
			"POST", "/api/v1/scheduled-plan-changes/77/cancel", "billing_admin", `{"reason":"x"}`)
		if rec.Code != 409 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("an unknown change is a 404", func(t *testing.T) {
		if rec := httptestGet(t, planChangeHarness(t, &stubPlanChanges{}),
			"/api/v1/scheduled-plan-changes/12", "isp_owner"); rec.Code != 404 {
			t.Errorf("got %d", rec.Code)
		}
	})

	t.Run("cancelling reaches the store with its reason", func(t *testing.T) {
		store := &stubPlanChanges{}
		rec := httptestJSON(t, planChangeHarness(t, store), "POST",
			"/api/v1/scheduled-plan-changes/77/cancel", "isp_owner", `{"reason":"customer changed their mind"}`)
		if rec.Code != 204 {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
		if store.cancelledID != 77 || store.reason != "customer changed their mind" {
			t.Errorf("store got %d / %q", store.cancelledID, store.reason)
		}
	})

	t.Run("a bad status filter is refused", func(t *testing.T) {
		if rec := httptestGet(t, planChangeHarness(t, &stubPlanChanges{}),
			"/api/v1/scheduled-plan-changes?status=maybe", "isp_owner"); rec.Code != 422 {
			t.Errorf("got %d", rec.Code)
		}
	})
}

// The JSON a client reads: the date as a bare day, and the plan names that
// make the change explicable without a second lookup.
func TestFR_BIL_023_PlanChangeJSONShape(t *testing.T) {
	rec := httptestGet(t, planChangeHarness(t, &stubPlanChanges{}), "/api/v1/scheduled-plan-changes/77", "isp_owner")
	if rec.Code != 200 {
		t.Fatalf("got %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"from_plan_name", "new_plan_name", "effective_on", "status", "new_plan_price"} {
		if got[key] == nil || got[key] == "" {
			t.Errorf("%s is missing from the response", key)
		}
	}
	// A bare date, not a timestamp: effective_on is a day.
	if on, _ := got["effective_on"].(string); len(on) != 10 {
		t.Errorf("effective_on is %q, want YYYY-MM-DD", on)
	}
	if got["status"] != billing.PlanChangeScheduled {
		t.Errorf("status is %v", got["status"])
	}
}
