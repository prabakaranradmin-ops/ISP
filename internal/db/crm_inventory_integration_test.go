//go:build integration

// CRM pipeline and CPE inventory persistence tests — FR-CRM-001..003,
// FR-INV-001..003 | MDS §4.16.
//
// The two properties worth the most here are both races the SQL is
// specifically shaped to survive: converting one lead twice must not produce
// two subscribers, and issuing one physical device twice must not hand it to
// two people. Both are enforced by a conditional UPDATE that only matches a
// row in the expected state, and both are exercised concurrently below —
// a sequential test would pass against an implementation that has neither.
package db_test

import (
	"context"
	"sync"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/crm"
	"github.com/maaransoft/isp-bss-oss/internal/inventory"
)

// newLead is the standard prospect these tests convert.
func newLead(source string) crm.Lead {
	return crm.Lead{
		FullName: "Ravi Kumar", MobileNumber: "+919876500111",
		Email: "ravi@example.com", Source: source,
	}
}

// subscriberFor builds the account half of a conversion — what the lead
// itself cannot supply.
func subscriberFor(caf, username string) api.SubscriberRecord {
	return api.SubscriberRecord{
		CAFNumber: caf, Username: username,
		MobileNumber: "+919876500111", Email: "ravi@example.com",
		PlanID: 1, RegisteredState: "TN",
		Status: "active", DunningState: "active", KYCStatus: "pending", WalletBalance: "0.00",
	}
}

// ── FR-CRM-001: pipeline ────────────────────────────────────────────────────

func TestFR_CRM_001_LeadCRUD(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.CRM()

	created, err := store.CreateLead(ctx, newLead("walk_in"))
	if err != nil {
		t.Fatalf("CreateLead: %v", err)
	}
	if created.Status != crm.StatusNew {
		t.Errorf("a new lead must start at %q, got %q", crm.StatusNew, created.Status)
	}
	if created.ConvertedSubscriberID != nil {
		t.Error("a new lead must not point at a subscriber")
	}

	t.Run("pipeline movement", func(t *testing.T) {
		status := crm.StatusContacted
		updated, err := store.UpdateLead(ctx, created.ID, &status, nil, nil, nil)
		if err != nil {
			t.Fatalf("UpdateLead: %v", err)
		}
		if updated.Status != crm.StatusContacted {
			t.Errorf("status = %q, want contacted", updated.Status)
		}
	})

	t.Run("unknown id returns (nil, nil)", func(t *testing.T) {
		l, err := store.GetLead(ctx, 999999)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if l != nil {
			t.Errorf("want nil for an unknown lead, got %+v", l)
		}
	})
}

// TestFR_CRM_001_SchemaRejectsMalformedMobile: a lead's number becomes a
// subscriber's number on conversion, so the E.164 rule applies here too.
func TestFR_CRM_001_SchemaRejectsMalformedMobile(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO leads (full_name, mobile_number, source) VALUES ('X', '9876543210', 'walk_in')`)
	if err == nil {
		t.Fatal("chk_leads_mobile_e164 must reject a number missing '+'")
	}
}

// TestFR_CRM_002_SchemaRejectsConvertedWithoutSubscriber is the constraint
// that keeps FR-CRM-003's conversion rate honest: a lead cannot claim to be
// converted while pointing at nobody.
func TestFR_CRM_002_SchemaRejectsConvertedWithoutSubscriber(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO leads (id, full_name, mobile_number, source)
		VALUES (1, 'X', '+919876500111', 'walk_in')`); err != nil {
		t.Fatalf("seed lead: %v", err)
	}

	_, err := pool.Exec(ctx, `UPDATE leads SET status='converted' WHERE id=1`)
	if err == nil {
		t.Fatal("chk_lead_converted_has_subscriber must reject a converted lead with no subscriber")
	}
}

// ── FR-CRM-002: the conversion handoff ──────────────────────────────────────

