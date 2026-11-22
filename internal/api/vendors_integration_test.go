//go:build integration

// The shared vendor master's HTTP surface — FR-INV-004.
//
// Two things here are worth defending more than the CRUD: that the
// compatibility path which still accepts a vendor *name* resolves it against
// the master and refuses an unknown one rather than creating it (the moment
// free text would come back), and that a retired vendor cannot be named on a
// new purchase even though every existing purchase still shows it.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// ── Stub master ──────────────────────────────────────────────────────────────

// stubVendors is an in-memory VendorStore. It enforces the two unique rules
// the real partial indexes enforce, keyed exactly as they are — a stub that
// accepted duplicates would make the conflict tests pass against an
// implementation that could never behave that way.
type stubVendors struct {
	mu     sync.Mutex
	byID   map[int]*vendors.Vendor
	nextID int
	// resolveCalls counts ResolveVendorByName, so a test can tell "the name
	// was resolved against the master" from "the name was taken on trust".
	resolveCalls int
}

func newStubVendors() *stubVendors {
	return &stubVendors{byID: map[int]*vendors.Vendor{}, nextID: 1}
}

// seed adds a vendor without going through validation, for arranging a test.
func (s *stubVendors) seed(v vendors.Vendor) *vendors.Vendor {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.ID = s.nextID
	s.nextID++
	if !v.Active {
		v.Active = v.ID != 0 && v.Active
	}
	s.byID[v.ID] = &v
	return &v
}

func (s *stubVendors) seedActive(v vendors.Vendor) *vendors.Vendor {
	v.Active = true
	return s.seed(v)
}

// conflict mirrors uq_vendors_name_active and uq_vendors_gstin_active: both
// are partial on active, so a retired row never blocks a new one.
func (s *stubVendors) conflict(v vendors.Vendor, excludeID int) error {
	for id, existing := range s.byID {
		if id == excludeID || !existing.Active {
			continue
		}
		if vendors.NormalisedKey(existing.Name) == vendors.NormalisedKey(v.Name) {
			return vendors.ErrNameTaken
		}
		if v.GSTIN != "" && existing.GSTIN == v.GSTIN {
			return vendors.ErrGSTINTaken
		}
	}
	return nil
}

func (s *stubVendors) CreateVendor(_ context.Context, v vendors.Vendor, _ string) (*vendors.Vendor, error) {
	v.Normalise()
	if err := v.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conflict(v, 0); err != nil {
		return nil, err
	}
	v.ID = s.nextID
	s.nextID++
	v.Active = true
	s.byID[v.ID] = &v
	return &v, nil
}

func (s *stubVendors) UpdateVendor(_ context.Context, id int, v vendors.Vendor, _ string) (*vendors.Vendor, error) {
	v.Normalise()
	if err := v.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.byID[id]
	if !ok {
		return nil, vendors.ErrNotFound
	}
	if err := s.conflict(v, id); err != nil {
		return nil, err
	}
	v.ID, v.Active = id, existing.Active
	s.byID[id] = &v
	return &v, nil
}

func (s *stubVendors) SetVendorActive(_ context.Context, id int, active bool, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.byID[id]
	if !ok {
		return vendors.ErrNotFound
	}
	v.Active = active
	return nil
}

func (s *stubVendors) GetVendor(_ context.Context, id int) (*vendors.Vendor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.byID[id]
	if !ok {
		return nil, vendors.ErrNotFound
	}
	copied := *v
	return &copied, nil
}

func (s *stubVendors) ListVendors(_ context.Context) ([]vendors.VendorWithUsage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]vendors.VendorWithUsage, 0, len(s.byID))
	for _, v := range s.byID {
		out = append(out, vendors.VendorWithUsage{Vendor: *v, Usage: vendors.Usage{SpendStr: "0.00"}})
	}
	return out, nil
}

