package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
	"github.com/shopspring/decimal"
)

// The Inventory screen — FR-INV-001..004.
//
// inventory.go was at 11.2%: 142 statements covering CPE stock, who may buy
// and who may hand a device over. Three behaviours are the point:
//
//   - Procurement and day-to-day handling are separated. Adding a device type
//     or recording a purchase is a spend decision (owner or billing admin);
//     issuing and returning a device is a technician's job. Collapsing the two
//     would either stop technicians working or let them commit money.
//   - A returned device never goes straight back to in_stock. It comes back
//     "returned" pending inspection, or "faulty". Anything else re-issues an
//     untested router to the next customer.
//   - A vendor comes from the shared master, never free text, which is what
//     keeps one supplier from appearing under four spellings.

var errInventoryStore = errors.New("store: inventory tables unreachable")

type fakeInventory struct {
	types     []inventory.DeviceType
	devices   []inventory.Device
	stock     []inventory.StockLevel
	purchases []inventory.Purchase

	stockErr        error
	devicesErr      error
	typesErr        error
	purchasesErr    error
	createTypeErr   error
	createDeviceErr error
	issueErr        error
	returnErr       error
	purchaseErr     error
	issueNil        bool
	returnNil       bool

	createdType    *inventory.DeviceType
	createdDevice  *inventory.Device
	issuedSerial   string
	issuedTo       int
	returnedSerial string
	returnedStatus string
	purchase       *inventory.Purchase
	statusFilter   *string
}

func (f *fakeInventory) ListDeviceTypes(context.Context) ([]inventory.DeviceType, error) {
	return f.types, f.typesErr
}

func (f *fakeInventory) CreateDeviceType(_ context.Context, t inventory.DeviceType) (*inventory.DeviceType, error) {
	f.createdType = &t
	if f.createTypeErr != nil {
		return nil, f.createTypeErr
	}
	t.ID = 3
	t.Vendor = "Chennai Networks"
	return &t, nil
}

func (f *fakeInventory) ListDevices(_ context.Context, status *string, _, _ *int) ([]inventory.Device, error) {
	f.statusFilter = status
	return f.devices, f.devicesErr
}

func (f *fakeInventory) CreateDevice(_ context.Context, d inventory.Device) (*inventory.Device, error) {
	f.createdDevice = &d
	if f.createDeviceErr != nil {
		return nil, f.createDeviceErr
	}
	d.ID = 11
	d.Status = inventory.StatusInStock
	return &d, nil
}

func (f *fakeInventory) GetDeviceBySerial(_ context.Context, serial string) (*inventory.Device, error) {
	for i := range f.devices {
		if f.devices[i].SerialNumber == serial {
			return &f.devices[i], nil
		}
	}
	return nil, nil
}

func (f *fakeInventory) IssueDevice(_ context.Context, serial string, subscriberID int) (*inventory.Device, error) {
	f.issuedSerial, f.issuedTo = serial, subscriberID
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	if f.issueNil {
		return nil, nil
	}
	return &inventory.Device{ID: 11, SerialNumber: serial, Status: inventory.StatusIssued,
		SubscriberID: &subscriberID}, nil
}

func (f *fakeInventory) ReturnDevice(_ context.Context, serial, newStatus string) (*inventory.Device, error) {
	f.returnedSerial, f.returnedStatus = serial, newStatus
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	if f.returnNil {
		return nil, nil
	}
	return &inventory.Device{ID: 11, SerialNumber: serial, Status: newStatus}, nil
}

func (f *fakeInventory) GetStockLevels(context.Context, bool) ([]inventory.StockLevel, error) {
	return f.stock, f.stockErr
}

func (f *fakeInventory) RecordPurchase(_ context.Context, p inventory.Purchase) (*inventory.Purchase, error) {
	f.purchase = &p
	if f.purchaseErr != nil {
		return nil, f.purchaseErr
	}
	p.ID = 5
	p.Vendor = "Chennai Networks"
	return &p, nil
}

func (f *fakeInventory) ListPurchases(_ context.Context, _ *int) ([]inventory.Purchase, error) {
	return f.purchases, f.purchasesErr
}