func TestFR_CRM_002_ConvertLeadCreatesSubscriberAndMarksLead(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	store := database.CRM()

	lead, err := store.CreateLead(ctx, newLead("referral"))
	if err != nil {
		t.Fatalf("CreateLead: %v", err)
	}

	converted, sub, err := store.ConvertLead(ctx, lead.ID, subscriberFor("CAF-9001", "ravi@isp"), "$2a$12$hash")
	if err != nil {
		t.Fatalf("ConvertLead: %v", err)
	}
	if converted == nil || sub == nil {
		t.Fatal("a first conversion must land")
	}

	if converted.Status != crm.StatusConverted {
		t.Errorf("lead status = %q, want converted", converted.Status)
	}
	if converted.ConvertedSubscriberID == nil || *converted.ConvertedSubscriberID != sub.ID {
		t.Errorf("lead must point at the subscriber it produced, got %v want %d",
			converted.ConvertedSubscriberID, sub.ID)
	}
	if converted.ConvertedAt == nil {
		t.Error("conversion must be timestamped")
	}
	// The carry-over FR-CRM-002 asks for.
	if sub.MobileNumber != lead.MobileNumber {
		t.Errorf("subscriber mobile = %q, want the lead's %q", sub.MobileNumber, lead.MobileNumber)
	}
	// pending_payment, as a direct sign-up is: a converted lead gets no
	// network access until their first cycle is paid for.
	if sub.Username != "ravi@isp" || sub.Status != "pending_payment" {
		t.Errorf("subscriber: %+v", sub)
	}
}

// TestFR_CRM_002_ConcurrentConversionsProduceExactlyOneSubscriber is the
// race this design exists to survive. Ten goroutines convert one lead; if
// the claim were a read-then-write, several would pass the pending check and
// the prospect would become several real, billable subscribers.
func TestFR_CRM_002_ConcurrentConversionsProduceExactlyOneSubscriber(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	store := database.CRM()

	lead, err := store.CreateLead(ctx, newLead("website"))
	if err != nil {
		t.Fatalf("CreateLead: %v", err)
	}

	const racers = 10
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			// Distinct CAF/username per racer: a unique-violation would
			// otherwise mask the claim as the thing keeping them apart.
			caf := "CAF-90" + string(rune('a'+i))
			user := "ravi" + string(rune('a'+i)) + "@isp"
			converted, _, err := store.ConvertLead(ctx, lead.ID, subscriberFor(caf, user), "$2a$12$hash")
			if err != nil {
				return // a losing racer may also lose a unique race; counted below only on success
			}
			if converted != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("want exactly 1 successful conversion out of %d, got %d", racers, winners)
	}

	// The decisive assertion: however many callers tried, exactly one
	// subscriber may exist, because the losers' inserts rolled back with
	// their failed claims.
	var subscriberCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM subscribers`).Scan(&subscriberCount); err != nil {
		t.Fatalf("count subscribers: %v", err)
	}
	if subscriberCount != 1 {
		t.Errorf("DUPLICATE SUBSCRIBERS: one lead produced %d subscribers, want exactly 1", subscriberCount)
	}
}

// TestFR_CRM_002_ConvertingAnAlreadyConvertedLeadReturnsNil covers the
// sequential replay, and that the second attempt leaves no orphan subscriber
// behind from its rolled-back insert.
func TestFR_CRM_002_ConvertingAnAlreadyConvertedLeadReturnsNil(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	store := database.CRM()

	lead, err := store.CreateLead(ctx, newLead("campaign"))
	if err != nil {
		t.Fatalf("CreateLead: %v", err)
	}
	if _, _, err := store.ConvertLead(ctx, lead.ID, subscriberFor("CAF-1", "first@isp"), "h"); err != nil {
		t.Fatalf("first ConvertLead: %v", err)
	}

	converted, sub, err := store.ConvertLead(ctx, lead.ID, subscriberFor("CAF-2", "second@isp"), "h")
	if err != nil {
		t.Fatalf("a replayed conversion must not error: %v", err)
	}
	if converted != nil || sub != nil {
		t.Error("a replayed conversion must not land")
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM subscribers`).Scan(&count); err != nil {
		t.Fatalf("count subscribers: %v", err)
	}
	if count != 1 {
		t.Errorf("the refused conversion must roll its subscriber insert back, got %d subscribers", count)
	}
}