func (s *stubVendors) ListActiveVendors(_ context.Context) ([]vendors.Vendor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]vendors.Vendor, 0, len(s.byID))
	for _, v := range s.byID {
		if v.Active {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (s *stubVendors) ResolveVendorByName(_ context.Context, name string) (*vendors.Vendor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolveCalls++
	key := vendors.NormalisedKey(name)
	if key == "" {
		return nil, vendors.ErrNotFound
	}
	for _, v := range s.byID {
		if v.Active && vendors.NormalisedKey(v.Name) == key {
			copied := *v
			return &copied, nil
		}
	}
	return nil, vendors.ErrNotFound
}

// ── Harness ──────────────────────────────────────────────────────────────────

func vendorMux(t *testing.T, store api.VendorStore, inv api.InventoryQuerier) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{},
		Vendors:   store,
		Inventory: inv,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func vendorDo(t *testing.T, mux *http.ServeMux, method, path, body, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, role+"_user"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ── Tests ────────────────────────────────────────────────────────────────────

func TestFR_INV_004_OnlyOwnerAndBillingReachTheMaster(t *testing.T) {
	store := newStubVendors()
	store.seedActive(vendors.Vendor{Name: "Chennai Networks"})
	mux := vendorMux(t, store, nil)

	t.Run("allowed", func(t *testing.T) {
		for _, role := range []string{"isp_owner", "billing_admin"} {
			if rec := vendorDo(t, mux, "GET", "/api/v1/vendors", "", role); rec.Code != http.StatusOK {
				t.Errorf("%s got %d reading the master, want 200", role, rec.Code)
			}
		}
	})

	// Reads are gated as tightly as writes, deliberately: a vendor row is a
	// contact person, a phone number and a GSTIN, which a technician issuing
	// a router has no reason to hold.
	t.Run("refused, reads included", func(t *testing.T) {
		for _, role := range []string{"csr", "technician", "noc_engineer"} {
			if rec := vendorDo(t, mux, "GET", "/api/v1/vendors", "", role); rec.Code != http.StatusForbidden {
				t.Errorf("%s got %d reading the master, want 403", role, rec.Code)
			}
			body := `{"name":"Sneaky Supplies"}`
			if rec := vendorDo(t, mux, "POST", "/api/v1/vendors", body, role); rec.Code != http.StatusForbidden {
				t.Errorf("%s got %d creating a vendor, want 403", role, rec.Code)
			}
		}
	})
}

func TestFR_INV_004_CreatingAVendorReportsWhatIsWrongWithIt(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
		says string
	}{
		{"a full vendor", `{"name":"Chennai Networks","gstin":"27AAPFU0939F1ZV","phone":"044-2812 3456 x14"}`, 201, ""},
		{"a name alone is enough", `{"name":"Anna Nagar Cables"}`, 201, ""},
		{"no name", `{"name":"  "}`, 422, "vendor name is required"},
		// The two GSTIN failures must read differently: "that is not a GSTIN"
		// and "that GSTIN has a typo in it" send the caller to different
		// places.
		{"a GSTIN of the wrong shape", `{"name":"X","gstin":"NOPE"}`, 422, "15 characters"},
		{"a GSTIN with a mistyped character", `{"name":"X","gstin":"27AAPFU0939F1ZA"}`, 422, "check digit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := vendorMux(t, newStubVendors(), nil)
			rec := vendorDo(t, mux, "POST", "/api/v1/vendors", tc.body, "billing_admin")
			if rec.Code != tc.code {
				t.Fatalf("got %d, want %d — %s", rec.Code, tc.code, rec.Body.String())
			}
			if tc.says != "" && !strings.Contains(rec.Body.String(), tc.says) {
				t.Errorf("the message does not mention %q: %s", tc.says, rec.Body.String())
			}
		})
	}
}

// A duplicate name and a duplicate GSTIN are both 409 but must not read the
// same: a shared name is usually two people adding the same supplier, while a
// shared GSTIN means one legal entity is in the master twice under two
// spellings — which is the duplicate worth merging.
func TestFR_INV_004_TheTwoKindsOfDuplicateReadDifferently(t *testing.T) {
	store := newStubVendors()
	store.seedActive(vendors.Vendor{Name: "Chennai Networks", GSTIN: "27AAPFU0939F1ZV"})
	mux := vendorMux(t, store, nil)

	byName := vendorDo(t, mux, "POST", "/api/v1/vendors",
		`{"name":"chennai   networks"}`, "billing_admin")
	if byName.Code != http.StatusConflict {
		t.Errorf("a case- and whitespace-different name got %d, want 409 — %s",
			byName.Code, byName.Body.String())
	}

	byGSTIN := vendorDo(t, mux, "POST", "/api/v1/vendors",
		`{"name":"Chennai Netwerks Private Limited","gstin":"27AAPFU0939F1ZV"}`, "billing_admin")
	if byGSTIN.Code != http.StatusConflict {
		t.Fatalf("a duplicate GSTIN got %d, want 409 — %s", byGSTIN.Code, byGSTIN.Body.String())
	}
	if !strings.Contains(byGSTIN.Body.String(), "GSTIN") {
		t.Error("a GSTIN clash does not say so, so the operator cannot tell it from a name clash")
	}
	if byName.Body.String() == byGSTIN.Body.String() {
		t.Error("the two kinds of duplicate are worded identically")
	}
}

// Retiring, not deleting. There is no DELETE route at all: a vendor's name and
// GSTIN are what its invoices were issued under.
func TestFR_INV_004_AVendorIsRetiredAndNeverDeleted(t *testing.T) {
	store := newStubVendors()
	v := store.seedActive(vendors.Vendor{Name: "Chennai Networks"})
	mux := vendorMux(t, store, nil)

	if rec := vendorDo(t, mux, "DELETE", "/api/v1/vendors/1", "", "isp_owner"); rec.Code == http.StatusNoContent || rec.Code == http.StatusOK {
		t.Errorf("DELETE on a vendor succeeded with %d — the row must outlive the relationship", rec.Code)
	}

	rec := vendorDo(t, mux, "POST", "/api/v1/vendors/1/active", `{"active":false}`, "isp_owner")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("retiring got %d — %s", rec.Code, rec.Body.String())
	}
	got, err := store.GetVendor(context.Background(), v.ID)
	if err != nil || got.Active {
		t.Errorf("the vendor is still active after being retired (%v)", err)
	}

	// And it is still there to be read, which is the whole difference from a
	// delete: the GSTIN on last year's purchase invoices stays answerable.
	if rec := vendorDo(t, mux, "GET", "/api/v1/vendors/1", "", "isp_owner"); rec.Code != http.StatusOK {
		t.Errorf("a retired vendor reads back %d, want 200", rec.Code)
	}
}