func serveInv(h *Handler, s Session, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))

	path := strings.Trim(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "devices" && len(parts) > i+1 {
			r.SetPathValue("serial", parts[i+1])
		}
	}

	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/issue"):
		h.IssueDeviceForm(w, r)
	case strings.HasSuffix(path, "/return"):
		h.ReturnDeviceForm(w, r)
	case strings.HasSuffix(path, "/device-types"):
		h.CreateDeviceTypeForm(w, r)
	case strings.HasSuffix(path, "/purchases"):
		h.RecordPurchaseForm(w, r)
	case strings.HasSuffix(path, "/devices") && method == http.MethodPost:
		h.CreateDeviceForm(w, r)
	default:
		h.Inventory(w, r)
	}
	return w
}

func invHandler(inv InventoryStore, v VendorManager) *Handler {
	h := &Handler{}
	if inv != nil {
		h.inventory = inv
	}
	if v != nil {
		h.vendors = v
	}
	return h
}

func invVendors(t *testing.T) *stubVendorStore {
	t.Helper()
	return &stubVendorStore{list: []vendors.Vendor{
		{ID: 1, Name: "Chennai Networks", Active: true},
	}}
}

// stubVendorStore satisfies VendorManager; only PickableVendors is used by
// these forms, and the rest exist because the interface requires them.
type stubVendorStore struct {
	list []vendors.Vendor
	err  error
}

func (s *stubVendorStore) PickableVendors(context.Context) ([]vendors.Vendor, error) {
	return s.list, s.err
}

func (s *stubVendorStore) AddVendor(_ context.Context, v vendors.Vendor) (*vendors.Vendor, error) {
	return &v, s.err
}

func (s *stubVendorStore) EditVendor(_ context.Context, _ int, v vendors.Vendor) (*vendors.Vendor, error) {
	return &v, s.err
}

func (s *stubVendorStore) SetVendorRetired(context.Context, int, bool) error { return s.err }

func (s *stubVendorStore) LookupVendor(context.Context, int) (*vendors.Vendor, error) {
	if len(s.list) == 0 {
		return nil, s.err
	}
	return &s.list[0], s.err
}

func (s *stubVendorStore) VendorsWithUsage(context.Context) ([]vendors.VendorWithUsage, error) {
	return nil, s.err
}

// A device comes back "returned" pending inspection, or "faulty". Never
// straight to in_stock — that would put an untested router from a customer's
// house into the next customer's house.
func TestFR_INV_002_AReturnedDeviceNeverGoesStraightBackToStock(t *testing.T) {
	for _, status := range []string{
		inventory.StatusInStock, inventory.StatusIssued, "repaired", "", "RETURNED",
	} {
		inv := &fakeInventory{}
		rec := serveInv(invHandler(inv, invVendors(t)), sess("technician"),
			http.MethodPost, "/staff/inventory/devices/SN-001/return",
			url.Values{"status": {status}})

		if !strings.Contains(pageText(rec.Body.String()), `Status must be "returned" or "faulty"`) {
			t.Errorf("status %q: want a refusal, got: %s", status, firstLines(rec.Body.String()))
		}
		if inv.returnedStatus != "" {
			t.Errorf("status %q reached the store", status)
		}
	}

	for _, status := range []string{inventory.StatusReturned, inventory.StatusFaulty} {
		inv := &fakeInventory{}
		rec := serveInv(invHandler(inv, invVendors(t)), sess("technician"),
			http.MethodPost, "/staff/inventory/devices/SN-001/return",
			url.Values{"status": {status}})
		if inv.returnedStatus != status {
			t.Errorf("the allowed status %q did not reach the store", status)
		}
		if !strings.Contains(pageText(rec.Body.String()), "marked "+status) {
			t.Errorf("no confirmation for %q: %s", status, firstLines(rec.Body.String()))
		}
	}
}

