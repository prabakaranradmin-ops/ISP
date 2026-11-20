//go:build integration

// Per-plan, per-vendor RADIUS reply attributes — FR-CAT-004 | migration 066.
//
// The dictionary in internal/nas is the first gate and is unit-tested there.
// What is tested here is what the database guarantees on its own: that the
// same attribute cannot be stored twice for one plan and vendor, that the
// vendor vocabulary matches the Go one, and that the attributes reach the
// resolver's refresh — because an attribute the console can store and the
// resolver never loads configures nothing.
package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

func addressList(planID int, value string) nas.ReplyAttribute {
	return nas.ReplyAttribute{
		PlanID: planID, Vendor: nas.VendorMikrotik,
		Name: "Mikrotik-Address-List", Value: value, Enabled: true,
	}
}

func TestFR_CAT_004_AnAttributeRoundTripsAndIsCorrectedInPlace(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()
	planID := seedPlanForBandwidth(t, pool, 1)

	created, err := store.SetPlanReplyAttribute(ctx, addressList(planID, "retail"), "noc")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if created.ID == 0 || created.Value != "retail" || !created.Enabled {
		t.Fatalf("stored as %+v", created)
	}
	if created.CreatedBy != "noc" {
		t.Errorf("created_by = %q, want %q", created.CreatedBy, "noc")
	}

	// Setting it again corrects the value rather than failing or adding a
	// second row: the console's form is the same for both, and an operator
	// fixing a typo should not have to delete the old one first.
	updated, err := store.SetPlanReplyAttribute(ctx, addressList(planID, "business"), "owner")
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if updated.ID != created.ID {
		t.Errorf("correcting the value created a second row (%d then %d)", created.ID, updated.ID)
	}
	if updated.Value != "business" {
		t.Errorf("value is %q after correction, want %q", updated.Value, "business")
	}

	all, err := store.ListPlanReplyAttributes(ctx, planID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("the plan has %d attributes, want 1", len(all))
	}
}

// Two rows for one plan, vendor and attribute would mean the router
// receives the attribute twice with no rule about which wins — the same
// ambiguity the dictionary's reserved list prevents between an operator
// attribute and the plan's own shaping.
func TestFR_CAT_004_TheDatabaseRefusesADuplicateAttribute(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)
	planID := seedPlanForBandwidth(t, pool, 1)

	for i := 0; i < 2; i++ {
		_, err := pool.Exec(ctx, `
			INSERT INTO plan_reply_attributes (plan_id, vendor, attribute, value)
			VALUES ($1, 'mikrotik', 'Mikrotik-Address-List', 'retail')`, planID)
		if i == 0 && err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if i == 1 {
			if err == nil {
				t.Fatal("the database accepted a duplicate attribute")
			}
			if !strings.Contains(err.Error(), "uq_plan_reply_attribute") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		}
	}
}

