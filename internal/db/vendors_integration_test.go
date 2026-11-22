//go:build integration

package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/procurement"
	"github.com/maaransoft/isp-bss-oss/internal/vendors"
	"github.com/shopspring/decimal"
)

// The shared vendor master against a real database — FR-INV-004 | migration
// 071.
//
// What needs a real PostgreSQL rather than a stub, in order of how quietly it
// would break:
//
//   - The two error mappings in vendorConflict are keyed on constraint *names*.
//     Rename an index in a later migration and the mapping silently stops
//     firing: a duplicate then surfaces as an unexplained 500 instead of
//     "another vendor already has that name", and no unit test can tell.
//   - The unique index normalises the name with its own SQL expression, which
//     has to agree with vendors.NormalisedKey in Go. Two different
//     normalisations means the duplicate the index exists to refuse goes live
//     under a name no search finds.
//   - A rename has to show up on every purchase and order at once. That is the
//     requirement, and it is a property of the join, not of any Go code.
//   - The write paths select their vendor from the master inside the insert, so
//     "retired" and "no such vendor" are distinguished by SQL, not by a check
//     the caller could skip.

func TestFR_INV_004_TheMasterRoundTripsEveryField(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Vendors()

	in := vendors.Vendor{
		Name: "  Chennai   Networks Pvt Ltd ", ContactPerson: "R.  Kumar",
		Phone: "044-2812 3456 x14", Email: "kumar@example.com",
		GSTIN: "27aapfu0939f1zv", Address: "Mount Road", Notes: "Net 30",
	}
	created, err := store.CreateVendor(ctx, in, "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Normalised on the way in by the store, not left to the caller.
	if created.Name != "Chennai Networks Pvt Ltd" {
		t.Errorf("name stored as %q", created.Name)
	}
	if created.ContactPerson != "R. Kumar" {
		t.Errorf("contact stored as %q", created.ContactPerson)
	}
	if created.GSTIN != "27AAPFU0939F1ZV" {
		t.Errorf("GSTIN stored as %q", created.GSTIN)
	}
	if !created.Active {
		t.Error("a new vendor is not active")
	}

	// And every optional field survives the NULLIF-on-write,
	// COALESCE-on-read round trip rather than coming back as "<nil>".
	got, err := store.GetVendor(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if *got != *created {
		t.Errorf("read back as %+v, want %+v", got, created)
	}

	// A vendor with nothing but a name reads back with empty strings, not
	// nulls — the shape every template and JSON consumer expects.
	bare, err := store.CreateVendor(ctx, vendors.Vendor{Name: "Anna Nagar Cables"}, "owner")
	if err != nil {
		t.Fatalf("create bare: %v", err)
	}
	if bare.GSTIN != "" || bare.Phone != "" || bare.ContactPerson != "" {
		t.Errorf("a vendor with only a name came back as %+v", bare)
	}
}

// The constraint-name mapping. If a later migration renames either index,
// this fails here rather than turning every duplicate into a 500.
func TestFR_INV_004_TheTwoUniqueIndexesMapToTheirOwnErrors(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Vendors()

	first, err := store.CreateVendor(ctx,
		vendors.Vendor{Name: "Chennai Networks", GSTIN: "27AAPFU0939F1ZV"}, "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The index normalises the name itself, so these have to collide even
	// though no Go code compared them.
	for _, name := range []string{
		"Chennai Networks", "chennai networks", "CHENNAI NETWORKS",
		"  Chennai Networks  ", "Chennai   Networks",
	} {
		_, err := store.CreateVendor(ctx, vendors.Vendor{Name: name}, "owner")
		if !errors.Is(err, vendors.ErrNameTaken) {
			t.Errorf("creating %q gave %v, want ErrNameTaken — the index's normalisation"+
				" and vendors.NormalisedKey have diverged", name, err)
		}
	}

	_, err = store.CreateVendor(ctx,
		vendors.Vendor{Name: "A Completely Different Name", GSTIN: "27AAPFU0939F1ZV"}, "owner")
	if !errors.Is(err, vendors.ErrGSTINTaken) {
		t.Errorf("a duplicate GSTIN gave %v, want ErrGSTINTaken", err)
	}

	// Both indexes are partial on active, so retiring the holder frees the
	// name and the GSTIN for whoever takes over that supply.
	if err := store.SetVendorActive(ctx, first.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := store.CreateVendor(ctx,
		vendors.Vendor{Name: "Chennai Networks", GSTIN: "27AAPFU0939F1ZV"}, "owner"); err != nil {
		t.Errorf("after retiring the holder, the name and GSTIN are still taken: %v", err)
	}
}

// Restoring a retired vendor can collide, because the indexes only cover
// active rows. The honest answer is the same conflict a create would give:
// there is now a live vendor by that name, and which of the two should stay is
// not this function's call.
func TestFR_INV_004_RestoringOverALiveNamesakeIsRefused(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Vendors()

	old := seedVendorVia(ctx, t, database, "Chennai Networks", "")
	if err := store.SetVendorActive(ctx, old.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if _, err := store.CreateVendor(ctx, vendors.Vendor{Name: "Chennai Networks"}, "owner"); err != nil {
		t.Fatalf("create the replacement: %v", err)
	}

	err := store.SetVendorActive(ctx, old.ID, true, "owner")
	if !errors.Is(err, vendors.ErrNameTaken) {
		t.Errorf("restoring over a live namesake gave %v, want ErrNameTaken", err)
	}
}

// Correcting a GSTIN on a supplier you no longer buy from is exactly what
// somebody reconciling last year's input tax credit needs to do, so a retired
// vendor stays editable.
func TestFR_INV_004_ARetiredVendorIsStillEditable(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Vendors()

	v := seedVendorVia(ctx, t, database, "Old Cable Supplies", "")
	if err := store.SetVendorActive(ctx, v.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	updated, err := store.UpdateVendor(ctx, v.ID,
		vendors.Vendor{Name: "Old Cable Supplies", GSTIN: "27AAPFU0939F1ZV"}, "owner")
	if err != nil {
		t.Fatalf("edit a retired vendor: %v", err)
	}
	if updated.GSTIN != "27AAPFU0939F1ZV" {
		t.Errorf("the GSTIN did not stick: %q", updated.GSTIN)
	}
	if updated.Active {
		t.Error("editing a retired vendor brought it back to life")
	}
}

// An edit writes every field, including the cleared ones — that is how a
// contact who has left the supplier gets removed.
func TestFR_INV_004_AnEditClearsTheFieldsLeftEmpty(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Vendors()

	v, err := store.CreateVendor(ctx, vendors.Vendor{
		Name: "Chennai Networks", ContactPerson: "R. Kumar", Phone: "044-2812 3456",
	}, "owner")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	updated, err := store.UpdateVendor(ctx, v.ID, vendors.Vendor{Name: "Chennai Networks"}, "owner")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if updated.ContactPerson != "" || updated.Phone != "" {
		t.Errorf("a cleared contact survived the edit: %+v", updated)
	}
}

func TestFR_INV_004_UpdatingAMissingVendorSaysSo(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Vendors()

	if _, err := store.UpdateVendor(ctx, 99999, vendors.Vendor{Name: "Nobody"}, "owner"); !errors.Is(err, vendors.ErrNotFound) {
		t.Errorf("updating a missing vendor gave %v, want ErrNotFound", err)
	}
	if err := store.SetVendorActive(ctx, 99999, false, "owner"); !errors.Is(err, vendors.ErrNotFound) {
		t.Errorf("retiring a missing vendor gave %v, want ErrNotFound", err)
	}
	if _, err := store.GetVendor(ctx, 99999); !errors.Is(err, vendors.ErrNotFound) {
		t.Errorf("reading a missing vendor gave %v, want ErrNotFound", err)
	}
}

// ── The requirement itself ───────────────────────────────────────────────────

// Correcting a vendor's name in the master corrects it on every purchase,
// device type and purchase order at once. This is the whole point of holding
// it in one place, and it is a property of the joins rather than of any Go
// code — which is why it is proven here.
func TestFR_INV_004_ARenameInTheMasterReachesEveryRowAtOnce(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Vendors()
	inv := database.Inventory()
	proc := database.Procurement()
	seedStaffUser(ctx, t, pool, 1, "owner", "isp_owner")
	seedStaffUser(ctx, t, pool, 2, "billing", "billing_admin")

	v := seedVendorVia(ctx, t, database, "Chennai Netwoks", "") // misspelled on entry

	dt, err := inv.CreateDeviceType(ctx, inventory.DeviceType{
		Name: "hAP ac2", VendorID: &v.ID, ReorderThreshold: 5})
	if err != nil {
		t.Fatalf("create device type: %v", err)
	}
	if dt.Vendor != "Chennai Netwoks" {
		t.Errorf("the device type took the vendor name as %q", dt.Vendor)
	}

	if _, err := inv.RecordPurchase(ctx, inventory.Purchase{
		DeviceTypeID: dt.ID, VendorID: &v.ID, Quantity: 10,
		UnitCost: decimal.RequireFromString("1250.00"), PurchasedBy: "owner"}); err != nil {
		t.Fatalf("record purchase: %v", err)
	}
	po, err := proc.CreatePurchaseOrder(ctx, procurement.NewPurchaseOrder{
		Description: "Patch cords", VendorID: v.ID, Category: "hardware",
		Amount: "4000.00", RequestedBy: "owner"})
	if err != nil {
		t.Fatalf("create purchase order: %v", err)
	}

	// Now fix the spelling in one place.
	if _, err := store.UpdateVendor(ctx, v.ID,
		vendors.Vendor{Name: "Chennai Networks", GSTIN: "27AAPFU0939F1ZV"}, "owner"); err != nil {
		t.Fatalf("rename: %v", err)
	}

	const want = "Chennai Networks"

	types, err := inv.ListDeviceTypes(ctx)
	if err != nil || len(types) != 1 {
		t.Fatalf("list device types: %v (%d rows)", err, len(types))
	}
	if types[0].Vendor != want {
		t.Errorf("the device type still shows %q", types[0].Vendor)
	}

	purchases, err := inv.ListPurchases(ctx, nil)
	if err != nil || len(purchases) != 1 {
		t.Fatalf("list purchases: %v (%d rows)", err, len(purchases))
	}
	if purchases[0].Vendor != want {
		t.Errorf("the purchase still shows %q", purchases[0].Vendor)
	}

	levels, err := inv.GetStockLevels(ctx, false)
	if err != nil || len(levels) != 1 {
		t.Fatalf("stock levels: %v (%d rows)", err, len(levels))
	}
	if levels[0].Vendor != want {
		t.Errorf("the stock level still shows %q", levels[0].Vendor)
	}

	orders, err := proc.ListPurchaseOrders(ctx, nil)
	if err != nil || len(orders) != 1 {
		t.Fatalf("list orders: %v (%d rows)", err, len(orders))
	}
	if orders[0].Vendor != want {
		t.Errorf("the purchase order still shows %q", orders[0].Vendor)
	}

	// Including the paths that return a row from an UPDATE. These use a CTE
	// and select through the join rather than a bare RETURNING, which cannot
	// see a joined table — without that, a decision's response would carry
	// the pre-rename name while the list beside it carried the new one.
	decided, err := proc.DecidePurchaseOrder(ctx, po.ID, true, "billing", "ok")
	if err != nil || decided == nil {
		t.Fatalf("decide: %v", err)
	}
	if decided.Vendor != want {
		t.Errorf("the decision response shows %q, want the master's current name", decided.Vendor)
	}
	received, err := proc.UpdateFulfilment(ctx, po.ID, procurement.StatusReceived, "billing")
	if err != nil || received == nil {
		t.Fatalf("fulfil: %v", err)
	}
	if received.Vendor != want {
		t.Errorf("the fulfilment response shows %q", received.Vendor)
	}

	// The free text each row was written with is kept, untouched by the
	// rename: it is the evidence trail migration 071's backfill needs if a
	// merge turns out to have been wrong.
	if snapshot := scanString(ctx, t, pool,
		`SELECT vendor FROM cpe_device_types WHERE id = $1`, dt.ID); snapshot != "Chennai Netwoks" {
		t.Errorf("the recorded-at-the-time name is now %q — the evidence trail was overwritten", snapshot)
	}
}

// A new purchase cannot name a retired vendor, an unknown one, or none at
// all, and the three are told apart — "retired" sends an operator to the
// master to restore it, "no such vendor" sends them looking for a typo.
func TestFR_INV_004_TheWritePathsRefuseAnUnusableVendor(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Vendors()
	inv := database.Inventory()
	proc := database.Procurement()
	seedStaffUser(ctx, t, pool, 1, "owner", "isp_owner")

	live := seedVendorVia(ctx, t, database, "Chennai Networks", "")
	retired := seedVendorVia(ctx, t, database, "Old Cable Supplies", "")
	if err := store.SetVendorActive(ctx, retired.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}
	dt, err := inv.CreateDeviceType(ctx, inventory.DeviceType{
		Name: "hAP ac2", VendorID: &live.ID, ReorderThreshold: 5})
	if err != nil {
		t.Fatalf("create device type: %v", err)
	}

	missing := 99999
	cases := []struct {
		name     string
		vendorID *int
		want     error
	}{
		{"a retired vendor", &retired.ID, vendors.ErrInactive},
		{"a vendor that never existed", &missing, vendors.ErrNotFound},
		{"no vendor at all", nil, vendors.ErrRequired},
	}
	for _, tc := range cases {
		t.Run("a purchase naming "+tc.name, func(t *testing.T) {
			_, err := inv.RecordPurchase(ctx, inventory.Purchase{
				DeviceTypeID: dt.ID, VendorID: tc.vendorID, Quantity: 1,
				UnitCost: decimal.RequireFromString("10.00"), PurchasedBy: "owner"})
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
		t.Run("a device type naming "+tc.name, func(t *testing.T) {
			_, err := inv.CreateDeviceType(ctx, inventory.DeviceType{
				Name: "another " + tc.name, VendorID: tc.vendorID, ReorderThreshold: 5})
			if !errors.Is(err, tc.want) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
		})
	}

	// And a purchase order, whose vendor id is a plain int rather than a
	// pointer, so "none" is zero.
	if _, err := proc.CreatePurchaseOrder(ctx, procurement.NewPurchaseOrder{
		Description: "x", VendorID: retired.ID, Category: "other",
		Amount: "1.00", RequestedBy: "owner"}); !errors.Is(err, vendors.ErrInactive) {
		t.Errorf("an order naming a retired vendor gave %v, want ErrInactive", err)
	}
	if _, err := proc.CreatePurchaseOrder(ctx, procurement.NewPurchaseOrder{
		Description: "x", VendorID: 0, Category: "other",
		Amount: "1.00", RequestedBy: "owner"}); !errors.Is(err, vendors.ErrRequired) {
		t.Errorf("an order naming no vendor gave %v, want ErrRequired", err)
	}

	// Nothing was written by any of the refusals.
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM cpe_purchases`); n != 0 {
		t.Errorf("%d purchase(s) were written despite every vendor being refused", n)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM purchase_orders`); n != 0 {
		t.Errorf("%d order(s) were written despite every vendor being refused", n)
	}
}

// Usage counts and spend: the columns that make the master list useful for
// finding the duplicates the backfill could not merge.
func TestFR_INV_004_UsageCountsAndSpendComeFromAllThreeTables(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	inv := database.Inventory()
	proc := database.Procurement()
	seedStaffUser(ctx, t, pool, 1, "owner", "isp_owner")
	seedStaffUser(ctx, t, pool, 2, "billing", "billing_admin")

	v := seedVendorVia(ctx, t, database, "Chennai Networks", "")
	dt, err := inv.CreateDeviceType(ctx, inventory.DeviceType{
		Name: "hAP ac2", VendorID: &v.ID, ReorderThreshold: 5})
	if err != nil {
		t.Fatalf("create device type: %v", err)
	}
	if _, err := inv.RecordPurchase(ctx, inventory.Purchase{
		DeviceTypeID: dt.ID, VendorID: &v.ID, Quantity: 10,
		UnitCost: decimal.RequireFromString("1250.00"), PurchasedBy: "owner"}); err != nil {
		t.Fatalf("purchase: %v", err)
	}

	// Two orders: one received, one still only requested. Only the received
	// one counts towards spend — a requested order is not money that went
	// anywhere, and counting it would make this column disagree with the
	// ledger for every order awaiting a decision.
	receivedPO, err := proc.CreatePurchaseOrder(ctx, procurement.NewPurchaseOrder{
		Description: "Patch cords", VendorID: v.ID, Category: "hardware",
		Amount: "4000.00", RequestedBy: "owner"})
	if err != nil {
		t.Fatalf("order: %v", err)
	}
	if _, err := proc.DecidePurchaseOrder(ctx, receivedPO.ID, true, "billing", "ok"); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if _, err := proc.UpdateFulfilment(ctx, receivedPO.ID, procurement.StatusReceived, "billing"); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if _, err := proc.CreatePurchaseOrder(ctx, procurement.NewPurchaseOrder{
		Description: "Not yet decided", VendorID: v.ID, Category: "other",
		Amount: "9999.00", RequestedBy: "owner"}); err != nil {
		t.Fatalf("second order: %v", err)
	}

	list, err := database.Vendors().ListVendors(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("the master has %d rows, want 1", len(list))
	}
	u := list[0].Usage
	if u.DeviceTypes != 1 || u.Purchases != 1 || u.PurchaseOrders != 2 {
		t.Errorf("usage is %+v, want 1 device type, 1 purchase, 2 orders", u)
	}
	// 10 × 1250.00 = 12500.00, plus the one received order at 4000.00.
	if u.SpendStr != "16500.00" {
		t.Errorf("spend is %q, want 16500.00 — the requested order's 9999.00 must not count", u.SpendStr)
	}
	if !u.Referenced() {
		t.Error("a vendor with purchases behind it reports itself unreferenced")
	}

	// A vendor nothing points at reports zero as a money string, not as "0".
	spare := seedVendorVia(ctx, t, database, "Anna Nagar Cables", "")
	list, err = database.Vendors().ListVendors(ctx)
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	for _, row := range list {
		if row.ID != spare.ID {
			continue
		}
		if row.Usage.SpendStr != "0.00" {
			t.Errorf("an unused vendor's spend is %q, want 0.00", row.Usage.SpendStr)
		}
		if row.Usage.Referenced() {
			t.Error("an unused vendor reports itself referenced")
		}
	}
}

// The compatibility path's lookup, against the index's own normalisation.
func TestFR_INV_004_ResolvingByNameMatchesTheIndexAndRefusesTheRest(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Vendors()

	v := seedVendorVia(ctx, t, database, "Chennai Networks", "")
	retired := seedVendorVia(ctx, t, database, "Old Cable Supplies", "")
	if err := store.SetVendorActive(ctx, retired.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	for _, name := range []string{
		"Chennai Networks", "chennai networks", "CHENNAI NETWORKS",
		"  Chennai   Networks  ", "chennai\tnetworks",
	} {
		got, err := store.ResolveVendorByName(ctx, name)
		if err != nil {
			t.Errorf("resolving %q failed: %v", name, err)
			continue
		}
		if got.ID != v.ID {
			t.Errorf("%q resolved to vendor %d, want %d", name, got.ID, v.ID)
		}
	}

	for _, name := range []string{"Chennai Netwoks", "", "   ", "Old Cable Supplies"} {
		if _, err := store.ResolveVendorByName(ctx, name); !errors.Is(err, vendors.ErrNotFound) {
			t.Errorf("resolving %q gave %v, want ErrNotFound — a retired or unknown"+
				" name must never resolve", name, err)
		}
	}
}

// Only active vendors are offered for new purchases, ordered so an operator
// scanning the dropdown finds a name regardless of how it was capitalised.
func TestFR_INV_004_OnlyActiveVendorsArePickableAndTheyAreNameOrdered(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Vendors()

	for _, name := range []string{"zebra cables", "Anna Nagar Cables", "Chennai Networks"} {
		seedVendorVia(ctx, t, database, name, "")
	}
	retired := seedVendorVia(ctx, t, database, "Old Cable Supplies", "")
	if err := store.SetVendorActive(ctx, retired.ID, false, "owner"); err != nil {
		t.Fatalf("retire: %v", err)
	}

	pickable, err := store.ListActiveVendors(ctx)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	var names []string
	for _, v := range pickable {
		names = append(names, v.Name)
	}
	want := []string{"Anna Nagar Cables", "Chennai Networks", "zebra cables"}
	if len(names) != len(want) {
		t.Fatalf("pickable vendors are %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("pickable vendors are %v, want %v — a lower-case name sorted"+
				" past the Zs means the ordering is not case-insensitive", names, want)
		}
	}

	// The retired one is still in the full list, because that is where the
	// duplicates to merge and the GSTINs to correct are.
	full, err := store.ListVendors(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(full) != 4 {
		t.Errorf("the master list has %d rows, want 4 including the retired one", len(full))
	}
	if full[len(full)-1].Active {
		t.Error("retired vendors are not last in the master list")
	}
}

// seedVendorVia creates a vendor through the real store.
func seedVendorVia(ctx context.Context, t *testing.T, database *db.DB, name, gstin string) *vendors.Vendor {
	t.Helper()
	v, err := database.Vendors().CreateVendor(ctx, vendors.Vendor{Name: name, GSTIN: gstin}, "owner")
	if err != nil {
		t.Fatalf("seed vendor %q: %v", name, err)
	}
	return v
}
