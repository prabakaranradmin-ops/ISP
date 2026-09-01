//go:build integration

// Franchise / LCO endpoint tests — FR-FRN-003..006.
//
// The commission engine, the scoping middleware and the subscriber-listing
// handler in internal/revenue shipped in v2.0 with no route mounting them, so
// none of this was reachable and none of it was exercised end to end. These
// tests cover the routes that make it reachable, and — more importantly — the
// isolation that makes exposing it safe: CRD-FRN-001 requires that an LCO
// partner cannot see another partner's data, and a scoping bug there is a
// cross-tenant data leak, not a cosmetic defect.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// itFranchiseToken mints a token for a franchise-scoped role. franchiseID 0
// deliberately produces a token with no franchise binding, which is the
// misissued-token case the handlers must refuse rather than treat as
// ISP-wide.
func itFranchiseToken(t *testing.T, role string, franchiseID int) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             role,
		FranchiseID:      franchiseID,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign %s token: %v", role, err)
	}
	return tok
}

// ── Stub ─────────────────────────────────────────────────────────────────────

type stubFranchises struct {
	// lastScope records what scope the handler passed down, so a test can
	// assert the caller's token — not the URL — decided visibility.
	lastScope   *int
	scopeWasSet bool

	records []revenue.FranchiseRecord
	pnl     *revenue.FranchisePnL
	created *revenue.FranchiseRecord
	err     error
	// commissions records any settlement the handler triggered — see the
	// CalculateAndStoreLCOCommission stub below.
	commissions []revenue.LCOCommissionEntry
}

func (s *stubFranchises) ListFranchises(_ context.Context, franchiseID *int) ([]revenue.FranchiseRecord, error) {
	s.lastScope, s.scopeWasSet = franchiseID, true
	if s.err != nil {
		return nil, s.err
	}
	if franchiseID == nil {
		return s.records, nil
	}
	var out []revenue.FranchiseRecord
	for _, r := range s.records {
		if r.ID == *franchiseID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *stubFranchises) CreateFranchise(_ context.Context, req revenue.CreateFranchiseRequest) (*revenue.FranchiseRecord, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &revenue.FranchiseRecord{
		ID: 99, Name: req.Name, OwnerName: req.OwnerName,
		MobileNumber: req.MobileNumber, CommissionRatePct: req.CommissionRatePct, Status: "active",
	}, nil
}

func (s *stubFranchises) GetFranchisePnL(_ context.Context, franchiseID int, _, _ *time.Time) (*revenue.FranchisePnL, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.pnl == nil {
		return nil, nil
	}
	cp := *s.pnl
	cp.FranchiseID = franchiseID
	return &cp, nil
}

func (s *stubFranchises) ListConsolidatedPnL(_ context.Context, _, _ *time.Time) (*revenue.ConsolidatedPnL, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &revenue.ConsolidatedPnL{TotalRecharges: "1000.00", CommissionEarned: "100.00", NetToISP: "900.00"}, nil
}

// The three below satisfy the revenue.FranchiseQuerier half of
// api.FranchiseQuerier, which the handler passes to
// revenue.SettleCommissionForRecharge after a wallet recharge commits
// (CRD-EXP-006 Phase 2). None of the franchise HTTP handlers under test here
// reach them, so they record rather than simulate: a commission settlement
// that ran during a franchise-endpoint test would mean the wiring is wrong.
func (s *stubFranchises) GetFranchiseByID(_ context.Context, id int) (*revenue.Franchise, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &revenue.Franchise{ID: id, Name: "stub", CommissionRatePct: decimal.NewFromInt(10), Status: "active"}, nil
}

func (s *stubFranchises) CalculateAndStoreLCOCommission(_ context.Context, entry revenue.LCOCommissionEntry) error {
	s.commissions = append(s.commissions, entry)
	return nil
}

func (s *stubFranchises) GetSubscriberFranchiseID(_ context.Context, _ int) (*int, error) {
	return nil, nil
}

func newFranchiseMux(t *testing.T, fr *stubFranchises) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Franchises: fr,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func doGET(t *testing.T, mux *http.ServeMux, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ── Isolation: the requirement that matters ──────────────────────────────────

// TestFR_FRN_004_FranchiseAdmin_CannotReadAnotherPartnersPnL is the
// cross-tenant leak test. A franchise_admin bound to franchise 1 asking for
// franchise 2's P&L must be refused — the id in the path must never be able
// to widen what the token allows.
func TestFR_FRN_004_FranchiseAdmin_CannotReadAnotherPartnersPnL(t *testing.T) {
	fr := &stubFranchises{pnl: &revenue.FranchisePnL{FranchiseName: "Other Partner", TotalRecharges: "50000.00"}}
	mux := newFranchiseMux(t, fr)

	rec := doGET(t, mux, "/api/v1/franchises/2/pnl", itFranchiseToken(t, "franchise_admin", 1))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a cross-franchise read, got %d — %s", rec.Code, rec.Body.String())
	}
	// Belt and braces: the refusal must happen before any data is fetched,
	// so nothing about the other partner can leak through the response body.
	if strings.Contains(rec.Body.String(), "Other Partner") || strings.Contains(rec.Body.String(), "50000") {
		t.Errorf("refused response leaked the other partner's data: %s", rec.Body.String())
	}
}

func TestFR_FRN_004_FranchiseAdmin_CanReadTheirOwnPnL(t *testing.T) {
	fr := &stubFranchises{pnl: &revenue.FranchisePnL{FranchiseName: "My Partner", TotalRecharges: "1200.00"}}
	mux := newFranchiseMux(t, fr)

	rec := doGET(t, mux, "/api/v1/franchises/7/pnl", itFranchiseToken(t, "franchise_admin", 7))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for a partner reading their own P&L, got %d — %s", rec.Code, rec.Body.String())
	}
	var got revenue.FranchisePnL
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FranchiseID != 7 {
		t.Errorf("franchise_id = %d, want 7", got.FranchiseID)
	}
}

