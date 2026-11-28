//go:build integration

// The CPE inventory endpoints' validation and failure paths — FR-INV-001,
// FR-INV-003, FR-INV-004 | MDS §4.16.
//
// crm_inventory_integration_test.go covers the onboarding handoff and the
// issue/return round trip. What it left (inventory.go at 42%): creating
// device types and devices, every list filter, and what each endpoint says
// when the store fails. The filters matter most. optionalQueryInt exists
// because a malformed filter that was silently dropped would answer a
// different question while looking correct — "devices of type 3" returning
// every device — and nothing tested it.
package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

var errInvStore = errors.New("store: cpe tables unreachable")

// recInventory records what the handlers pass to the store, and fails on
// demand. Everything else is the shared in-memory stub.
type recInventory struct {
	*stubInventory

	gotType       inventory.DeviceType
	gotStatus     *string
	gotTypeFilter *int
	gotSubscriber *int
	gotLowOnly    bool
	gotPurchase   inventory.Purchase
	gotPurchaseBy *int

	createTypeErr, createDeviceErr, listErr, lookupErr, returnErr, purchaseErr error
}

func (r *recInventory) CreateDeviceType(ctx context.Context, t inventory.DeviceType) (*inventory.DeviceType, error) {
	r.gotType = t
	if r.createTypeErr != nil {
		return nil, r.createTypeErr
	}
	return r.stubInventory.CreateDeviceType(ctx, t)
}

func (r *recInventory) ListDeviceTypes(ctx context.Context) ([]inventory.DeviceType, error) {
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.stubInventory.ListDeviceTypes(ctx)
}

func (r *recInventory) CreateDevice(ctx context.Context, d inventory.Device) (*inventory.Device, error) {
	if r.createDeviceErr != nil {
		return nil, r.createDeviceErr
	}
	return r.stubInventory.CreateDevice(ctx, d)
}

func (r *recInventory) ListDevices(ctx context.Context, status *string, typeID, subscriberID *int) ([]inventory.Device, error) {
	r.gotStatus, r.gotTypeFilter, r.gotSubscriber = status, typeID, subscriberID
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.stubInventory.ListDevices(ctx, status, typeID, subscriberID)
}

func (r *recInventory) GetDeviceBySerial(ctx context.Context, serial string) (*inventory.Device, error) {
	if r.lookupErr != nil {
		return nil, r.lookupErr
	}
	return r.stubInventory.GetDeviceBySerial(ctx, serial)
}

func (r *recInventory) ReturnDevice(ctx context.Context, serial, status string) (*inventory.Device, error) {
	if r.returnErr != nil {
		return nil, r.returnErr
	}
	return r.stubInventory.ReturnDevice(ctx, serial, status)
}

func (r *recInventory) GetStockLevels(ctx context.Context, lowOnly bool) ([]inventory.StockLevel, error) {
	r.gotLowOnly = lowOnly
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.stubInventory.GetStockLevels(ctx, lowOnly)
}

func (r *recInventory) RecordPurchase(ctx context.Context, p inventory.Purchase) (*inventory.Purchase, error) {
	r.gotPurchase = p
	if r.purchaseErr != nil {
		return nil, r.purchaseErr
	}
	return r.stubInventory.RecordPurchase(ctx, p)
}

func (r *recInventory) ListPurchases(ctx context.Context, typeID *int) ([]inventory.Purchase, error) {
	r.gotPurchaseBy = typeID
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.stubInventory.ListPurchases(ctx, typeID)
}

type invHarness struct {
	crmHarness
	rec    *recInventory
	ingram *vendors.Vendor
}

func newInvHarness(t *testing.T) *invHarness {
	t.Helper()
	h := &invHarness{rec: &recInventory{stubInventory: newStubInventory()}}
	h.vendors = newStubVendors()
	h.ingram = h.vendors.seedActive(vendors.Vendor{Name: "Ingram"})
	handler := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Inventory: h.rec, Vendors: h.vendors,
	})
	h.mux = http.NewServeMux()
	handler.RegisterRoutes(h.mux, itJWTSecret)
	return h
}

func (h *invHarness) owner(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	rec := h.do(t, method, path, body, "isp_owner", "owner1")
	return rec.Code, rec.Body.String()
}