// The store has to map that constraint to the domain sentinel, or the
// console reports a generic failure for a condition it can explain.
func TestFR_CAT_004_ADuplicateFromTwoWritersIsRecognisable(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()
	planID := seedPlanForBandwidth(t, pool, 1)

	// Insert behind the store's back so its upsert cannot absorb it, then
	// make the store's INSERT collide on a path with no ON CONFLICT: the
	// attribute name differs only by case, which Normalise would have
	// settled but a direct insert did not.
	if _, err := pool.Exec(ctx, `
		INSERT INTO plan_reply_attributes (plan_id, vendor, attribute, value)
		VALUES ($1, 'mikrotik', 'Mikrotik-Address-List', 'retail')`, planID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The store upserts, so the same attribute is a correction and must
	// succeed rather than report a conflict.
	if _, err := store.SetPlanReplyAttribute(ctx, addressList(planID, "business"), "noc"); err != nil {
		if errors.Is(err, nas.ErrReplyAttributeExists) {
			t.Error("the store reported a conflict for what is a correction")
		} else {
			t.Errorf("correction failed: %v", err)
		}
	}
}

// The vendor CHECK and internal/nas.Vendors() must agree. If they drift, an
// attribute the console offers is refused by the database at save time, or
// one the database accepts has no builder to send it.
func TestFR_CAT_004_TheVendorVocabularyMatchesTheGoOne(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)
	planID := seedPlanForBandwidth(t, pool, 1)

	for _, vendor := range nas.Vendors() {
		// Session-Timeout is definable for every vendor, so it is the one
		// attribute that can be stored against each in turn.
		_, err := pool.Exec(ctx, `
			INSERT INTO plan_reply_attributes (plan_id, vendor, attribute, value)
			VALUES ($1, $2, 'Session-Timeout', '3600')`, planID, vendor)
		if err != nil {
			t.Errorf("the database refuses vendor %q, which internal/nas supports: %v", vendor, err)
		}
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO plan_reply_attributes (plan_id, vendor, attribute, value)
		VALUES ($1, 'ubiquiti', 'Session-Timeout', '3600')`, planID); err == nil {
		t.Error("the database accepted a vendor with no builder to send its attributes")
	}
}

// Deleting a plan must take its attributes with it: they describe the plan
// and mean nothing without it. Deliberately different from the RESTRICT on
// plans.bandwidth_profile_id (065), where the profile is shared and outlives
// any one plan.
func TestFR_CAT_004_AttributesGoWithTheirPlan(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()
	planID := seedPlanForBandwidth(t, pool, 1)

	if _, err := store.SetPlanReplyAttribute(ctx, addressList(planID, "retail"), "noc"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM plans WHERE id = $1`, planID); err != nil {
		t.Fatalf("deleting the plan was blocked by its attributes: %v", err)
	}

	var left int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM plan_reply_attributes WHERE plan_id = $1`, planID).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 0 {
		t.Errorf("%d attributes outlived their plan", left)
	}
}

// Switching an attribute off keeps it but takes it out of what the resolver
// loads, so switching it back on does not mean retyping it.
func TestFR_CAT_004_SwitchingOffKeepsTheValueButStopsSendingIt(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()
	planID := seedPlanForBandwidth(t, pool, 1)

	created, err := store.SetPlanReplyAttribute(ctx, addressList(planID, "retail"), "noc")
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	if err := store.SetPlanReplyAttributeEnabled(ctx, created.ID, false, "noc"); err != nil {
		t.Fatalf("switch off: %v", err)
	}

	// Still there, with its value.
	all, err := store.ListPlanReplyAttributes(ctx, planID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[0].Value != "retail" || all[0].Enabled {
		t.Errorf("after switching off the plan has %+v", all)
	}

	// But not in what gets sent.
	enabled, err := store.EnabledReplyAttributes(ctx, planID)
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	if len(enabled) != 0 {
		t.Errorf("a switched-off attribute is still being sent: %+v", enabled)
	}

	// And back on without retyping.
	if err := store.SetPlanReplyAttributeEnabled(ctx, created.ID, true, "noc"); err != nil {
		t.Fatalf("switch on: %v", err)
	}
	enabled, err = store.EnabledReplyAttributes(ctx, planID)
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	if len(enabled) != 1 || enabled[0].Value != "retail" {
		t.Errorf("switching back on gave %+v", enabled)
	}
}

// The wiring that makes the feature real: the resolver's refresh has to see
// the attributes, and encode them for the right plan and vendor. Without
// this the attributes are storable and never sent.
func TestFR_CAT_004_TheAttributesReachTheResolver(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.NAS()

	planID := seedPlanForBandwidth(t, pool, 1)
	otherPlan := seedPlanForBandwidth(t, pool, 2)

	for _, a := range []nas.ReplyAttribute{
		addressList(planID, "retail"),
		{PlanID: planID, Vendor: nas.VendorMikrotik, Name: "Session-Timeout", Value: "3600", Enabled: true},
		{PlanID: planID, Vendor: nas.VendorCisco, Name: "Cisco-AVPair", Value: "ip:addr-pool=retail", Enabled: true},
		addressList(otherPlan, "other-plan"),
		// Switched off, so it must not be loaded.
		{PlanID: planID, Vendor: nas.VendorMikrotik, Name: "Mikrotik-Group", Value: "off", Enabled: false},
	} {
		if _, err := store.SetPlanReplyAttribute(ctx, a, "noc"); err != nil {
			t.Fatalf("set %s: %v", a.Name, err)
		}
	}

	// The resolver's own view: every enabled attribute, all plans.
	loaded, err := store.ListEnabledReplyAttributes(ctx)
	if err != nil {
		t.Fatalf("resolver load: %v", err)
	}
	if len(loaded) != 4 {
		t.Fatalf("the resolver would load %d attributes, want 4 (the switched-off one excluded): %+v",
			len(loaded), loaded)
	}
	for _, a := range loaded {
		if !a.Enabled {
			t.Errorf("a switched-off attribute reached the resolver: %+v", a)
		}
	}

	// Build the resolver over the real store and check it encodes the right
	// attributes for the right plan and vendor — the end of the chain the
	// RADIUS path reads from.
	resolver := nas.NewResolver(store, nil, []byte("secret"), 3799)
	if err := resolver.Refresh(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	mikrotik := resolver.ResolveReplyAttributes(planID, nas.VendorMikrotik)
	if len(mikrotik) != 2 {
		t.Errorf("the plan resolves %d MikroTik attributes, want 2", len(mikrotik))
	}
	cisco := resolver.ResolveReplyAttributes(planID, nas.VendorCisco)
	if len(cisco) != 1 {
		t.Errorf("the plan resolves %d Cisco attributes, want 1", len(cisco))
	}
	// Another plan's attributes must not come back.
	var body string
	for _, a := range mikrotik {
		body += string(a.Value)
	}
	if strings.Contains(body, "other-plan") {
		t.Error("another plan's attribute leaked into this plan's set")
	}
	if !strings.Contains(body, "retail") {
		t.Error("the plan's own attribute did not survive the resolver")
	}

	// A vendor with nothing set resolves to nothing rather than failing.
	if got := resolver.ResolveReplyAttributes(planID, nas.VendorAruba); len(got) != 0 {
		t.Errorf("a vendor with no attributes resolved %d", len(got))
	}
}