// TestFR_FRN_004_FranchiseRoleWithNoBinding_IsRefused covers the misissued
// token. A franchise-scoped role whose token carries no franchise_id must be
// refused, never quietly treated as ISP-wide — that default would turn one
// bad token into full cross-partner visibility.
func TestFR_FRN_004_FranchiseRoleWithNoBinding_IsRefused(t *testing.T) {
	fr := &stubFranchises{records: []revenue.FranchiseRecord{{ID: 1, Name: "A"}, {ID: 2, Name: "B"}}}
	mux := newFranchiseMux(t, fr)

	for _, path := range []string{"/api/v1/franchises", "/api/v1/franchises/1/pnl"} {
		rec := doGET(t, mux, path, itFranchiseToken(t, "franchise_admin", 0))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403 for a token with no franchise binding, got %d", path, rec.Code)
		}
	}
}

// TestFR_FRN_004_FranchiseAdmin_ListSeesOnlyTheirOwn verifies the list route
// scopes from the token rather than returning every partner.
func TestFR_FRN_004_FranchiseAdmin_ListSeesOnlyTheirOwn(t *testing.T) {
	fr := &stubFranchises{records: []revenue.FranchiseRecord{
		{ID: 1, Name: "Partner One"}, {ID: 2, Name: "Partner Two"}, {ID: 3, Name: "Partner Three"},
	}}
	mux := newFranchiseMux(t, fr)

	rec := doGET(t, mux, "/api/v1/franchises", itFranchiseToken(t, "franchise_admin", 2))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	var got []revenue.FranchiseRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].ID != 2 {
		t.Errorf("franchise_admin saw %+v, want only partner 2", got)
	}
	// The scope must have been pushed into the query, not filtered after the
	// fact — a store that returned everything and a handler that trimmed the
	// slice would pass the assertion above while still reading other
	// partners' rows out of the database.
	if !fr.scopeWasSet || fr.lastScope == nil || *fr.lastScope != 2 {
		t.Errorf("store received scope %v, want a non-nil scope of 2", fr.lastScope)
	}
}

// TestFR_FRN_003_ConsolidatedPnL_RefusedToFranchiseRoles: a consolidated view
// is ISP-wide by definition. Scoping it down to one partner would answer a
// different question while looking like the right one, so it is refused.
func TestFR_FRN_003_ConsolidatedPnL_RefusedToFranchiseRoles(t *testing.T) {
	mux := newFranchiseMux(t, &stubFranchises{})

	for _, role := range []string{"lco", "franchise_admin", "franchise_staff"} {
		rec := doGET(t, mux, "/api/v1/franchises/consolidated-pnl", itFranchiseToken(t, role, 1))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403 on consolidated P&L, got %d", role, rec.Code)
		}
	}
}

