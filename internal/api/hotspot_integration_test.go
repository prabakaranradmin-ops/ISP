//go:build integration

// Staff hotspot administration — FR-HSP-001..002 | MDS §4.23.
//
// Route wiring, authorisation and response shape for the endpoints that issue
// and revoke hotspot access. The persistence behind them is covered in
// internal/db/hotspot_integration_test.go; what matters here is that a printed
// voucher code is returned exactly once and never again, that the role tiers
// hold, and that a MAC is stored in the spelling the RADIUS side looks up.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// ── Stub ────────────────────────────────────────────────────────────────────

type stubHotspot struct {
	mu sync.Mutex

	created  []hotspot.NewVoucher
	listed   []hotspot.Voucher
	filter   hotspot.VoucherFilter
	devices  []stubDeviceReg
	voided   []int
	voidable bool

	createErr error
}

type stubDeviceReg struct {
	MAC          string
	SubscriberID int
	Label        string
	NASID        *int
}

func (s *stubHotspot) CreateVoucher(_ context.Context, v hotspot.NewVoucher) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return 0, s.createErr
	}
	s.created = append(s.created, v)
	return len(s.created), nil
}

func (s *stubHotspot) ListVouchers(_ context.Context, f hotspot.VoucherFilter) ([]hotspot.Voucher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filter = f
	return s.listed, nil
}

func (s *stubHotspot) VoidVoucher(_ context.Context, id int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.voided = append(s.voided, id)
	return s.voidable, nil
}

func (s *stubHotspot) RegisterDevice(_ context.Context, mac string, subscriberID int, label string, nasID *int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices = append(s.devices, stubDeviceReg{mac, subscriberID, label, nasID})
	return len(s.devices), nil
}

func (s *stubHotspot) DeactivateDevice(_ context.Context, mac string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return mac == "AA:BB:CC:DD:EE:FF", nil
}

// GetVoucherCommissionSummary backs the reseller-settlement endpoint
// (CRD-EXP-010). Fixed figures rather than a running total over the vouchers
// this stub recorded: the endpoint's own test asserts the handler returns
// what the store gave it, which a computed value would make circular.
func (s *stubHotspot) GetVoucherCommissionSummary(_ context.Context, franchiseID int) (*hotspot.VoucherCommissionSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &hotspot.VoucherCommissionSummary{
		FranchiseID:     franchiseID,
		VoucherCount:    3,
		TotalSales:      "300.00",
		TotalCommission: "30.00",
	}, nil
}

func (s *stubHotspot) snapshotCreated() []hotspot.NewVoucher {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]hotspot.NewVoucher(nil), s.created...)
}

func (s *stubHotspot) snapshotDevices() []stubDeviceReg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubDeviceReg(nil), s.devices...)
}

// ── Harness ─────────────────────────────────────────────────────────────────

func hotspotMux(store api.HotspotQuerier) *http.ServeMux {
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Hotspot: store})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

// hotspotStaffToken mints a token shaped like the ones the staff console
// actually issues — role *and* subject.
//
// itRoleToken omits the subject, which is harmless for the routes it was
// written for but not here: created_by on a voucher batch is the record of who
// printed prepaid service, and a subject-less token would leave every assertion
// about attribution passing against an empty string.
func hotspotStaffToken(t *testing.T, role string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   role + "@ops",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign %s token: %v", role, err)
	}
	return tok
}

func hotspotCall(t *testing.T, mux *http.ServeMux, method, path, role, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)) //nolint:noctx // httptest.NewRequestWithContext needs go1.23; module is go1.22
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("Authorization", "Bearer "+hotspotStaffToken(t, role))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ── Voucher issuance ────────────────────────────────────────────────────────