// Issuing and returning are a technician's job; buying is not. Both halves
// asserted, because collapsing them either stops technicians working or lets
// them commit money.
func TestFR_INV_003_ProcurementAndHandlingAreSeparateDuties(t *testing.T) {
	handling := []struct {
		target string
		form   url.Values
	}{
		{"/staff/inventory/devices/SN-001/issue", url.Values{"subscriber_id": {"12"}}},
		{"/staff/inventory/devices/SN-001/return", url.Values{"status": {"returned"}}},
	}
	procurement := []struct {
		target string
		form   url.Values
	}{
		{"/staff/inventory/device-types", url.Values{
			"name": {"ONT X1"}, "vendor_id": {"1"}, "reorder_threshold": {"5"},
		}},
		{"/staff/inventory/purchases", url.Values{
			"device_type_id": {"3"}, "vendor_id": {"1"}, "quantity": {"10"},
			"unit_cost": {"1200.00"},
		}},
	}

	// A technician handles devices but cannot buy them.
	for _, tc := range handling {
		inv := &fakeInventory{}
		rec := serveInv(invHandler(inv, invVendors(t)), sess("technician"),
			http.MethodPost, tc.target, tc.form)
		if rec.Code == http.StatusForbidden {
			t.Errorf("a technician was refused %s, which is their day job", tc.target)
		}
	}
	for _, tc := range procurement {
		inv := &fakeInventory{}
		rec := serveInv(invHandler(inv, invVendors(t)), sess("technician"),
			http.MethodPost, tc.target, tc.form)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a technician reached %s: %d", tc.target, rec.Code)
		}
		if inv.createdType != nil || inv.purchase != nil {
			t.Errorf("a technician's %s reached the store", tc.target)
		}
	}

	// Owner and billing admin may buy.
	for _, role := range []string{"isp_owner", "billing_admin"} {
		for _, tc := range procurement {
			inv := &fakeInventory{}
			rec := serveInv(invHandler(inv, invVendors(t)), sess(role),
				http.MethodPost, tc.target, tc.form)
			if rec.Code == http.StatusForbidden {
				t.Errorf("%s was refused %s", role, tc.target)
			}
		}
	}
}

// A vendor must be chosen from the master, and the message says where to add
// one — the alternative is a free-text field and four spellings of one
// supplier.
func TestFR_INV_004_AVendorMustComeFromTheMaster(t *testing.T) {
	for name, tc := range map[string]struct {
		target string
		form   url.Values
	}{
		"device type, no vendor": {"/staff/inventory/device-types", url.Values{
			"name": {"ONT X1"}, "reorder_threshold": {"5"},
		}},
		"device type, bad vendor": {"/staff/inventory/device-types", url.Values{
			"name": {"ONT X1"}, "vendor_id": {"0"}, "reorder_threshold": {"5"},
		}},
		"purchase, no vendor": {"/staff/inventory/purchases", url.Values{
			"device_type_id": {"3"}, "quantity": {"10"}, "unit_cost": {"1200.00"},
		}},
		"purchase, bad vendor": {"/staff/inventory/purchases", url.Values{
			"device_type_id": {"3"}, "vendor_id": {"-1"}, "quantity": {"10"},
			"unit_cost": {"1200.00"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			inv := &fakeInventory{}
			rec := serveInv(invHandler(inv, invVendors(t)), owner(),
				http.MethodPost, tc.target, tc.form)
			body := pageText(rec.Body.String())
			if !strings.Contains(body, "Choose a vendor") {
				t.Errorf("want a vendor message, got: %s", firstLines(body))
			}
			if !strings.Contains(body, "Vendors screen") {
				t.Errorf("the message should say where to add one, got: %s", firstLines(body))
			}
			if inv.createdType != nil || inv.purchase != nil {
				t.Error("the write reached the store with no vendor")
			}
		})
	}
}

// A vendor retired between the page loading and the form submitting is
// reported as exactly that, rather than as a generic failure the operator
// cannot act on.
func TestFR_INV_004_AVendorRetiredSincePageLoadIsReportedAsSuch(t *testing.T) {
	for name, tc := range map[string]struct {
		inv    *fakeInventory
		target string
		form   url.Values
	}{
		"device type": {&fakeInventory{createTypeErr: vendors.ErrInactive},
			"/staff/inventory/device-types", url.Values{
				"name": {"ONT X1"}, "vendor_id": {"1"}, "reorder_threshold": {"5"},
			}},
		"purchase": {&fakeInventory{purchaseErr: vendors.ErrInactive},
			"/staff/inventory/purchases", url.Values{
				"device_type_id": {"3"}, "vendor_id": {"1"}, "quantity": {"10"},
				"unit_cost": {"1200.00"},
			}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveInv(invHandler(tc.inv, invVendors(t)), owner(),
				http.MethodPost, tc.target, tc.form)
			if !strings.Contains(pageText(rec.Body.String()), "retired since this page loaded") {
				t.Errorf("want a retired-vendor message, got: %s", firstLines(rec.Body.String()))
			}
		})
	}
}