func TestFR_FRN_003_ConsolidatedPnL_AllowedToBillingAdmin(t *testing.T) {
	mux := newFranchiseMux(t, &stubFranchises{})

	rec := doGET(t, mux, "/api/v1/franchises/consolidated-pnl", itRoleToken(t, "billing_admin", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var got revenue.ConsolidatedPnL
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NetToISP != "900.00" {
		t.Errorf("net_to_isp = %q, want 900.00", got.NetToISP)
	}
}

// TestFR_FRN_004_ISPWideStaff_SeeEveryPartner confirms the scoping does not
// accidentally restrict the roles it should not.
func TestFR_FRN_004_ISPWideStaff_SeeEveryPartner(t *testing.T) {
	fr := &stubFranchises{records: []revenue.FranchiseRecord{{ID: 1}, {ID: 2}, {ID: 3}}}
	mux := newFranchiseMux(t, fr)

	rec := doGET(t, mux, "/api/v1/franchises", itRoleToken(t, "billing_admin", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got []revenue.FranchiseRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("billing_admin saw %d partners, want 3", len(got))
	}
	if fr.lastScope != nil {
		t.Errorf("store received scope %v, want nil (unrestricted) for ISP-wide staff", *fr.lastScope)
	}
}

// TestFR_FRN_004_RolesWithNoFranchiseBusiness_AreRefused: csr and technician
// have no franchise remit, and the route must say so rather than relying on
// nobody thinking to try.
func TestFR_FRN_004_RolesWithNoFranchiseBusiness_AreRefused(t *testing.T) {
	mux := newFranchiseMux(t, &stubFranchises{})

	for _, role := range []string{"csr", "technician", "noc_engineer"} {
		for _, path := range []string{
			"/api/v1/franchises",
			"/api/v1/franchises/1/pnl",
			"/api/v1/franchises/consolidated-pnl",
		} {
			rec := doGET(t, mux, path, itRoleToken(t, role, false))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s on %s: want 403, got %d", role, path, rec.Code)
			}
		}
	}
}

// ── Onboarding (FR-FRN-006) ──────────────────────────────────────────────────

func TestFR_FRN_006_CreateFranchise_Onboards(t *testing.T) {
	mux := newFranchiseMux(t, &stubFranchises{})

	body := `{"name":"Chennai North","owner_name":"R Kumar","mobile_number":"+919876500000","commission_rate_pct":"12.50"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/franchises", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	var got revenue.FranchiseRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "Chennai North" || got.Status != "active" {
		t.Errorf("created: %+v", got)
	}
}

// TestFR_FRN_006_CreateFranchise_RejectsImpossibleCommission: the column is
// NUMERIC(5,2) and would happily store 150, but a commission above 100% pays
// a partner more than the subscriber paid.
func TestFR_FRN_006_CreateFranchise_RejectsImpossibleCommission(t *testing.T) {
	mux := newFranchiseMux(t, &stubFranchises{})

	for _, rate := range []string{"150", "-5", "not-a-number"} {
		body := `{"name":"X","owner_name":"Y","mobile_number":"+919876500000","commission_rate_pct":"` + rate + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/franchises", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("rate %q: want 422, got %d", rate, rec.Code)
		}
	}
}

// TestFR_FRN_006_CreateFranchise_RefusedToFranchiseRoles: a partner must not
// be able to onboard other partners.
func TestFR_FRN_006_CreateFranchise_RefusedToFranchiseRoles(t *testing.T) {
	mux := newFranchiseMux(t, &stubFranchises{})

	body := `{"name":"X","owner_name":"Y","mobile_number":"+919876500000","commission_rate_pct":"10"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/franchises", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itFranchiseToken(t, "franchise_admin", 1))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d", rec.Code)
	}
}

// ── Misc ─────────────────────────────────────────────────────────────────────

func TestFR_FRN_003_UnknownFranchise_Returns404(t *testing.T) {
	mux := newFranchiseMux(t, &stubFranchises{pnl: nil})

	rec := doGET(t, mux, "/api/v1/franchises/999/pnl", itRoleToken(t, "billing_admin", false))
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
}

// TestFR_FRN_003_MalformedDateWindow_IsRejected: silently ignoring an
// unparseable ?from= would report the entire history as though it were the
// requested window — a wrong answer that looks like a right one.
func TestFR_FRN_003_MalformedDateWindow_IsRejected(t *testing.T) {
	mux := newFranchiseMux(t, &stubFranchises{pnl: &revenue.FranchisePnL{}})

	for _, q := range []string{"?from=yesterday", "?to=13-08-2026", "?from=2026-08-13&to=2026-08-01"} {
		rec := doGET(t, mux, "/api/v1/franchises/1/pnl"+q, itRoleToken(t, "billing_admin", false))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: want 422, got %d", q, rec.Code)
		}
	}
}

func TestFranchiseRoutes_UnconfiguredStoreReturns503(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	rec := doGET(t, mux, "/api/v1/franchises", itRoleToken(t, "billing_admin", false))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when the franchise store is not configured, got %d", rec.Code)
	}
}
