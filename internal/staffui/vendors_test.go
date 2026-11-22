package staffui

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/procurement"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
)

// The shared vendor master in the console — FR-INV-004.
//
// The template is executed with real data in each shape it branches on: a Go
// template resolves field names at run time, so a wrong name compiles cleanly
// and fails only here. The vendor row has a GSTIN that may be absent, a
// contact that may be entirely absent, and a retired state — three branches
// that a compile could not see into.

type fakeVendors struct {
	list []vendors.VendorWithUsage
	one  *vendors.Vendor
	err  error

	added      vendors.Vendor
	editedID   int
	editedTo   vendors.Vendor
	retiredID  int
	retired    bool
	who        string
	calls      int
	pickCalled int
}

func (f *fakeVendors) AddVendor(ctx context.Context, v vendors.Vendor) (*vendors.Vendor, error) {
	f.calls++
	f.added, f.who = v, middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	v.ID, v.Active = 1, true
	return &v, nil
}

func (f *fakeVendors) EditVendor(ctx context.Context, id int, v vendors.Vendor) (*vendors.Vendor, error) {
	f.calls++
	f.editedID, f.editedTo, f.who = id, v, middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	v.ID = id
	return &v, nil
}

func (f *fakeVendors) SetVendorRetired(ctx context.Context, id int, retired bool) error {
	f.calls++
	f.retiredID, f.retired, f.who = id, retired, middleware.SubjectFromContext(ctx)
	return f.err
}

func (f *fakeVendors) LookupVendor(_ context.Context, id int) (*vendors.Vendor, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.one != nil && f.one.ID == id {
		return f.one, nil
	}
	return nil, vendors.ErrNotFound
}

func (f *fakeVendors) VendorsWithUsage(context.Context) ([]vendors.VendorWithUsage, error) {
	return f.list, f.err
}

func (f *fakeVendors) PickableVendors(context.Context) ([]vendors.Vendor, error) {
	f.pickCalled++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]vendors.Vendor, 0, len(f.list))
	for _, v := range f.list {
		if v.Active {
			out = append(out, v.Vendor)
		}
	}
	return out, nil
}

// twoVendors covers every branch the row has: a complete vendor with a GSTIN
// and a contact, and a retired one with neither.
func twoVendors() []vendors.VendorWithUsage {
	return []vendors.VendorWithUsage{
		{Vendor: vendors.Vendor{
			ID: 4, Name: "Chennai Networks Pvt Ltd", ContactPerson: "R. Kumar",
			Phone: "044-2812 3456 x14", Email: "kumar@example.com",
			GSTIN: "27AAPFU0939F1ZV", Address: "Mount Road", Active: true,
		}, Usage: vendors.Usage{DeviceTypes: 2, Purchases: 11, PurchaseOrders: 3, SpendStr: "184500.00"}},
		{Vendor: vendors.Vendor{
			ID: 5, Name: "Old Cable Supplies", Active: false,
		}, Usage: vendors.Usage{SpendStr: "0.00"}},
	}
}

func vendorScreen() (*Handler, *fakeVendors) {
	f := &fakeVendors{list: twoVendors()}
	return &Handler{vendors: f}, f
}

// ── Reach ────────────────────────────────────────────────────────────────────

// Reads are gated as tightly as writes, deliberately: a vendor row carries a
// contact person, a phone number and a GSTIN, which a technician issuing a
// router has no reason to hold — even though the same technician can see the
// Inventory screen the supplier dropdown appears on.
func TestFR_INV_004_OnlyOwnerAndBillingSeeTheVendorSection(t *testing.T) {
	want := map[string]bool{"isp_owner": true, "billing_admin": true}
	for _, role := range []string{"isp_owner", "billing_admin", "csr", "noc_engineer",
		"technician", "lco", "franchise_admin", "intern", ""} {
		var inSection bool
		for _, sec := range AllowedSections(role, false) {
			if sec.Key == "vendors" {
				inSection = true
			}
		}
		if inSection != want[role] {
			t.Errorf("role %q: in the Vendors section = %v, want %v", role, inSection, want[role])
		}
	}
}