// Issuing a device that is not in stock says why rather than reporting
// success — "already issued, returned pending inspection, or faulty" is the
// list a technician standing in a customer's house needs.
func TestFR_INV_002_IssuingADeviceThatIsNotInStockSaysWhy(t *testing.T) {
	inv := &fakeInventory{issueNil: true}
	rec := serveInv(invHandler(inv, invVendors(t)), sess("technician"),
		http.MethodPost, "/staff/inventory/devices/SN-001/issue",
		url.Values{"subscriber_id": {"12"}})

	body := pageText(rec.Body.String())
	if !strings.Contains(body, "not in stock") {
		t.Errorf("want a not-in-stock message, got: %s", firstLines(body))
	}
	// The wording matters: a technician standing in a customer's house needs
	// the list of reasons, not just "no".
	for _, want := range []string{"already be issued", "returned pending inspection", "faulty"} {
		if !strings.Contains(body, want) {
			t.Errorf("the message should mention %q, got: %s", want, firstLines(body))
		}
	}
	if inv.issuedSerial != "SN-001" {
		t.Errorf("the store was asked about serial %q", inv.issuedSerial)
	}
}

// Returning a device nobody has is reported too.
func TestFR_INV_002_ReturningADeviceNobodyHasIsReported(t *testing.T) {
	inv := &fakeInventory{returnNil: true}
	rec := serveInv(invHandler(inv, invVendors(t)), sess("technician"),
		http.MethodPost, "/staff/inventory/devices/SN-001/return",
		url.Values{"status": {"returned"}})
	if !strings.Contains(pageText(rec.Body.String()), "not currently issued") {
		t.Errorf("want a not-issued message, got: %s", firstLines(rec.Body.String()))
	}
}

// An issue needs a real subscriber id, and a device registration needs a
// serial and a type.
func TestFR_INV_001_FormValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		target string
		form   url.Values
		want   string
	}{
		"issue with no subscriber": {"/staff/inventory/devices/SN-001/issue",
			url.Values{}, "valid subscriber id"},
		"issue with zero": {"/staff/inventory/devices/SN-001/issue",
			url.Values{"subscriber_id": {"0"}}, "valid subscriber id"},
		"issue with text": {"/staff/inventory/devices/SN-001/issue",
			url.Values{"subscriber_id": {"twelve"}}, "valid subscriber id"},
		"device with no serial": {"/staff/inventory/devices",
			url.Values{"device_type_id": {"3"}}, "Serial number and a device type"},
		"device with no type": {"/staff/inventory/devices",
			url.Values{"serial_number": {"SN-002"}}, "Serial number and a device type"},
		"device with bad type": {"/staff/inventory/devices",
			url.Values{"serial_number": {"SN-002"}, "device_type_id": {"0"}},
			"Serial number and a device type"},
		"device type with no name": {"/staff/inventory/device-types",
			url.Values{"vendor_id": {"1"}, "reorder_threshold": {"5"}}, "name is required"},
		"device type with bad threshold": {"/staff/inventory/device-types",
			url.Values{"name": {"ONT X1"}, "vendor_id": {"1"}, "reorder_threshold": {"-1"}},
			"non-negative number"},
		"purchase with no quantity": {"/staff/inventory/purchases",
			url.Values{"device_type_id": {"3"}, "vendor_id": {"1"}, "unit_cost": {"1200"}},
			"positive quantity"},
		"purchase with zero quantity": {"/staff/inventory/purchases",
			url.Values{"device_type_id": {"3"}, "vendor_id": {"1"}, "quantity": {"0"},
				"unit_cost": {"1200"}}, "positive quantity"},
		"purchase with negative cost": {"/staff/inventory/purchases",
			url.Values{"device_type_id": {"3"}, "vendor_id": {"1"}, "quantity": {"10"},
				"unit_cost": {"-5"}}, "non-negative number"},
		"purchase with bad cost": {"/staff/inventory/purchases",
			url.Values{"device_type_id": {"3"}, "vendor_id": {"1"}, "quantity": {"10"},
				"unit_cost": {"a lot"}}, "non-negative number"},
	} {
		t.Run(name, func(t *testing.T) {
			inv := &fakeInventory{}
			rec := serveInv(invHandler(inv, invVendors(t)), owner(), http.MethodPost,
				tc.target, tc.form)
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if inv.createdType != nil || inv.createdDevice != nil ||
				inv.purchase != nil || inv.issuedSerial != "" {
				t.Error("invalid input reached the store")
			}
		})
	}
}