// ── FR-CRM-003: funnel ──────────────────────────────────────────────────────

func TestFR_CRM_003_FunnelReportsRateBySourceAndStage(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	store := database.CRM()

	// Four leads, one converted: a 25.00% rate that would round badly if
	// this were computed in float.
	for i := 0; i < 4; i++ {
		if _, err := store.CreateLead(ctx, newLead("walk_in")); err != nil {
			t.Fatalf("CreateLead: %v", err)
		}
	}
	if _, _, err := store.ConvertLead(ctx, 1, subscriberFor("CAF-1", "conv@isp"), "h"); err != nil {
		t.Fatalf("ConvertLead: %v", err)
	}

	report, err := store.GetFunnel(ctx, nil)
	if err != nil {
		t.Fatalf("GetFunnel: %v", err)
	}
	if report.TotalLeads != 4 {
		t.Errorf("total_leads = %d, want 4", report.TotalLeads)
	}
	if report.ConvertedLeads != 1 {
		t.Errorf("converted_leads = %d, want 1", report.ConvertedLeads)
	}
	if report.ConversionRatePct != "25.00" {
		t.Errorf("conversion_rate_pct = %q, want 25.00", report.ConversionRatePct)
	}
	if len(report.Stages) == 0 {
		t.Error("the funnel must break down by source and stage")
	}
}

func TestFR_CRM_003_EmptyFunnelDoesNotDivideByZero(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	report, err := database.CRM().GetFunnel(ctx, nil)
	if err != nil {
		t.Fatalf("GetFunnel on an empty pipeline: %v", err)
	}
	if report.ConversionRatePct != "0.00" {
		t.Errorf("conversion_rate_pct = %q, want 0.00 for an empty pipeline", report.ConversionRatePct)
	}
}

// ── FR-INV: inventory ───────────────────────────────────────────────────────

func TestFR_INV_001_DeviceLifecycle(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "cpe@isp"})

	store := database.Inventory()
	dt, err := store.CreateDeviceType(ctx, inventory.DeviceType{
		Name: "Archer C6", Vendor: "TP-Link", ReorderThreshold: 2,
	})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}

	device, err := store.CreateDevice(ctx, inventory.Device{
		DeviceTypeID: dt.ID, SerialNumber: "SN-1001", Location: "Chennai warehouse",
	})
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if device.Status != inventory.StatusInStock {
		t.Errorf("a new device must start in_stock, got %q", device.Status)
	}
	if device.DeviceType != "Archer C6" {
		t.Errorf("device_type should be joined in, got %q", device.DeviceType)
	}

	t.Run("issue links it to the subscriber", func(t *testing.T) {
		issued, err := store.IssueDevice(ctx, "SN-1001", 1)
		if err != nil {
			t.Fatalf("IssueDevice: %v", err)
		}
		if issued == nil {
			t.Fatal("issuing an in-stock device must land")
		}
		if issued.Status != inventory.StatusIssued {
			t.Errorf("status = %q, want issued", issued.Status)
		}
		if issued.SubscriberID == nil || *issued.SubscriberID != 1 {
			t.Errorf("subscriber_id = %v, want 1", issued.SubscriberID)
		}
		if issued.IssuedAt == nil {
			t.Error("issuance must be timestamped")
		}
	})

	t.Run("return clears the holder", func(t *testing.T) {
		returned, err := store.ReturnDevice(ctx, "SN-1001", inventory.StatusReturned)
		if err != nil {
			t.Fatalf("ReturnDevice: %v", err)
		}
		if returned == nil {
			t.Fatal("returning an issued device must land")
		}
		if returned.SubscriberID != nil {
			t.Errorf("subscriber_id must be cleared on return, got %v", returned.SubscriberID)
		}
		if returned.Status != inventory.StatusReturned {
			t.Errorf("status = %q, want returned", returned.Status)
		}
	})

	t.Run("returning a device nobody holds does not land", func(t *testing.T) {
		d, err := store.ReturnDevice(ctx, "SN-1001", inventory.StatusReturned)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if d != nil {
			t.Error("a device already returned must not be returnable again")
		}
	})
}

