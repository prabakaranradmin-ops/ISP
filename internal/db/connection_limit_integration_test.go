//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// Connection limits against a real database — FR-AAA-002, FR-BIL-022 |
// migration 077. Every plan includes one connection; each active unit of a
// product marked adds_connections allows one more, billed by the ordinary
// renewal.

func extraConnectionProduct(ctx context.Context, t *testing.T, b interface {
	CreateProduct(context.Context, billing.Product, string) (*billing.Product, error)
}, code string, adds bool) *billing.Product {
	t.Helper()
	p, err := b.CreateProduct(ctx, billing.Product{
		Code: code, Name: code, Kind: billing.ProductRecurring,
		Price: decimal.RequireFromString("199.00"), HSNSAC: "998432", AddsConnections: adds,
	}, "meena")
	if err != nil {
		t.Fatalf("create %s: %v", code, err)
	}
	return p
}

func TestFR_BIL_022_AnExtraConnectionIsARecurringProduct(t *testing.T) {
	ctx := context.Background()
	d, pool := newTestDB(t)
	b := d.Billing()

	p := extraConnectionProduct(ctx, t, b, "EXTRA-CONN", true)
	if !p.AddsConnections {
		t.Fatal("the flag did not round-trip")
	}
	if got, err := b.GetProduct(ctx, p.ID); err != nil || !got.AddsConnections {
		t.Errorf("GetProduct %+v %v", got, err)
	}

	// Refused in Go, and by the database for anything that skips Go.
	_, err := b.CreateProduct(ctx, billing.Product{Code: "ONE-OFF-CONN", Name: "x", Kind: billing.ProductOneTime,
		Price: decimal.RequireFromString("99.00"), HSNSAC: "998432", AddsConnections: true}, "meena")
	if billing.ProductInvalidReason(err) == "" {
		t.Errorf("a one-time product that adds connections: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO products (code, name, kind, price, hsn_sac, created_by, adds_connections)
		VALUES ('RAW-CONN', 'x', 'one_time', 99, '998432', 'x', TRUE)`); err == nil {
		t.Error("chk_product_adds_connections let a one-time product add connections")
	}

	// Editing the product never changes what it grants.
	p.Name, p.AddsConnections = "Extra connection (renamed)", false
	updated, err := b.UpdateProduct(ctx, *p, "ravi")
	if err != nil {
		t.Fatal(err)
	}
	if !updated.AddsConnections {
		t.Error("an edit took the flag off; every subscriber holding it would lose their extra connection")
	}
}

func TestFR_AAA_002_TheLimitIsOnePlusActiveExtraConnections(t *testing.T) {
	ctx := context.Background()
	d, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50", "50M/50M", 0, "", "500.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "family", MobileNumber: "+919820000301"})
	b := d.Billing()
	limit := func() int {
		t.Helper()
		sub, err := d.Radius().GetSubscriberByUsername(ctx, "family")
		if err != nil || sub == nil {
			t.Fatalf("lookup: %v", err)
		}
		return sub.ConnectionLimit
	}

	if got := limit(); got != billing.IncludedConnections {
		t.Fatalf("no extras: limit %d, want %d", got, billing.IncludedConnections)
	}
	extra := extraConnectionProduct(ctx, t, b, "EXTRA-CONN", true)
	rental := extraConnectionProduct(ctx, t, b, "ROUTER-RENT", false)

	two, err := b.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: extra.ID, Quantity: 2}, "meena")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: rental.ID, Quantity: 3}, "meena"); err != nil {
		t.Fatal(err)
	}
	if got := limit(); got != 3 {
		t.Errorf("two extra connections and a router rental: limit %d, want 3", got)
	}

	// Ending the charge ends the connections it bought.
	if err := b.EndAttachment(ctx, two.ID, "downgraded", "ravi"); err != nil {
		t.Fatal(err)
	}
	if got := limit(); got != 1 {
		t.Errorf("after ending the extras: limit %d, want 1", got)
	}
}

func TestFR_AAA_002_OpenConnectionsCountLinesNotDevicesOrRedials(t *testing.T) {
	ctx := context.Background()
	d, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50", "50M/50M", 0, "", "500.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sharing", MobileNumber: "+919820000302"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "neighbour", MobileNumber: "+919820000303"})
	s := d.FUP()
	const home, second = "D4:CA:6D:00:00:01", "D4:CA:6D:00:00:02"

	for _, st := range []radius.SessionStart{
		{SubscriberID: 1, SessionID: "home", NASIP: "10.0.0.1", AssignedIP: "203.0.113.4", DeviceMAC: home},
		{SubscriberID: 1, SessionID: "second", NASIP: "10.0.0.2", AssignedIP: "100.64.0.20", DeviceMAC: second},
		{SubscriberID: 1, SessionID: "old", NASIP: "10.0.0.1"}, // before migration 077: no device
		{SubscriberID: 1, SessionID: "phone", NASIP: "10.0.0.5", DeviceMAC: "AA:BB:CC:DD:EE:01", MAB: true},
		{SubscriberID: 1, SessionID: "ended", NASIP: "10.0.0.1", DeviceMAC: home},
		{SubscriberID: 2, SessionID: "theirs", NASIP: "10.0.0.1", DeviceMAC: "D4:CA:6D:00:00:09"},
	} {
		if err := s.OpenSession(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.StopSession(ctx, "ended", 0, 0, "User-Request"); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		except string
		want   int
	}{
		"every password line":        {"", 3},
		"the home router redialling": {home, 2},
		"a router nobody has seen":   {"D4:CA:6D:00:00:77", 3},
	} {
		if n, err := s.OpenConnections(ctx, 1, tc.except); err != nil || n != tc.want {
			t.Errorf("%s: %d %v, want %d", name, n, err, tc.want)
		}
	}

	held, err := s.StaticAddressHeldElsewhere(ctx, 1, "203.0.113.4", second)
	if err != nil || !held {
		t.Errorf("the second router asking about the address the home router holds: %v %v", held, err)
	}
	if held, _ := s.StaticAddressHeldElsewhere(ctx, 1, "203.0.113.4", home); held {
		t.Error("the router that holds the address was told someone else does")
	}
	if held, _ := s.StaticAddressHeldElsewhere(ctx, 2, "203.0.113.4", "D4:CA:6D:00:00:09"); held {
		t.Error("another subscriber's session counted as holding this one's address")
	}
}

func TestFR_AAA_002_TheReportListsOnlyThoseOverTheirLimit(t *testing.T) {
	ctx := context.Background()
	d, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50", "50M/50M", 0, "", "500.00")
	for id, name := range map[int]string{1: "sharing", 2: "paid-for-two", 3: "one-line", 4: "phones"} {
		seedSubscriber(ctx, t, pool, id, seedOpts{Username: name, MobileNumber: "+91982000040" + string(rune('0'+id))})
	}
	b := d.Billing()
	extra := extraConnectionProduct(ctx, t, b, "EXTRA-CONN", true)
	if _, err := b.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 2, ProductID: extra.ID}, "meena"); err != nil {
		t.Fatal(err)
	}
	s := d.FUP()
	open := func(sub int, id string, mab bool) {
		t.Helper()
		if err := s.OpenSession(ctx, radius.SessionStart{SubscriberID: sub, SessionID: id, NASIP: "10.0.0.1", MAB: mab}); err != nil {
			t.Fatal(err)
		}
	}
	open(1, "s1-a", false)
	open(1, "s1-b", false)
	open(1, "s1-c", false) // three lines on one: over by two
	open(2, "s2-a", false)
	open(2, "s2-b", false) // two lines, paid for two: fine
	open(3, "s3-a", false) // one line: fine
	open(4, "s4-a", false)
	open(4, "s4-phone", true)
	open(4, "s4-tablet", true) // hotspot devices do not count

	got, err := s.ListOverConnectionLimit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("report %+v; want only the subscriber running three lines on one", got)
	}
	if got[0].Username != "sharing" || got[0].Limit != 1 || len(got[0].Sessions) != 3 || got[0].Over() != 2 {
		t.Errorf("report row %+v", got[0])
	}

	// Selling them two extras clears them from the report.
	if _, err := b.AttachProduct(ctx, billing.AttachRequest{SubscriberID: 1, ProductID: extra.ID, Quantity: 2}, "meena"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ListOverConnectionLimit(ctx); err != nil || len(got) != 0 {
		t.Errorf("after selling the extras: %+v %v", got, err)
	}
}