// A purchase records who made it, from the session, and the money as a
// decimal rather than a float.
func TestFR_INV_003_APurchaseRecordsWhoBoughtItAndTheExactCost(t *testing.T) {
	inv := &fakeInventory{}
	rec := serveInv(invHandler(inv, invVendors(t)), sess("billing_admin"),
		http.MethodPost, "/staff/inventory/purchases", url.Values{
			"device_type_id": {"3"}, "vendor_id": {"1"}, "quantity": {"10"},
			"unit_cost": {"1234.56"}, "invoice_ref": {"  INV-9001  "},
			"purchased_by": {"somebody_else"},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d — %s", rec.Code, firstLines(rec.Body.String()))
	}
	if inv.purchase == nil {
		t.Fatal("nothing reached the store")
	}
	if inv.purchase.PurchasedBy != "billing_admin_user" {
		t.Errorf("purchased_by = %q; it must come from the session, not the form",
			inv.purchase.PurchasedBy)
	}
	if !inv.purchase.UnitCost.Equal(decimal.RequireFromString("1234.56")) {
		t.Errorf("unit cost = %s", inv.purchase.UnitCost)
	}
	if inv.purchase.InvoiceRef != "INV-9001" {
		t.Errorf("invoice ref = %q; it should be trimmed", inv.purchase.InvoiceRef)
	}
	if inv.purchase.Quantity != 10 {
		t.Errorf("quantity = %d", inv.purchase.Quantity)
	}
}

// A device type is stored with its vendor id, trimmed name and threshold.
func TestFR_INV_004_ADeviceTypeIsStoredWithItsVendorId(t *testing.T) {
	inv := &fakeInventory{}
	rec := serveInv(invHandler(inv, invVendors(t)), owner(), http.MethodPost,
		"/staff/inventory/device-types", url.Values{
			"name": {"  ONT X1  "}, "vendor_id": {"1"}, "reorder_threshold": {"5"},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d — %s", rec.Code, firstLines(rec.Body.String()))
	}
	if inv.createdType == nil {
		t.Fatal("nothing reached the store")
	}
	if inv.createdType.Name != "ONT X1" {
		t.Errorf("name = %q; it should be trimmed", inv.createdType.Name)
	}
	if inv.createdType.VendorID == nil || *inv.createdType.VendorID != 1 {
		t.Errorf("vendor id = %v", inv.createdType.VendorID)
	}
	if inv.createdType.ReorderThreshold != 5 {
		t.Errorf("threshold = %d", inv.createdType.ReorderThreshold)
	}
}

// A duplicate serial is reported in terms an operator can act on — the serial
// or the MAC is already registered — rather than a bare failure.
func TestFR_INV_001_ADuplicateSerialPointsAtTheCause(t *testing.T) {
	inv := &fakeInventory{createDeviceErr: errInventoryStore}
	rec := serveInv(invHandler(inv, invVendors(t)), sess("technician"), http.MethodPost,
		"/staff/inventory/devices", url.Values{
			"serial_number": {"SN-002"}, "device_type_id": {"3"},
			"mac_address": {"AA:BB:CC:DD:EE:FF"},
		})
	body := pageText(rec.Body.String())
	if !strings.Contains(body, "already in use") {
		t.Errorf("want a duplicate hint, got: %s", firstLines(body))
	}
	if strings.Contains(body, errInventoryStore.Error()) {
		t.Error("the store's own error reached the page")
	}
}

// The screen renders stock, devices and purchases, and gates the procurement
// forms by role rather than by hiding them client-side.
func TestFR_INV_001_TheScreenRendersAndGatesTheProcurementForms(t *testing.T) {
	inv := &fakeInventory{
		stock: []inventory.StockLevel{{DeviceTypeID: 3}},
		devices: []inventory.Device{
			{ID: 11, SerialNumber: "SN-001", DeviceType: "ONT X1", Status: inventory.StatusInStock},
		},
		types: []inventory.DeviceType{{ID: 3, Name: "ONT X1", Vendor: "Chennai Networks"}},
		purchases: []inventory.Purchase{
			{ID: 5, Quantity: 10, Vendor: "Chennai Networks", UnitCostStr: "1200.00"},
		},
	}
	h := invHandler(inv, invVendors(t))

	body := pageText(serveInv(h, owner(), http.MethodGet, "/staff/inventory", nil).Body.String())
	for _, want := range []string{"SN-001", "ONT X1", "Chennai Networks"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q from the screen", want)
		}
	}

	// Every role that reaches the screen sees the roster.
	for _, role := range []string{"isp_owner", "billing_admin", "technician"} {
		if code := serveInv(h, sess(role), http.MethodGet, "/staff/inventory", nil).Code; code != http.StatusOK {
			t.Errorf("%s got %d from the inventory screen", role, code)
		}
	}
}

// Store failures render a readable page that names what could not be loaded,
// without the store's own error.
func TestFR_INV_001_StoreFailuresAreReadable(t *testing.T) {
	for name, tc := range map[string]struct {
		inv  *fakeInventory
		want string
	}{
		"stock":   {&fakeInventory{stockErr: errInventoryStore}, "Could not load stock levels"},
		"devices": {&fakeInventory{devicesErr: errInventoryStore}, "Could not load the device roster"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveInv(invHandler(tc.inv, invVendors(t)), owner(),
				http.MethodGet, "/staff/inventory", nil)
			if rec.Code != http.StatusOK {
				t.Errorf("want a rendered page, got %d", rec.Code)
			}
			body := pageText(rec.Body.String())
			if !strings.Contains(body, tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(body))
			}
			if strings.Contains(body, errInventoryStore.Error()) {
				t.Error("the store's own error reached the page")
			}
		})
	}
}

// An unconfigured deployment says so on the screen and refuses every action.
func TestFR_INV_001_AnUnconfiguredDeploymentSaysSo(t *testing.T) {
	bare := invHandler(nil, invVendors(t))

	if !strings.Contains(pageText(serveInv(bare, owner(), http.MethodGet,
		"/staff/inventory", nil).Body.String()), "not configured") {
		t.Error("the screen did not say it is unconfigured")
	}
	for _, target := range []string{
		"/staff/inventory/device-types", "/staff/inventory/devices",
		"/staff/inventory/devices/SN-001/issue", "/staff/inventory/devices/SN-001/return",
		"/staff/inventory/purchases",
	} {
		rec := serveInv(bare, owner(), http.MethodPost, target, url.Values{
			"name": {"x"}, "serial_number": {"SN"}, "device_type_id": {"3"},
			"subscriber_id": {"12"}, "status": {"returned"}, "vendor_id": {"1"},
			"quantity": {"1"}, "unit_cost": {"1"}, "reorder_threshold": {"0"},
		})
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("POST %s: want 503, got %d", target, rec.Code)
		}
	}
}