// TestFR_INV_002_ConcurrentIssuesGiveTheDeviceToExactlyOneSubscriber is the
// inventory counterpart of the conversion race: one physical router, ten
// callers, exactly one holder.
func TestFR_INV_002_ConcurrentIssuesGiveTheDeviceToExactlyOneSubscriber(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	for i := 1; i <= 10; i++ {
		seedSubscriber(ctx, t, pool, i, seedOpts{Username: "sub" + string(rune('a'+i)) + "@isp"})
	}

	store := database.Inventory()
	dt, err := store.CreateDeviceType(ctx, inventory.DeviceType{Name: "Archer C6", Vendor: "TP-Link"})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	if _, err := store.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: "SN-RACE"}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
	)
	start := make(chan struct{})
	for i := 1; i <= 10; i++ {
		wg.Add(1)
		go func(subscriberID int) {
			defer wg.Done()
			<-start
			issued, err := store.IssueDevice(ctx, "SN-RACE", subscriberID)
			if err != nil {
				t.Errorf("IssueDevice: %v", err)
				return
			}
			if issued != nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Errorf("DOUBLE ISSUE: %d callers were told they got the same device, want exactly 1", winners)
	}
}

// TestFR_INV_002_SchemaRejectsIssuedWithoutSubscriber backs the application
// rule with the constraint: an "issued to nobody" row would corrupt the
// stock count FR-INV-003 relies on.
func TestFR_INV_002_SchemaRejectsIssuedWithoutSubscriber(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	store := database.Inventory()
	dt, err := store.CreateDeviceType(ctx, inventory.DeviceType{Name: "Archer C6", Vendor: "TP-Link"})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}

	_, err = pool.Exec(ctx, `
		INSERT INTO cpe_devices (device_type_id, serial_number, status)
		VALUES ($1, 'SN-BAD', 'issued')`, dt.ID)
	if err == nil {
		t.Fatal("chk_cpe_issued_has_subscriber must reject an issued device with no subscriber")
	}
}

func TestFR_INV_001_DuplicateSerialIsRejected(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	store := database.Inventory()
	dt, err := store.CreateDeviceType(ctx, inventory.DeviceType{Name: "Archer C6", Vendor: "TP-Link"})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	if _, err := store.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: "SN-DUP"}); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	// One physical router tracked as two rows would make every stock count
	// wrong from then on.
	if _, err := store.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: "SN-DUP"}); err == nil {
		t.Error("a duplicate serial number must be rejected")
	}
}

// ── FR-INV-003: stock levels and purchases ──────────────────────────────────