// TestFR_HSP_001_VoucherBatchReturnsEveryCodeExactlyOnce is the property the
// whole hashing scheme rests on: the plaintext exists in this one response and
// nowhere else. If it could be re-read later, hashing the codes would buy
// nothing.
func TestFR_HSP_001_VoucherBatchReturnsEveryCodeExactlyOnce(t *testing.T) {
	store := &stubHotspot{}
	mux := hotspotMux(store)

	rec := hotspotCall(t, mux, http.MethodPost, "/api/v1/hotspot/vouchers", "billing_admin",
		`{"plan_id":3,"count":5,"duration_minutes":120,"valid_for_days":30,"batch_ref":"cafe-june"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		BatchRef string   `json:"batch_ref"`
		Count    int      `json:"count"`
		Codes    []string `json:"codes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 5 || len(resp.Codes) != 5 {
		t.Fatalf("want 5 codes, got count=%d len=%d", resp.Count, len(resp.Codes))
	}
	if resp.BatchRef != "cafe-june" {
		t.Errorf("batch_ref: want cafe-june, got %q", resp.BatchRef)
	}

	// Distinct codes — a batch where two vouchers share a code is a batch where
	// redeeming one silently spends the other.
	seen := map[string]bool{}
	for _, c := range resp.Codes {
		if seen[c] {
			t.Fatalf("duplicate code in one batch: %q", c)
		}
		seen[c] = true
	}

	created := store.snapshotCreated()
	if len(created) != 5 {
		t.Fatalf("want 5 stored vouchers, got %d", len(created))
	}
	for i, v := range created {
		if v.PlanID != 3 || v.DurationMinutes != 120 {
			t.Errorf("voucher %d: plan/duration not carried through: %+v", i, v)
		}
		if v.ExpiresAt == nil {
			t.Errorf("voucher %d: valid_for_days must set a shelf life on the printed code", i)
		}
		if v.CreatedBy == "" {
			t.Errorf("voucher %d: created_by must record who issued it", i)
		}
		// The store must receive the hash and the prefix — never the code.
		if v.CodeHash == "" || v.CodePrefix == "" {
			t.Errorf("voucher %d: missing hash or prefix: %+v", i, v)
		}
		for _, plaintext := range resp.Codes {
			if v.CodeHash == plaintext {
				t.Errorf("voucher %d: the plaintext code reached storage — it must be hashed", i)
			}
		}
	}

	// Each stored hash must correspond to one issued code, or the printed sheet
	// and the database disagree about what will redeem.
	for _, plaintext := range resp.Codes {
		found := false
		for _, v := range created {
			if v.CodeHash == hotspot.HashCode(plaintext) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("issued code %q has no matching stored hash", plaintext)
		}
	}

	// Listing afterwards must not hand the codes back.
	store.listed = []hotspot.Voucher{{ID: 1, CodePrefix: "HS-ABCD", Status: "unused"}}
	list := hotspotCall(t, mux, http.MethodGet, "/api/v1/hotspot/vouchers?batch_ref=cafe-june", "csr", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list: want 200, got %d", list.Code)
	}
	for _, plaintext := range resp.Codes {
		if bytes.Contains(list.Body.Bytes(), []byte(plaintext)) {
			t.Fatalf("a voucher listing returned the plaintext code %q — the code must be "+
				"unrecoverable after issuance", plaintext)
		}
	}
	if store.filter.BatchRef != "cafe-june" {
		t.Errorf("batch_ref filter must reach the store, got %q", store.filter.BatchRef)
	}
}

func TestFR_HSP_001_VoucherBatchValidation(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"no plan", `{"count":1,"duration_minutes":60}`, http.StatusUnprocessableEntity},
		{"zero count", `{"plan_id":1,"count":0,"duration_minutes":60}`, http.StatusUnprocessableEntity},
		{"absurd count", `{"plan_id":1,"count":100000,"duration_minutes":60}`, http.StatusUnprocessableEntity},
		{"no duration", `{"plan_id":1,"count":1}`, http.StatusUnprocessableEntity},
		{"negative cap", `{"plan_id":1,"count":1,"duration_minutes":60,"data_cap_bytes":-1}`, http.StatusUnprocessableEntity},
		{"malformed", `{`, http.StatusBadRequest},
		{"valid", `{"plan_id":1,"count":1,"duration_minutes":60}`, http.StatusCreated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubHotspot{}
			rec := hotspotCall(t, hotspotMux(store), http.MethodPost,
				"/api/v1/hotspot/vouchers", "billing_admin", tc.body)
			if rec.Code != tc.want {
				t.Errorf("want %d, got %d — %s", tc.want, rec.Code, rec.Body.String())
			}
			if tc.want != http.StatusCreated && len(store.snapshotCreated()) != 0 {
				t.Error("a rejected batch must not store any vouchers")
			}
		})
	}
}