// An omitted "active" must not default to false, or an empty body retires a
// vendor.
func TestFR_INV_004_AnOmittedActiveFlagRetiresNothing(t *testing.T) {
	store := newStubVendors()
	store.seedActive(vendors.Vendor{Name: "Chennai Networks"})
	mux := vendorMux(t, store, nil)

	rec := vendorDo(t, mux, "POST", "/api/v1/vendors/1/active", `{}`, "isp_owner")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("an empty body got %d, want 422 — %s", rec.Code, rec.Body.String())
	}
	got, _ := store.GetVendor(context.Background(), 1)
	if !got.Active {
		t.Error("an empty body retired the vendor")
	}
}

func TestFR_INV_004_TheActiveListIsThePickableShape(t *testing.T) {
	store := newStubVendors()
	store.seedActive(vendors.Vendor{Name: "Chennai Networks"})
	store.seed(vendors.Vendor{Name: "Retired Cables"}) // Active defaults false
	mux := vendorMux(t, store, nil)

	rec := vendorDo(t, mux, "GET", "/api/v1/vendors?active=true", "", "billing_admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d — %s", rec.Code, rec.Body.String())
	}
	var pickable []vendors.Vendor
	if err := json.Unmarshal(rec.Body.Bytes(), &pickable); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(pickable) != 1 || pickable[0].Name != "Chennai Networks" {
		t.Fatalf("the pickable list is %+v, want the one active vendor", pickable)
	}

	// The unfiltered list carries usage, which is what makes it useful for
	// finding the duplicates the backfill could not merge — and it includes
	// the retired row, because that is where those duplicates are.
	full := vendorDo(t, mux, "GET", "/api/v1/vendors", "", "billing_admin")
	var withUsage []vendors.VendorWithUsage
	if err := json.Unmarshal(full.Body.Bytes(), &withUsage); err != nil {
		t.Fatalf("decode: %v — %s", err, full.Body.String())
	}
	if len(withUsage) != 2 {
		t.Errorf("the full list has %d rows, want 2 including the retired one", len(withUsage))
	}
}