func TestFR_INV_003_StockLevelsFlagLowStockAtTheThreshold(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "stock@isp"})

	store := database.Inventory()
	dt, err := store.CreateDeviceType(ctx, inventory.DeviceType{
		Name: "Archer C6", Vendor: "TP-Link", ReorderThreshold: 2,
	})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	for _, sn := range []string{"SN-1", "SN-2", "SN-3"} {
		if _, err := store.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: sn}); err != nil {
			t.Fatalf("CreateDevice %s: %v", sn, err)
		}
	}

	levels, err := store.GetStockLevels(ctx, false)
	if err != nil {
		t.Fatalf("GetStockLevels: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("want 1 device type, got %d", len(levels))
	}
	if levels[0].InStock != 3 {
		t.Errorf("in_stock = %d, want 3", levels[0].InStock)
	}
	if levels[0].IsLow {
		t.Error("3 in stock against a threshold of 2 must not be flagged low")
	}

	// Issue one: 2 left, which is *at* the threshold — the boundary that
	// decides whether "reorder at 2" means 2 or 1.
	if _, err := store.IssueDevice(ctx, "SN-1", 1); err != nil {
		t.Fatalf("IssueDevice: %v", err)
	}
	levels, err = store.GetStockLevels(ctx, false)
	if err != nil {
		t.Fatalf("GetStockLevels: %v", err)
	}
	if levels[0].InStock != 2 || levels[0].Issued != 1 {
		t.Errorf("after issuing one: in_stock=%d issued=%d, want 2 and 1", levels[0].InStock, levels[0].Issued)
	}
	if !levels[0].IsLow {
		t.Error("stock at exactly the reorder threshold must be flagged low")
	}

	t.Run("low_only filters", func(t *testing.T) {
		low, err := store.GetStockLevels(ctx, true)
		if err != nil {
			t.Fatalf("GetStockLevels(low): %v", err)
		}
		if len(low) != 1 {
			t.Errorf("want the low type returned, got %d rows", len(low))
		}
	})
}

// TestFR_INV_003_StockLevelsDoNotMultiplyRows guards the same join hazard
// the franchise P&L query had: counting three statuses must not multiply the
// per-type row.
func TestFR_INV_003_StockLevelsDoNotMultiplyRows(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	store := database.Inventory()
	dt, err := store.CreateDeviceType(ctx, inventory.DeviceType{Name: "Archer C6", Vendor: "TP-Link"})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	for _, sn := range []string{"SN-1", "SN-2", "SN-3", "SN-4", "SN-5"} {
		if _, err := store.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: sn}); err != nil {
			t.Fatalf("CreateDevice: %v", err)
		}
	}

	levels, err := store.GetStockLevels(ctx, false)
	if err != nil {
		t.Fatalf("GetStockLevels: %v", err)
	}
	if len(levels) != 1 {
		t.Fatalf("5 devices of one type must report as 1 row, got %d", len(levels))
	}
	if levels[0].InStock != 5 {
		t.Errorf("in_stock = %d, want 5", levels[0].InStock)
	}
}

func TestFR_INV_003_PurchaseRecordsKeepMoneyExact(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	store := database.Inventory()
	dt, err := store.CreateDeviceType(ctx, inventory.DeviceType{Name: "Archer C6", Vendor: "TP-Link"})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}

	// 3 × 1799.99 = 5399.97 — a total that binary floating point would not
	// reproduce exactly.
	created, err := store.RecordPurchase(ctx, inventory.Purchase{
		DeviceTypeID: dt.ID, Vendor: "Ingram Micro", Quantity: 3,
		UnitCost: mustDecimal(t, "1799.99"), InvoiceRef: "INV-77", PurchasedBy: "priya.billing",
	})
	if err != nil {
		t.Fatalf("RecordPurchase: %v", err)
	}
	if created.UnitCostStr != "1799.99" {
		t.Errorf("unit_cost = %q, want 1799.99", created.UnitCostStr)
	}
	if created.TotalCostStr != "5399.97" {
		t.Errorf("total_cost = %q, want 5399.97", created.TotalCostStr)
	}

	list, err := store.ListPurchases(ctx, &dt.ID)
	if err != nil {
		t.Fatalf("ListPurchases: %v", err)
	}
	if len(list) != 1 || list[0].InvoiceRef != "INV-77" {
		t.Errorf("purchase history: %+v", list)
	}
	if !list[0].UnitCost.Equal(decimal.RequireFromString("1799.99")) {
		t.Errorf("unit cost round trip lost precision: %s", list[0].UnitCost)
	}
}