// TestFR_HSP_001_VoucherBatchGetsABatchRefEvenWhenUnnamed — "which sheet did
// this come from" is the first question asked when printed vouchers go
// missing, so the answer cannot be optional.
func TestFR_HSP_001_VoucherBatchGetsABatchRefEvenWhenUnnamed(t *testing.T) {
	store := &stubHotspot{}
	rec := hotspotCall(t, hotspotMux(store), http.MethodPost, "/api/v1/hotspot/vouchers",
		"billing_admin", `{"plan_id":1,"count":2,"duration_minutes":60}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d", rec.Code)
	}
	for _, v := range store.snapshotCreated() {
		if v.BatchRef == "" {
			t.Error("every voucher must carry a batch reference, generated when the caller omits one")
		}
	}
}

// ── Authorisation ───────────────────────────────────────────────────────────

// TestFR_HSP_001_HotspotRoleTiers pins who can do what. Issuing vouchers is
// prepaid service and sits with billing; registering a customer's phone is
// support work and sits with csr/technician. A subscriber token reaches none
// of it.
func TestFR_HSP_001_HotspotRoleTiers(t *testing.T) {
	tests := []struct {
		name, method, path, role, body string
		wantAllowed                    bool
	}{
		{"owner issues", http.MethodPost, "/api/v1/hotspot/vouchers", "isp_owner", `{"plan_id":1,"count":1,"duration_minutes":60}`, true},
		{"billing issues", http.MethodPost, "/api/v1/hotspot/vouchers", "billing_admin", `{"plan_id":1,"count":1,"duration_minutes":60}`, true},
		{"csr cannot issue", http.MethodPost, "/api/v1/hotspot/vouchers", "csr", `{"plan_id":1,"count":1,"duration_minutes":60}`, false},
		{"technician cannot issue", http.MethodPost, "/api/v1/hotspot/vouchers", "technician", `{"plan_id":1,"count":1,"duration_minutes":60}`, false},
		{"subscriber cannot issue", http.MethodPost, "/api/v1/hotspot/vouchers", "subscriber", `{"plan_id":1,"count":1,"duration_minutes":60}`, false},

		{"csr lists", http.MethodGet, "/api/v1/hotspot/vouchers", "csr", "", true},
		{"noc lists", http.MethodGet, "/api/v1/hotspot/vouchers", "noc_engineer", "", true},
		{"subscriber cannot list", http.MethodGet, "/api/v1/hotspot/vouchers", "subscriber", "", false},

		{"csr registers a MAC", http.MethodPost, "/api/v1/hotspot/devices", "csr", `{"mac_address":"AA:BB:CC:DD:EE:FF","subscriber_id":1}`, true},
		{"technician registers a MAC", http.MethodPost, "/api/v1/hotspot/devices", "technician", `{"mac_address":"AA:BB:CC:DD:EE:FF","subscriber_id":1}`, true},
		{"subscriber cannot register a MAC", http.MethodPost, "/api/v1/hotspot/devices", "subscriber", `{"mac_address":"AA:BB:CC:DD:EE:FF","subscriber_id":1}`, false},
		{"noc cannot register a MAC", http.MethodPost, "/api/v1/hotspot/devices", "noc_engineer", `{"mac_address":"AA:BB:CC:DD:EE:FF","subscriber_id":1}`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := hotspotCall(t, hotspotMux(&stubHotspot{voidable: true}), tc.method, tc.path, tc.role, tc.body)
			denied := rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden
			if tc.wantAllowed && denied {
				t.Errorf("%s must be permitted, got %d — %s", tc.role, rec.Code, rec.Body.String())
			}
			if !tc.wantAllowed && !denied {
				t.Errorf("%s must be refused, got %d — %s", tc.role, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestFR_HSP_001_HotspotRoutesRequireAToken — no anonymous reach at all. The
// captive portal is the only unauthenticated hotspot surface, and it lives in
// a different package precisely so this one stays closed.
func TestFR_HSP_001_HotspotRoutesRequireAToken(t *testing.T) {
	mux := hotspotMux(&stubHotspot{})
	for _, path := range []string{"/api/v1/hotspot/vouchers", "/api/v1/hotspot/devices"} {
		rec := hotspotCall(t, mux, http.MethodPost, path, "", `{"plan_id":1,"count":1,"duration_minutes":60}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a token: want 401, got %d", path, rec.Code)
		}
	}
	rec := hotspotCall(t, mux, http.MethodGet, "/api/v1/hotspot/vouchers", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("listing without a token: want 401, got %d", rec.Code)
	}
}

// ── MAC registration ────────────────────────────────────────────────────────