// The GSTIN is audited as a flag, never as a value. Asserted rather than
// assumed, because the comment that says so is otherwise the only thing
// stopping a tax identifier being copied into a second table.
func TestFR_INV_004_TheAuditTrailRecordsThatAGSTINWasEnteredNotWhatItIs(t *testing.T) {
	sink := &recordingAuditSink{}
	middleware.SetAuditSink(sink)
	t.Cleanup(func() { middleware.SetAuditSink(nil) })

	mux := vendorMux(t, newStubVendors(), nil)
	const gstin = "27AAPFU0939F1ZV"
	rec := vendorDo(t, mux, "POST", "/api/v1/vendors",
		`{"name":"Chennai Networks","gstin":"`+gstin+`"}`, "billing_admin")
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d — %s", rec.Code, rec.Body.String())
	}

	entries := sink.entries()
	if len(entries) == 0 {
		t.Fatal("creating a vendor was not audited at all")
	}
	e := entries[len(entries)-1]
	if e.Action != "vendor.created" {
		t.Errorf("audited as %q, want vendor.created", e.Action)
	}
	if e.Detail["has_gstin"] != true {
		t.Errorf("the entry does not record that a GSTIN was entered: %+v", e.Detail)
	}
	for k, v := range e.Detail {
		if s, ok := v.(string); ok && strings.Contains(s, gstin) {
			t.Errorf("the GSTIN itself was written into the audit detail under %q", k)
		}
	}
	if e.ActorID != "billing_admin_user" {
		t.Errorf("audited against actor %q — the trail has to name who entered it", e.ActorID)
	}
}

// ── The compatibility path ───────────────────────────────────────────────────

// A client written against the pre-master API still sends a vendor name. It
// keeps working, but only as a lookup: the name is resolved against the
// master and an unknown one is refused, never created.
func TestFR_INV_004_ALegacyVendorNameIsResolvedNotTrusted(t *testing.T) {
	store := newStubVendors()
	chennai := store.seedActive(vendors.Vendor{Name: "Chennai Networks"})
	inv := newStubInventoryForVendors()
	mux := vendorMux(t, store, inv)

	t.Run("a known name resolves to its master id", func(t *testing.T) {
		body := `{"device_type_id":1,"vendor":"chennai   NETWORKS","quantity":10,"unit_cost":"1250.00"}`
		rec := vendorDo(t, mux, "POST", "/api/v1/cpe/purchases", body, "billing_admin")
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d — %s", rec.Code, rec.Body.String())
		}
		if store.resolveCalls == 0 {
			t.Error("the name was never resolved against the master, so it was taken on trust")
		}
		if inv.lastPurchase == nil || inv.lastPurchase.VendorID == nil ||
			*inv.lastPurchase.VendorID != chennai.ID {
			t.Errorf("the purchase reached the store with vendor %v, want id %d",
				inv.lastPurchase, chennai.ID)
		}
	})

	t.Run("an unknown name is refused, not created", func(t *testing.T) {
		before := len(store.byID)
		body := `{"device_type_id":1,"vendor":"Chennai Netwoks","quantity":10,"unit_cost":"1250.00"}`
		rec := vendorDo(t, mux, "POST", "/api/v1/cpe/purchases", body, "billing_admin")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("a typo'd vendor name got %d, want 422 — %s", rec.Code, rec.Body.String())
		}
		if len(store.byID) != before {
			t.Error("an unrecognised name created a vendor, which is the duplication the master exists to end")
		}
		if !strings.Contains(rec.Body.String(), "vendor master") {
			t.Errorf("the refusal does not say where to add it: %s", rec.Body.String())
		}
	})

	t.Run("no vendor at all is refused", func(t *testing.T) {
		body := `{"device_type_id":1,"quantity":10,"unit_cost":"1250.00"}`
		rec := vendorDo(t, mux, "POST", "/api/v1/cpe/purchases", body, "billing_admin")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("got %d, want 422 — %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "vendor_id") {
			t.Errorf("the refusal does not name the field to send: %s", rec.Body.String())
		}
	})

	// vendor_id wins when both are sent: the id is the real field, and
	// resolving the name as well would make a disagreement between them
	// decide silently in the name's favour.
	t.Run("vendor_id takes precedence over a name", func(t *testing.T) {
		other := store.seedActive(vendors.Vendor{Name: "Anna Nagar Cables"})
		store.resolveCalls = 0
		body := `{"device_type_id":1,"vendor_id":` + strconv.Itoa(other.ID) +
			`,"vendor":"Chennai Networks","quantity":1,"unit_cost":"10.00"}`
		rec := vendorDo(t, mux, "POST", "/api/v1/cpe/purchases", body, "billing_admin")
		if rec.Code != http.StatusCreated {
			t.Fatalf("got %d — %s", rec.Code, rec.Body.String())
		}
		if store.resolveCalls != 0 {
			t.Error("the name was resolved even though vendor_id was supplied")
		}
		if *inv.lastPurchase.VendorID != other.ID {
			t.Errorf("the purchase used vendor %d, want the vendor_id %d",
				*inv.lastPurchase.VendorID, other.ID)
		}
	})
}