func TestFR_INV_001_CreateDeviceType(t *testing.T) {
	h := newInvHarness(t)

	code, body := h.owner(t, http.MethodPost, "/api/v1/cpe/types", `{"name":"Archer C6","vendor":"ingram"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	// The name is resolved against the master, not stored as free text.
	if h.rec.gotType.VendorID == nil || *h.rec.gotType.VendorID != h.ingram.ID {
		t.Errorf("vendor reached the store as %v, want the master's id %d", h.rec.gotType.VendorID, h.ingram.ID)
	}
	if h.rec.gotType.ReorderThreshold != 5 {
		t.Errorf("default threshold = %d, want the column default 5", h.rec.gotType.ReorderThreshold)
	}

	// Zero is a real threshold ("alert only when we have none"), not a
	// missing one.
	if code, body := h.owner(t, http.MethodPost, "/api/v1/cpe/types",
		`{"name":"ONT","vendor_id":1,"reorder_threshold":0}`); code != http.StatusCreated {
		t.Fatalf("threshold 0: %d %s", code, body)
	}
	if h.rec.gotType.ReorderThreshold != 0 {
		t.Errorf("an explicit 0 became %d", h.rec.gotType.ReorderThreshold)
	}
}

func TestFR_INV_001_CreateDeviceTypeRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		err  error
		code int
		want string
	}{
		"malformed":          {`{"name":`, nil, http.StatusBadRequest, ""},
		"no name":            {`{"vendor_id":1}`, nil, http.StatusUnprocessableEntity, "name is required"},
		"no vendor":          {`{"name":"x"}`, nil, http.StatusUnprocessableEntity, "vendor_id is required"},
		"unknown vendor":     {`{"name":"x","vendor":"Nobody Ltd"}`, nil, http.StatusUnprocessableEntity, "no active vendor"},
		"negative threshold": {`{"name":"x","vendor_id":1,"reorder_threshold":-1}`, nil, http.StatusUnprocessableEntity, "must not be negative"},
		"duplicate":          {`{"name":"x","vendor_id":1}`, errors.New("duplicate key value violates unique constraint"), http.StatusConflict, "already exists"},
		"retired vendor":     {`{"name":"x","vendor_id":1}`, vendors.ErrInactive, http.StatusUnprocessableEntity, ""},
		"store down":         {`{"name":"x","vendor_id":1}`, errInvStore, http.StatusInternalServerError, "create device type failed"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newInvHarness(t)
			h.rec.createTypeErr = tc.err
			code, body := h.owner(t, http.MethodPost, "/api/v1/cpe/types", tc.body)
			if code != tc.code || !strings.Contains(body, tc.want) {
				t.Errorf("got %d %s, want %d containing %q", code, body, tc.code, tc.want)
			}
			if strings.Contains(body, errInvStore.Error()) {
				t.Error("the store's own error reached the client")
			}
		})
	}
}

func TestFR_INV_001_CreateDevice(t *testing.T) {
	h := newInvHarness(t)
	code, body := h.owner(t, http.MethodPost, "/api/v1/cpe/devices",
		`{"device_type_id":1,"serial_number":"SN-9","mac_address":"AA:BB:CC:00:11:22"}`)
	if code != http.StatusCreated || !strings.Contains(body, `"in_stock"`) {
		t.Fatalf("create: %d %s", code, body)
	}
	if code, _ := h.owner(t, http.MethodPost, "/api/v1/cpe/devices",
		`{"device_type_id":1,"serial_number":"SN-9"}`); code != http.StatusConflict {
		t.Errorf("a duplicate serial: %d, want 409", code)
	}

	for name, tc := range map[string]struct {
		body string
		err  error
		code int
	}{
		"malformed": {`[`, nil, http.StatusBadRequest},
		"no serial": {`{"device_type_id":1}`, nil, http.StatusUnprocessableEntity},
		"no type":   {`{"serial_number":"SN-1"}`, nil, http.StatusUnprocessableEntity},
		"bad type":  {`{"serial_number":"SN-1","device_type_id":-2}`, nil, http.StatusUnprocessableEntity},
		"store":     {`{"serial_number":"SN-1","device_type_id":1}`, errInvStore, http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			h := newInvHarness(t)
			h.rec.createDeviceErr = tc.err
			if code, body := h.owner(t, http.MethodPost, "/api/v1/cpe/devices", tc.body); code != tc.code {
				t.Errorf("got %d %s, want %d", code, body, tc.code)
			}
		})
	}
}

// Each filter reaches the store as given, and a malformed one is refused by
// name rather than dropped.
func TestFR_INV_001_DeviceFiltersReachTheStore(t *testing.T) {
	h := newInvHarness(t)
	code, body := h.owner(t, http.MethodGet, "/api/v1/cpe/devices?status=issued&device_type_id=3&subscriber_id=7", "")
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("got %d %s; an empty result is [] not null", code, body)
	}
	if h.rec.gotStatus == nil || *h.rec.gotStatus != "issued" ||
		h.rec.gotTypeFilter == nil || *h.rec.gotTypeFilter != 3 ||
		h.rec.gotSubscriber == nil || *h.rec.gotSubscriber != 7 {
		t.Errorf("filters reached the store as status=%v type=%v subscriber=%v",
			h.rec.gotStatus, h.rec.gotTypeFilter, h.rec.gotSubscriber)
	}

	// No filter is nil, not zero: "type 0" and "any type" are different
	// questions.
	h.owner(t, http.MethodGet, "/api/v1/cpe/devices", "")
	if h.rec.gotStatus != nil || h.rec.gotTypeFilter != nil || h.rec.gotSubscriber != nil {
		t.Error("an absent filter reached the store as a value")
	}

	for _, q := range []string{"device_type_id=three", "subscriber_id=7x"} {
		h.rec.gotTypeFilter, h.rec.gotSubscriber = nil, nil
		code, body := h.owner(t, http.MethodGet, "/api/v1/cpe/devices?"+q, "")
		param, _, _ := strings.Cut(q, "=")
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, param) {
			t.Errorf("%s: got %d %s; a malformed filter must be refused by name, not dropped", q, code, body)
		}
	}

	if code, _ := h.owner(t, http.MethodGet, "/api/v1/cpe/purchases?device_type_id=2", ""); code != http.StatusOK ||
		h.rec.gotPurchaseBy == nil || *h.rec.gotPurchaseBy != 2 {
		t.Errorf("purchases filter: %d, reached the store as %v", code, h.rec.gotPurchaseBy)
	}
	if code, _ := h.owner(t, http.MethodGet, "/api/v1/cpe/purchases?device_type_id=x", ""); code != http.StatusUnprocessableEntity {
		t.Errorf("a malformed purchases filter: %d", code)
	}

	h.owner(t, http.MethodGet, "/api/v1/cpe/stock?low_only=true", "")
	if !h.rec.gotLowOnly {
		t.Error("low_only=true did not reach the store")
	}
	h.owner(t, http.MethodGet, "/api/v1/cpe/stock?low_only=yes", "")
	if h.rec.gotLowOnly {
		t.Error("only the literal true narrows the list")
	}
}

// Every list answers [] for nothing and a generic 500 for a store failure.
func TestFR_INV_001_ListsOnEmptyAndOnFailure(t *testing.T) {
	paths := []string{"/api/v1/cpe/types", "/api/v1/cpe/devices", "/api/v1/cpe/stock", "/api/v1/cpe/purchases"}
	h := newInvHarness(t)
	for _, p := range paths {
		if code, body := h.owner(t, http.MethodGet, p, ""); code != http.StatusOK || strings.TrimSpace(body) != "[]" {
			t.Errorf("%s empty: %d %s", p, code, body)
		}
	}
	h.rec.listErr = errInvStore
	for _, p := range paths {
		code, body := h.owner(t, http.MethodGet, p, "")
		if code != http.StatusInternalServerError || strings.Contains(body, errInvStore.Error()) {
			t.Errorf("%s failing: %d %s", p, code, body)
		}
	}
}

func TestFR_INV_002_IssueAndReturnRefusals(t *testing.T) {
	h := newInvHarness(t)
	h.rec.seedDevice("SN-1")

	for name, tc := range map[string]struct {
		path, body string
		setup      func()
		code       int
	}{
		"issue malformed":      {"/api/v1/cpe/devices/SN-1/issue", `{`, nil, http.StatusBadRequest},
		"issue no subscriber":  {"/api/v1/cpe/devices/SN-1/issue", `{}`, nil, http.StatusUnprocessableEntity},
		"issue lookup fails":   {"/api/v1/cpe/devices/SN-1/issue", `{"subscriber_id":7}`, func() { h.rec.lookupErr = errInvStore }, http.StatusInternalServerError},
		"issue claim fails":    {"/api/v1/cpe/devices/SN-1/issue", `{"subscriber_id":7}`, func() { h.rec.issueErr = errInvStore }, http.StatusInternalServerError},
		"return malformed":     {"/api/v1/cpe/devices/SN-1/return", `{`, nil, http.StatusBadRequest},
		"return not issued":    {"/api/v1/cpe/devices/SN-1/return", `{}`, nil, http.StatusConflict},
		"return to the shelf":  {"/api/v1/cpe/devices/SN-1/return", `{"status":"in_stock"}`, nil, http.StatusUnprocessableEntity},
		"return store failure": {"/api/v1/cpe/devices/SN-1/return", `{"status":"faulty"}`, func() { h.rec.returnErr = errInvStore }, http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			h.rec.lookupErr, h.rec.issueErr, h.rec.returnErr = nil, nil, nil
			if tc.setup != nil {
				tc.setup()
			}
			if code, body := h.owner(t, http.MethodPost, tc.path, tc.body); code != tc.code {
				t.Errorf("got %d %s, want %d", code, body, tc.code)
			}
		})
	}

	// A return with no status goes to "returned", pending inspection.
	h.rec.lookupErr, h.rec.issueErr, h.rec.returnErr = nil, nil, nil
	h.owner(t, http.MethodPost, "/api/v1/cpe/devices/SN-1/issue", `{"subscriber_id":7}`)
	code, body := h.owner(t, http.MethodPost, "/api/v1/cpe/devices/SN-1/return", `{}`)
	if code != http.StatusOK || !strings.Contains(body, `"returned"`) {
		t.Errorf("a bare return: %d %s", code, body)
	}
}

func TestFR_INV_003_PurchaseValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		err  error
		code int
	}{
		"malformed":         {`{`, nil, http.StatusBadRequest},
		"no type":           {`{"vendor_id":1,"quantity":1,"unit_cost":"10"}`, nil, http.StatusUnprocessableEntity},
		"no vendor":         {`{"device_type_id":1,"quantity":1,"unit_cost":"10"}`, nil, http.StatusUnprocessableEntity},
		"zero quantity":     {`{"device_type_id":1,"vendor_id":1,"quantity":0,"unit_cost":"10"}`, nil, http.StatusUnprocessableEntity},
		"negative cost":     {`{"device_type_id":1,"vendor_id":1,"quantity":1,"unit_cost":"-1"}`, nil, http.StatusUnprocessableEntity},
		"cost not a number": {`{"device_type_id":1,"vendor_id":1,"quantity":1,"unit_cost":"ten"}`, nil, http.StatusUnprocessableEntity},
		"retired vendor":    {`{"device_type_id":1,"vendor_id":1,"quantity":1,"unit_cost":"10"}`, vendors.ErrInactive, http.StatusUnprocessableEntity},
		"store down":        {`{"device_type_id":1,"vendor_id":1,"quantity":1,"unit_cost":"10"}`, errInvStore, http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			h := newInvHarness(t)
			h.rec.purchaseErr = tc.err
			if code, body := h.owner(t, http.MethodPost, "/api/v1/cpe/purchases", tc.body); code != tc.code {
				t.Errorf("got %d %s, want %d", code, body, tc.code)
			}
		})
	}

	// The purchaser is whoever's token it is, not a field in the body.
	h := newInvHarness(t)
	code, body := h.owner(t, http.MethodPost, "/api/v1/cpe/purchases",
		`{"device_type_id":1,"vendor_id":1,"quantity":2,"unit_cost":"0","purchased_by":"someone_else"}`)
	if code != http.StatusCreated {
		t.Fatalf("a free purchase (cost 0) was refused: %d %s", code, body)
	}
	if h.rec.gotPurchase.PurchasedBy != "owner1" {
		t.Errorf("purchased_by = %q; it must come from the token", h.rec.gotPurchase.PurchasedBy)
	}
}