// TestFR_HSP_002_RegisteredMACIsNormalised is what keeps the admin API and the
// RADIUS daemon agreeing on what a device is called. Registered in one
// spelling and looked up in another, a device is refused for reasons invisible
// from the console.
func TestFR_HSP_002_RegisteredMACIsNormalised(t *testing.T) {
	for _, spelling := range []string{
		"AA:BB:CC:DD:EE:FF",
		"aa:bb:cc:dd:ee:ff",
		"aa-bb-cc-dd-ee-ff",
		"aabb.ccdd.eeff",
		"aabbccddeeff",
	} {
		store := &stubHotspot{}
		rec := hotspotCall(t, hotspotMux(store), http.MethodPost, "/api/v1/hotspot/devices",
			"csr", `{"mac_address":"`+spelling+`","subscriber_id":9,"label":"phone"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s: want 201, got %d — %s", spelling, rec.Code, rec.Body.String())
		}
		devices := store.snapshotDevices()
		if len(devices) != 1 || devices[0].MAC != "AA:BB:CC:DD:EE:FF" {
			t.Errorf("%s must normalise to AA:BB:CC:DD:EE:FF, got %+v", spelling, devices)
		}
		if devices[0].SubscriberID != 9 {
			t.Errorf("subscriber_id must be carried through, got %d", devices[0].SubscriberID)
		}
	}
}

func TestFR_HSP_002_BadDeviceRegistrationIsRefused(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"not a MAC", `{"mac_address":"hello","subscriber_id":1}`, http.StatusUnprocessableEntity},
		{"too short", `{"mac_address":"AA:BB:CC","subscriber_id":1}`, http.StatusUnprocessableEntity},
		{"no subscriber", `{"mac_address":"AA:BB:CC:DD:EE:FF"}`, http.StatusUnprocessableEntity},
		{"malformed body", `{`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubHotspot{}
			rec := hotspotCall(t, hotspotMux(store), http.MethodPost, "/api/v1/hotspot/devices", "csr", tc.body)
			if rec.Code != tc.want {
				t.Errorf("want %d, got %d — %s", tc.want, rec.Code, rec.Body.String())
			}
			if len(store.snapshotDevices()) != 0 {
				t.Error("an invalid registration must not reach the store")
			}
		})
	}
}

func TestFR_HSP_002_DeviceDeactivationReportsHonestly(t *testing.T) {
	mux := hotspotMux(&stubHotspot{})

	// The stub deactivates only this MAC; a lost phone that was registered
	// comes back 200.
	if rec := hotspotCall(t, mux, http.MethodDelete,
		"/api/v1/hotspot/devices/aa-bb-cc-dd-ee-ff", "technician", ""); rec.Code != http.StatusOK {
		t.Errorf("deactivating a registered device: want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	// One that was never registered must not report success — an operator who
	// believes they revoked a device that is still live is worse off than one
	// who is told it was not found.
	if rec := hotspotCall(t, mux, http.MethodDelete,
		"/api/v1/hotspot/devices/11:22:33:44:55:66", "technician", ""); rec.Code != http.StatusNotFound {
		t.Errorf("deactivating an unknown device: want 404, got %d", rec.Code)
	}
	if rec := hotspotCall(t, mux, http.MethodDelete,
		"/api/v1/hotspot/devices/not-a-mac", "technician", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("deactivating a junk MAC: want 400, got %d", rec.Code)
	}
}

// ── Voiding ─────────────────────────────────────────────────────────────────

func TestFR_HSP_001_VoidingReportsWhetherItLanded(t *testing.T) {
	unused := hotspotCall(t, hotspotMux(&stubHotspot{voidable: true}), http.MethodDelete,
		"/api/v1/hotspot/vouchers/7", "billing_admin", "")
	if unused.Code != http.StatusOK {
		t.Errorf("voiding an unused voucher: want 200, got %d", unused.Code)
	}

	// Already redeemed (or already void): reported as a conflict rather than
	// success, so an operator is not told they pulled a code out of circulation
	// when a live grant is still running behind it.
	spent := hotspotCall(t, hotspotMux(&stubHotspot{voidable: false}), http.MethodDelete,
		"/api/v1/hotspot/vouchers/7", "billing_admin", "")
	if spent.Code != http.StatusConflict {
		t.Errorf("voiding a redeemed voucher: want 409, got %d — %s", spent.Code, spent.Body.String())
	}
}

// ── Unconfigured deployment ─────────────────────────────────────────────────

// TestFR_HSP_001_HotspotDegradesTo503WhenUnconfigured matches every other
// optional dependency in this package: the route exists, its store does not,
// and the process still serves everything else.
func TestFR_HSP_001_HotspotDegradesTo503WhenUnconfigured(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	for _, tc := range []struct{ method, path, role, body string }{
		{http.MethodPost, "/api/v1/hotspot/vouchers", "billing_admin", `{"plan_id":1,"count":1,"duration_minutes":60}`},
		{http.MethodGet, "/api/v1/hotspot/vouchers", "csr", ""},
		{http.MethodDelete, "/api/v1/hotspot/vouchers/1", "billing_admin", ""},
		{http.MethodPost, "/api/v1/hotspot/devices", "csr", `{"mac_address":"AA:BB:CC:DD:EE:FF","subscriber_id":1}`},
		{http.MethodDelete, "/api/v1/hotspot/devices/AA:BB:CC:DD:EE:FF", "csr", ""},
	} {
		rec := hotspotCall(t, mux, tc.method, tc.path, tc.role, tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s unconfigured: want 503, got %d", tc.method, tc.path, rec.Code)
		}
	}
}