// A technician can reach Inventory but not Vendors, and that has to stay
// true: the purchase form's dropdown is fed from the master without handing
// out the master itself.
func TestFR_INV_004_ATechnicianSeesInventoryButNotTheMaster(t *testing.T) {
	var inventorySeen, vendorsSeen bool
	for _, sec := range AllowedSections("technician", false) {
		switch sec.Key {
		case "inventory":
			inventorySeen = true
		case "vendors":
			vendorsSeen = true
		}
	}
	if !inventorySeen {
		t.Error("a technician has lost the Inventory screen")
	}
	if vendorsSeen {
		t.Error("a technician can reach the vendor master, which holds suppliers' contact details and GSTINs")
	}
}

func TestFR_INV_004_AnUnwiredDeploymentSaysSoRatherThanPanicking(t *testing.T) {
	h := &Handler{}
	w := servePR(h, "isp_owner", "GET", "/staff/vendors", nil, h.Vendors)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "not configured") {
		t.Errorf("got %d: %s", w.Code, w.Body.String())
	}
}

// ── Screen ───────────────────────────────────────────────────────────────────

func TestFR_INV_004_TheScreenRendersWithRealData(t *testing.T) {
	h, f := vendorScreen()

	t.Run("the list, live and retired", func(t *testing.T) {
		w := servePR(h, "billing_admin", "GET", "/staff/vendors", nil, h.Vendors)
		if w.Code != 200 {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		for _, want := range []string{
			"Chennai Networks Pvt Ltd", "R. Kumar", "27AAPFU0939F1ZV", "184500.00",
			"Old Cable Supplies", "retired",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the screen does not show %q", want)
			}
		}
		// A vendor with no GSTIN says so rather than rendering an empty cell,
		// which would read as a value that failed to load.
		if !strings.Contains(body, "not recorded") {
			t.Error("a vendor without a GSTIN shows a blank cell instead of saying none is recorded")
		}
		if strings.Contains(body, "<no value>") {
			t.Error("a template field name is wrong — the page rendered <no value>")
		}
	})

	t.Run("the edit form loads one vendor", func(t *testing.T) {
		f.one = &vendors.Vendor{ID: 4, Name: "Chennai Networks Pvt Ltd",
			GSTIN: "27AAPFU0939F1ZV", ContactPerson: "R. Kumar", Active: true}
		w := servePR(h, "billing_admin", "GET", "/staff/vendors?edit=4", nil, h.Vendors)
		if w.Code != 200 {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `action="/staff/vendors/4"`) {
			t.Error("the edit form does not post to the vendor it loaded")
		}
		if !strings.Contains(body, `value="27AAPFU0939F1ZV"`) {
			t.Error("the edit form does not carry the GSTIN it is editing")
		}
	})

	// A hand-edited URL naming a vendor that does not exist shows the add
	// form rather than an error page: the list is still useful, and an
	// operator who mistyped an id does not need a dead end.
	t.Run("an unknown edit id falls back to the add form", func(t *testing.T) {
		f.one = nil
		w := servePR(h, "billing_admin", "GET", "/staff/vendors?edit=9999", nil, h.Vendors)
		if w.Code != 200 {
			t.Fatalf("got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "Add a vendor") {
			t.Error("an unknown edit id did not fall back to the add form")
		}
	})

	t.Run("an empty master explains itself", func(t *testing.T) {
		empty := &Handler{vendors: &fakeVendors{}}
		w := servePR(empty, "isp_owner", "GET", "/staff/vendors", nil, empty.Vendors)
		if !strings.Contains(w.Body.String(), "No vendors yet") {
			t.Errorf("an empty master renders no explanation: %s", w.Body.String())
		}
	})
}

// ── Writes ───────────────────────────────────────────────────────────────────

// The console must file against the signed-in operator, never a form field,
// so the audit trail names who entered a GSTIN.
func TestFR_INV_004_AVendorIsFiledAgainstTheSignedInOperator(t *testing.T) {
	h, f := vendorScreen()
	form := url.Values{
		"name":           {"  Anna   Nagar Cables "},
		"contact_person": {"K. Devi"},
		"phone":          {"044-2812 3456 x14"},
		"gstin":          {"27aapfu0939f1zv"},
	}
	w := servePR(h, "billing_admin", "POST", "/staff/vendors", form, h.CreateVendorForm)
	if w.Code != 303 {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if f.who != "billing_admin_user" {
		t.Errorf("filed by %q, want the session's own username", f.who)
	}
	// Normalised on the way in: whitespace collapsed, GSTIN upper-cased.
	if f.added.Name != "Anna Nagar Cables" {
		t.Errorf("the name reached the store as %q", f.added.Name)
	}
	if f.added.GSTIN != "27AAPFU0939F1ZV" {
		t.Errorf("the GSTIN reached the store as %q", f.added.GSTIN)
	}
}

// A bad GSTIN is refused before it reaches the store, with the sentence that
// says which of the two checks it failed — that is the whole value of the
// message, so it is carried verbatim rather than mapped to a code.
func TestFR_INV_004_TheFormExplainsWhichGSTINCheckFailed(t *testing.T) {
	cases := []struct {
		name  string
		gstin string
		says  string
	}{
		{"not a GSTIN at all", "NOPE", "15 characters"},
		{"well-formed but mistyped", "27AAPFU0939F1ZA", "check digit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, f := vendorScreen()
			form := url.Values{"name": {"Chennai Networks"}, "gstin": {tc.gstin}}
			w := servePR(h, "billing_admin", "POST", "/staff/vendors", form, h.CreateVendorForm)
			if w.Code != 303 {
				t.Fatalf("got %d", w.Code)
			}
			if f.calls != 0 {
				t.Error("an invalid GSTIN reached the store")
			}
			location, err := url.Parse(w.Header().Get("Location"))
			if err != nil {
				t.Fatalf("Location: %v", err)
			}
			if got := location.Query().Get("err"); !strings.Contains(got, tc.says) {
				t.Errorf("the message %q does not mention %q", got, tc.says)
			}
		})
	}
}

// A duplicate name and a duplicate GSTIN must not read the same: one is two
// people adding the same supplier, the other is one legal entity in the
// master twice.
func TestFR_INV_004_TheTwoDuplicatesAreWordedDifferently(t *testing.T) {
	byName := vendorMessages["duplicate"]
	byGSTIN := vendorMessages["gstin_dupe"]
	if byName == "" || byGSTIN == "" {
		t.Fatal("one of the duplicate messages is missing")
	}
	if byName == byGSTIN {
		t.Error("the two kinds of duplicate are worded identically")
	}
	if !strings.Contains(byGSTIN, "GSTIN") {
		t.Error("the GSTIN clash message does not mention the GSTIN")
	}

	h, f := vendorScreen()
	f.err = vendors.ErrGSTINTaken
	w := servePR(h, "billing_admin", "POST", "/staff/vendors",
		url.Values{"name": {"Chennai Netwerks"}, "gstin": {"27AAPFU0939F1ZV"}}, h.CreateVendorForm)
	location, _ := url.Parse(w.Header().Get("Location"))
	if location.Query().Get("m") != "gstin_dupe" {
		t.Errorf("a GSTIN clash redirected with m=%q", location.Query().Get("m"))
	}
}

func TestFR_INV_004_RetiringAndRestoringGoThroughTheSameForm(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
		code  string
	}{
		{"true", true, "retired"},
		{"false", false, "restored"},
	} {
		h, f := vendorScreen()
		w := servePR(h, "isp_owner", "POST", "/staff/vendors/4/retired",
			url.Values{"retired": {tc.value}}, h.SetVendorRetiredForm)
		if w.Code != 303 {
			t.Fatalf("got %d", w.Code)
		}
		if f.retiredID != 4 || f.retired != tc.want {
			t.Errorf("retired=%q reached the store as id %d retired=%v", tc.value, f.retiredID, f.retired)
		}
		location, _ := url.Parse(w.Header().Get("Location"))
		if location.Query().Get("m") != tc.code {
			t.Errorf("redirected with m=%q, want %q", location.Query().Get("m"), tc.code)
		}
	}
}