// A retired vendor cannot be named on a new purchase. The store is what
// refuses it (its insert selects only an active vendor), so this proves the
// API turns that into something the caller can act on rather than a 500.
func TestFR_INV_004_ARetiredVendorIsRefusedOnANewPurchase(t *testing.T) {
	store := newStubVendors()
	store.seedActive(vendors.Vendor{Name: "Chennai Networks"})
	inv := newStubInventoryForVendors()
	inv.vendorErr = vendors.ErrInactive
	mux := vendorMux(t, store, inv)

	body := `{"device_type_id":1,"vendor_id":1,"quantity":10,"unit_cost":"1250.00"}`
	rec := vendorDo(t, mux, "POST", "/api/v1/cpe/purchases", body, "billing_admin")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422 — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "retired") {
		t.Errorf("the refusal does not say the vendor was retired: %s", rec.Body.String())
	}
}

func TestFR_INV_004_AnUnwiredMasterSaysSo(t *testing.T) {
	mux := vendorMux(t, nil, nil)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/v1/vendors", ""},
		{"POST", "/api/v1/vendors", `{"name":"X"}`},
		{"GET", "/api/v1/vendors/1", ""},
		{"PATCH", "/api/v1/vendors/1", `{"name":"X"}`},
		{"POST", "/api/v1/vendors/1/active", `{"active":false}`},
	} {
		rec := vendorDo(t, mux, c.method, c.path, c.body, "isp_owner")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s got %d, want 503 — %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

// ── Shared stubs ─────────────────────────────────────────────────────────────

// recordingAuditSink captures audit entries so a test can assert on what the
// trail actually holds rather than on the comment that says what it should.
type recordingAuditSink struct {
	mu   sync.Mutex
	seen []middleware.AuditEntry
}

func (s *recordingAuditSink) RecordAudit(_ context.Context, e middleware.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, e)
	return nil
}

func (s *recordingAuditSink) entries() []middleware.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]middleware.AuditEntry(nil), s.seen...)
}

// stubInventoryForVendors is an InventoryQuerier that records what reached it
// and can fail the way the real store fails on a retired vendor. Only the two
// vendor-carrying methods do anything; the rest satisfy the interface.
type stubInventoryForVendors struct {
	lastPurchase   *inventory.Purchase
	lastDeviceType *inventory.DeviceType
	// vendorErr, when set, is what the write paths return — standing in for
	// the real store's "the insert selected no active vendor" result.
	vendorErr error
}

func newStubInventoryForVendors() *stubInventoryForVendors { return &stubInventoryForVendors{} }

func (s *stubInventoryForVendors) RecordPurchase(_ context.Context, p inventory.Purchase) (*inventory.Purchase, error) {
	if s.vendorErr != nil {
		return nil, s.vendorErr
	}
	p.ID = 1
	p.UnitCostStr, p.TotalCostStr = "0.00", "0.00"
	s.lastPurchase = &p
	return &p, nil
}

func (s *stubInventoryForVendors) CreateDeviceType(_ context.Context, t inventory.DeviceType) (*inventory.DeviceType, error) {
	if s.vendorErr != nil {
		return nil, s.vendorErr
	}
	t.ID = 1
	s.lastDeviceType = &t
	return &t, nil
}

func (s *stubInventoryForVendors) ListDeviceTypes(context.Context) ([]inventory.DeviceType, error) {
	return nil, nil
}
func (s *stubInventoryForVendors) CreateDevice(context.Context, inventory.Device) (*inventory.Device, error) {
	return nil, nil
}
func (s *stubInventoryForVendors) GetDeviceBySerial(context.Context, string) (*inventory.Device, error) {
	return nil, nil
}
func (s *stubInventoryForVendors) ListDevices(context.Context, *string, *int, *int) ([]inventory.Device, error) {
	return nil, nil
}
func (s *stubInventoryForVendors) IssueDevice(context.Context, string, int) (*inventory.Device, error) {
	return nil, nil
}
func (s *stubInventoryForVendors) ReturnDevice(context.Context, string, string) (*inventory.Device, error) {
	return nil, nil
}
func (s *stubInventoryForVendors) GetStockLevels(context.Context, bool) ([]inventory.StockLevel, error) {
	return nil, nil
}
func (s *stubInventoryForVendors) ListPurchases(context.Context, *int) ([]inventory.Purchase, error) {
	return nil, nil
}