// The retire message has to say that history is kept, because "retire" next
// to a vendor with eleven purchases behind it otherwise reads like a delete.
func TestFR_INV_004_TheRetireMessageSaysHistoryIsKept(t *testing.T) {
	m := vendorMessages["retired"]
	if !strings.Contains(m, "unchanged") {
		t.Errorf("the retire message does not say existing purchases are kept: %q", m)
	}
}

// ── The dropdowns ────────────────────────────────────────────────────────────

// The half of this requirement an operator notices: the Inventory and
// Procurement forms take a supplier from the master instead of a text box.
func TestFR_INV_004_TheInventoryAndProcurementFormsOfferTheMaster(t *testing.T) {
	f := &fakeVendors{list: twoVendors()}

	t.Run("inventory", func(t *testing.T) {
		h := &Handler{vendors: f, inventory: &vendorAwareInventory{}}
		w := servePR(h, "isp_owner", "GET", "/staff/inventory", nil, h.Inventory)
		if w.Code != 200 {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `name="vendor_id"`) {
			t.Error("the inventory forms still take a typed vendor rather than one from the master")
		}
		if strings.Contains(body, `<input name="vendor"`) {
			t.Error("a free-text vendor box survives on the inventory screen")
		}
		// Only active vendors are offered; a retired one is still shown on
		// every historical row but cannot be chosen for something new.
		if !strings.Contains(body, "Chennai Networks Pvt Ltd") {
			t.Error("the active vendor is not in the dropdown")
		}
		if strings.Contains(body, `<option value="5"`) {
			t.Error("a retired vendor is offered for a new purchase")
		}
	})

	t.Run("procurement", func(t *testing.T) {
		h := &Handler{vendors: f, procurement: &emptyProcurement{}}
		w := servePR(h, "isp_owner", "GET", "/staff/procurement", nil, h.Procurement)
		if w.Code != 200 {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, `name="vendor_id"`) {
			t.Error("the purchase-order form still takes a typed vendor")
		}
		if strings.Contains(body, `<input name="vendor"`) {
			t.Error("a free-text vendor box survives on the procurement screen")
		}
	})
}

// With no vendors yet, the forms say where to add one rather than showing an
// empty dropdown that cannot be satisfied.
func TestFR_INV_004_AnEmptyMasterTellsTheFormsWhereToGo(t *testing.T) {
	h := &Handler{vendors: &fakeVendors{}, inventory: &vendorAwareInventory{}}
	w := servePR(h, "isp_owner", "GET", "/staff/inventory", nil, h.Inventory)
	if !strings.Contains(w.Body.String(), "/staff/vendors") {
		t.Error("with no vendors, the inventory form does not point at the Vendors screen")
	}
	if strings.Contains(w.Body.String(), `<select name="vendor_id"`) {
		t.Error("an empty dropdown is shown instead of an explanation")
	}
}

// A missing master must not take the Inventory screen down with it: issuing
// and returning devices have nothing to do with vendors.
func TestFR_INV_004_AMissingMasterDoesNotBreakTheInventoryScreen(t *testing.T) {
	h := &Handler{inventory: &vendorAwareInventory{}} // no vendors wired at all
	w := servePR(h, "technician", "GET", "/staff/inventory", nil, h.Inventory)
	if w.Code != 200 {
		t.Fatalf("the inventory screen returns %d with no vendor master: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Stock levels") {
		t.Error("the stock table did not render")
	}
	if strings.Contains(body, "<no value>") {
		t.Error("the page rendered <no value>")
	}
}

// ── Fakes for the two screens the dropdown appears on ────────────────────────

// vendorAwareInventory is an InventoryStore with one of everything, so the
// Inventory screen renders its real template rather than an empty page that
// would pass any assertion about what is absent.
type vendorAwareInventory struct{}

func vendorIDPtr(n int) *int { return &n }

func (vendorAwareInventory) ListDeviceTypes(context.Context) ([]inventory.DeviceType, error) {
	return []inventory.DeviceType{{ID: 1, Name: "hAP ac2", Vendor: "Chennai Networks Pvt Ltd",
		VendorID: vendorIDPtr(4), ReorderThreshold: 5}}, nil
}

func (vendorAwareInventory) CreateDeviceType(_ context.Context, t inventory.DeviceType) (*inventory.DeviceType, error) {
	return &t, nil
}

func (vendorAwareInventory) ListDevices(context.Context, *string, *int, *int) ([]inventory.Device, error) {
	return []inventory.Device{{ID: 1, DeviceTypeID: 1, DeviceType: "hAP ac2",
		SerialNumber: "SN-1", Status: inventory.StatusInStock}}, nil
}

func (vendorAwareInventory) CreateDevice(_ context.Context, d inventory.Device) (*inventory.Device, error) {
	return &d, nil
}
func (vendorAwareInventory) GetDeviceBySerial(context.Context, string) (*inventory.Device, error) {
	return nil, nil
}
func (vendorAwareInventory) IssueDevice(context.Context, string, int) (*inventory.Device, error) {
	return nil, nil
}
func (vendorAwareInventory) ReturnDevice(context.Context, string, string) (*inventory.Device, error) {
	return nil, nil
}

func (vendorAwareInventory) GetStockLevels(context.Context, bool) ([]inventory.StockLevel, error) {
	return []inventory.StockLevel{{DeviceTypeID: 1, DeviceType: "hAP ac2",
		Vendor: "Chennai Networks Pvt Ltd", VendorID: vendorIDPtr(4), InStock: 9, ReorderThreshold: 5}}, nil
}

func (vendorAwareInventory) RecordPurchase(_ context.Context, p inventory.Purchase) (*inventory.Purchase, error) {
	return &p, nil
}

func (vendorAwareInventory) ListPurchases(context.Context, *int) ([]inventory.Purchase, error) {
	return []inventory.Purchase{{ID: 1, DeviceTypeID: 1, Vendor: "Chennai Networks Pvt Ltd",
		VendorID: vendorIDPtr(4), Quantity: 10, UnitCostStr: "1250.00", TotalCostStr: "12500.00",
		PurchasedBy: "owner", PurchasedAt: time.Now()}}, nil
}

// emptyProcurement renders the Procurement screen with no orders — enough for
// the form, which is what these tests look at.
type emptyProcurement struct{}

func (emptyProcurement) CreatePurchaseOrder(_ context.Context, po procurement.NewPurchaseOrder) (*procurement.PurchaseOrder, error) {
	return &procurement.PurchaseOrder{ID: 1, Description: po.Description,
		VendorID: vendorIDPtr(po.VendorID), Category: po.Category, Amount: po.Amount}, nil
}

func (emptyProcurement) ListPurchaseOrders(context.Context, *string) ([]procurement.PurchaseOrder, error) {
	return nil, nil
}
func (emptyProcurement) GetPurchaseOrder(context.Context, int) (*procurement.PurchaseOrder, error) {
	return nil, nil
}
func (emptyProcurement) DecidePurchaseOrder(context.Context, int, bool, string, string) (*procurement.PurchaseOrder, error) {
	return nil, nil
}
func (emptyProcurement) UpdateFulfilment(context.Context, int, string, string) (*procurement.PurchaseOrder, error) {
	return nil, nil
}
