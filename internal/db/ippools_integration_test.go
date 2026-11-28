//go:build integration

// IP pools and static addresses — FR-NET-005 | migration 060.
//
// The properties only a real database shows: one live holder per address
// under concurrency, allocation that skips exclusions and addresses held
// from other pools, the catalogue charge attached and ended together with
// the address (FR-BIL-022), and the static address reaching the RADIUS
// hot-path query.
package db_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
	"github.com/shopspring/decimal"
)

func mustRange(t *testing.T, first, last string) nas.AddrRange {
	t.Helper()
	r, err := nas.ParseAddrRange(first, last)
	if err != nil {
		t.Fatalf("range %q-%q: %v", first, last, err)
	}
	return r
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("addr %q: %v", s, err)
	}
	return a.Unmap()
}

// poolFixture seeds a plan, a subscriber and a /29 static pool with its
// network, gateway and broadcast excluded.
func poolFixture(ctx context.Context, t *testing.T, database *db.DB, pool *pgxpool.Pool) (*db.NASStore, nas.Pool) {
	t.Helper()
	seedPlan(ctx, t, pool, 1, "Business", "100M/100M", 0, "", "2000.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "biz", Balance: "0.00", MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "biz2", Balance: "0.00", MobileNumber: "+919800000002"})

	store := database.NAS()
	p, err := store.CreatePool(ctx, nas.Pool{
		Name: "Customer statics", Purpose: nas.PoolStatic,
		Range: mustRange(t, "203.0.113.0/29", ""),
	}, "meena")
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	if _, err := store.AddExclusion(ctx, p.ID,
		mustRange(t, "203.0.113.0", "203.0.113.1"), "network and gateway", "meena"); err != nil {
		t.Fatalf("exclude network: %v", err)
	}
	if _, err := store.AddExclusion(ctx, p.ID,
		mustRange(t, "203.0.113.7", ""), "broadcast", "meena"); err != nil {
		t.Fatalf("exclude broadcast: %v", err)
	}
	reloaded, err := store.GetPool(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, *reloaded
}

func TestFR_NET_005_PoolLifecycle(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store, p := poolFixture(ctx, t, database, pool)

	if len(p.Exclusions) != 2 {
		t.Fatalf("want 2 exclusions, got %d", len(p.Exclusions))
	}
	if p.SizeLabel() != "8" {
		t.Errorf("a /29 reports size %q", p.SizeLabel())
	}
	if !p.Range.Contains(mustAddr(t, "203.0.113.4")) {
		t.Error("the stored range does not contain an address inside it")
	}

	t.Run("the name is unique per router, case-insensitively", func(t *testing.T) {
		_, err := store.CreatePool(ctx, nas.Pool{
			Name: "customer STATICS", Purpose: nas.PoolStatic,
			Range: mustRange(t, "198.51.100.0/29", ""),
		}, "meena")
		if !errors.Is(err, nas.ErrPoolNameTaken) {
			t.Fatalf("want ErrPoolNameTaken, got %v", err)
		}
	})

	t.Run("the database refuses a network address as a pool bound", func(t *testing.T) {
		// Written straight to SQL to confirm chk_ip_pool_hosts is really
		// there, not just the Go-side parsing.
		_, err := pool.Exec(ctx, `
			INSERT INTO ip_pools (name, purpose, first_address, last_address, created_by)
			VALUES ('masked', 'static', '203.0.113.0/24', '203.0.113.255/24', 'meena')`)
		if err == nil {
			t.Fatal("the database accepted a masked address as a pool bound")
		}
	})

	t.Run("the database refuses a reversed range", func(t *testing.T) {
		_, err := pool.Exec(ctx, `
			INSERT INTO ip_pools (name, purpose, first_address, last_address, created_by)
			VALUES ('backwards', 'static', '203.0.113.20', '203.0.113.10', 'meena')`)
		if err == nil {
			t.Fatal("the database accepted a reversed range")
		}
	})

	t.Run("an exclusion outside the pool is refused", func(t *testing.T) {
		_, err := store.AddExclusion(ctx, p.ID,
			mustRange(t, "198.51.100.1", ""), "wrong pool", "meena")
		if !errors.Is(err, nas.ErrPoolInvalid) {
			t.Fatalf("want ErrPoolInvalid, got %v", err)
		}
	})

	t.Run("an exclusion needs a reason", func(t *testing.T) {
		_, err := store.AddExclusion(ctx, p.ID, mustRange(t, "203.0.113.3", ""), "  ", "meena")
		if !errors.Is(err, nas.ErrPoolInvalid) {
			t.Fatalf("want ErrPoolInvalid, got %v", err)
		}
	})

	t.Run("a pool nothing has used can be deleted", func(t *testing.T) {
		spare, err := store.CreatePool(ctx, nas.Pool{
			Name: "Spare", Purpose: nas.PoolDHCP, Range: mustRange(t, "10.0.0.0/24", ""),
		}, "meena")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeletePool(ctx, spare.ID); err != nil {
			t.Fatalf("delete unused pool: %v", err)
		}
		if _, err := store.GetPool(ctx, spare.ID); !errors.Is(err, nas.ErrPoolNotFound) {
			t.Errorf("want ErrPoolNotFound, got %v", err)
		}
	})

	t.Run("listing filters by purpose", func(t *testing.T) {
		if _, err := store.CreatePool(ctx, nas.Pool{
			Name: "Router DHCP", Purpose: nas.PoolDHCP, Range: mustRange(t, "10.1.0.0/24", ""),
		}, "meena"); err != nil {
			t.Fatal(err)
		}
		statics, err := store.ListPools(ctx, nas.PoolFilter{Purpose: nas.PoolStatic})
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range statics {
			if got.Purpose != nas.PoolStatic {
				t.Errorf("a %s pool came back in a static-only listing", got.Purpose)
			}
		}
		all, err := store.ListPools(ctx, nas.PoolFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(all) <= len(statics) {
			t.Errorf("unfiltered listing returned %d, static-only %d", len(all), len(statics))
		}
		// Exclusions come along, because capacity is meaningless without
		// them and the allocator needs them.
		for _, got := range all {
			if got.ID == p.ID && len(got.Exclusions) != 2 {
				t.Errorf("the listed pool carries %d exclusions", len(got.Exclusions))
			}
		}
	})
}

func TestFR_NET_005_AllocationSkipsExclusionsAndHolders(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store, p := poolFixture(ctx, t, database, pool)

	// .0 and .1 are excluded, so the first free address is .2.
	first, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: p.ID, SubscriberID: 1}, "meena")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if first.Address != mustAddr(t, "203.0.113.2") {
		t.Errorf("first allocation gave %s, want 203.0.113.2", first.Address)
	}
	if first.Username != "biz" || first.PoolName != "Customer statics" {
		t.Errorf("assignment reads %+v", first)
	}
	if first.Billed() {
		t.Error("an unbilled allocation reports itself billed")
	}

	// The next subscriber gets .3, not .2 again.
	second, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: p.ID, SubscriberID: 2}, "meena")
	if err != nil {
		t.Fatalf("second allocate: %v", err)
	}
	if second.Address != mustAddr(t, "203.0.113.3") {
		t.Errorf("second allocation gave %s, want 203.0.113.3", second.Address)
	}

	t.Run("a subscriber holds at most one address", func(t *testing.T) {
		_, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: p.ID, SubscriberID: 1}, "meena")
		if !errors.Is(err, nas.ErrSubscriberHasAddress) {
			t.Fatalf("want ErrSubscriberHasAddress, got %v", err)
		}
	})

	t.Run("a specific free address can be asked for", func(t *testing.T) {
		seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "biz3", Balance: "0.00", MobileNumber: "+919800000003"})
		got, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{
			PoolID: p.ID, SubscriberID: 3, Address: mustAddr(t, "203.0.113.6"),
		}, "meena")
		if err != nil {
			t.Fatalf("allocate a specific address: %v", err)
		}
		if got.Address != mustAddr(t, "203.0.113.6") {
			t.Errorf("got %s", got.Address)
		}
	})

	t.Run("an excluded address is refused even when asked for by name", func(t *testing.T) {
		seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "biz4", Balance: "0.00", MobileNumber: "+919800000004"})
		_, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{
			PoolID: p.ID, SubscriberID: 4, Address: mustAddr(t, "203.0.113.1"),
		}, "meena")
		if !errors.Is(err, nas.ErrAddressExcluded) {
			t.Fatalf("want ErrAddressExcluded, got %v", err)
		}
	})

	t.Run("an address outside the pool is refused", func(t *testing.T) {
		_, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{
			PoolID: p.ID, SubscriberID: 4, Address: mustAddr(t, "198.51.100.5"),
		}, "meena")
		if !errors.Is(err, nas.ErrAddressOutsidePool) {
			t.Fatalf("want ErrAddressOutsidePool, got %v", err)
		}
	})

	t.Run("an address already held is refused", func(t *testing.T) {
		_, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{
			PoolID: p.ID, SubscriberID: 4, Address: mustAddr(t, "203.0.113.2"),
		}, "meena")
		if !errors.Is(err, nas.ErrAddressTaken) {
			t.Fatalf("want ErrAddressTaken, got %v", err)
		}
	})

	t.Run("a non-static pool is never allocated from", func(t *testing.T) {
		dhcp, err := store.CreatePool(ctx, nas.Pool{
			Name: "DHCP", Purpose: nas.PoolDHCP, Range: mustRange(t, "10.2.0.0/24", ""),
		}, "meena")
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: dhcp.ID, SubscriberID: 4}, "meena")
		if !errors.Is(err, nas.ErrPoolNotStatic) {
			t.Fatalf("want ErrPoolNotStatic, got %v", err)
		}
	})

	t.Run("an exhausted pool says so", func(t *testing.T) {
		// .2, .3 and .6 are held; take .4 and .5 and the pool is full.
		for i, sub := range []int{5, 6} {
			seedSubscriber(ctx, t, pool, sub, seedOpts{
				Username: "fill" + string(rune('a'+i)), Balance: "0.00",
				MobileNumber: "+91980000001" + string(rune('0'+i)),
			})
			if _, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: p.ID, SubscriberID: sub}, "meena"); err != nil {
				t.Fatalf("filling the pool: %v", err)
			}
		}
		seedSubscriber(ctx, t, pool, 7, seedOpts{Username: "toolate", Balance: "0.00", MobileNumber: "+919800000017"})
		_, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: p.ID, SubscriberID: 7}, "meena")
		if !errors.Is(err, nas.ErrPoolExhausted) {
			t.Fatalf("want ErrPoolExhausted, got %v", err)
		}
	})

	t.Run("a pool with live assignments cannot be deleted", func(t *testing.T) {
		if err := store.DeletePool(ctx, p.ID); !errors.Is(err, nas.ErrPoolInUse) {
			t.Fatalf("want ErrPoolInUse, got %v", err)
		}
	})
}

// An address held from one pool must not be handed out by another pool
// that happens to overlap it — the collision this feature exists to stop.
func TestFR_NET_005_OverlappingPoolsDoNotHandOutTheSameAddress(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store, p := poolFixture(ctx, t, database, pool)

	held, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{
		PoolID: p.ID, SubscriberID: 1, Address: mustAddr(t, "203.0.113.2"),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}

	// A second pool covering the same addresses, created by operator error.
	//
	// It starts at the address already held, deliberately: a pool starting
	// anywhere else would hand out its own first address and the test
	// would pass whether or not held addresses from other pools are
	// considered — which is exactly how an earlier version of this test
	// passed with the cross-pool check removed.
	overlap, err := store.CreatePool(ctx, nas.Pool{
		Name: "Overlapping", Purpose: nas.PoolStatic,
		Range: mustRange(t, "203.0.113.2", "203.0.113.15"),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}

	// Asking that pool for its next free address must skip the one already
	// held through the first pool, even though this pool knows nothing
	// about it.
	got, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: overlap.ID, SubscriberID: 2}, "meena")
	if err != nil {
		t.Fatalf("allocate from the overlapping pool: %v", err)
	}
	if got.Address == held.Address {
		t.Fatalf("both pools handed out %s", got.Address)
	}
	if got.Address != mustAddr(t, "203.0.113.3") {
		t.Errorf("got %s, want 203.0.113.3 — the next one after the address already held", got.Address)
	}

	// And asking for the held address by name is refused from here too.
	seedSubscriber(ctx, t, pool, 30, seedOpts{Username: "thirdparty", Balance: "0.00", MobileNumber: "+919830000001"})
	_, err = store.AllocateStaticIP(ctx, nas.AllocateRequest{
		PoolID: overlap.ID, SubscriberID: 30, Address: held.Address,
	}, "meena")
	if !errors.Is(err, nas.ErrAddressTaken) {
		t.Fatalf("want ErrAddressTaken for an address held via another pool, got %v", err)
	}
}

// Two operators allocating at the same moment must not both get one
// address.
func TestFR_NET_005_ConcurrentAllocationsDoNotCollide(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store, p := poolFixture(ctx, t, database, pool)

	const n = 4
	for i := 0; i < n; i++ {
		seedSubscriber(ctx, t, pool, 10+i, seedOpts{
			Username:     "race" + string(rune('a'+i)),
			Balance:      "0.00",
			MobileNumber: "+91981000000" + string(rune('0'+i)),
		})
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		addrs []netip.Addr
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(sub int) {
			defer wg.Done()
			got, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: p.ID, SubscriberID: sub}, "meena")
			if err != nil {
				return
			}
			mu.Lock()
			addrs = append(addrs, got.Address)
			mu.Unlock()
		}(10 + i)
	}
	wg.Wait()

	if len(addrs) != n {
		t.Fatalf("%d of %d concurrent allocations succeeded; the pool has room for all of them", len(addrs), n)
	}
	seen := map[netip.Addr]bool{}
	for _, a := range addrs {
		if seen[a] {
			t.Errorf("%s was handed out twice", a)
		}
		seen[a] = true
	}
	// And the database agrees: one live row per address.
	if got := scanString(ctx, t, pool, `
		SELECT (COUNT(*) = COUNT(DISTINCT address))::text
		  FROM static_ip_assignments WHERE status = 'active'`); got != "true" {
		t.Error("the same address is held by more than one live assignment")
	}
}

// "Billed through FR-BIL-022": the address is charged as an ordinary
// catalogue product, and releasing it ends that charge in the same
// transaction.
func TestFR_NET_005_StaticAddressIsBilledThroughTheProductCatalogue(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store, p := poolFixture(ctx, t, database, pool)
	billingStore := database.Billing()

	product, err := billingStore.CreateProduct(ctx, billing.Product{
		Code: "STATIC-IP", Name: "Static IP address", Kind: billing.ProductRecurring,
		HSNSAC: "998432", Price: decimal.RequireFromString("200.00"),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{
		PoolID: p.ID, SubscriberID: 1, ProductID: product.ID,
	}, "meena")
	if err != nil {
		t.Fatalf("allocate with a charge: %v", err)
	}
	if !got.Billed() {
		t.Fatal("the assignment does not record its charge")
	}

	// The charge is a real attachment the renewal path will bill.
	billable, err := billingStore.ListBillableProducts(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(billable) != 1 || billable[0].ProductCode != "STATIC-IP" {
		t.Fatalf("billable charges are %+v", billable)
	}
	if !billable[0].UnitPrice.Equal(decimal.RequireFromString("200.00")) {
		t.Errorf("the charge is priced %s, want the catalogue 200.00", billable[0].UnitPrice)
	}

	t.Run("releasing the address ends the charge", func(t *testing.T) {
		released, err := store.ReleaseStaticIP(ctx, got.ID, "customer downgraded", "ravi")
		if err != nil {
			t.Fatalf("release: %v", err)
		}
		// The released row names whose session still holds the address,
		// so the caller can end it before the address goes to anyone else.
		if released.ID != got.ID || released.SubscriberID != 1 || released.Username != "biz" ||
			released.Address != got.Address || released.Status != nas.AssignmentReleased ||
			released.ReleaseReason != "customer downgraded" || released.ReleasedBy != "ravi" {
			t.Errorf("released row %+v", released)
		}
		after, err := billingStore.ListBillableProducts(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != 0 {
			t.Errorf("the static IP charge is still billable after release: %+v", after)
		}
		// With its reason recorded, so a later question about the invoice
		// is answerable.
		all, err := billingStore.ListSubscriberProducts(ctx, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 || all[0].Status != billing.AttachmentEnded {
			t.Fatalf("history is %+v", all)
		}
		if !strings.Contains(all[0].EndReason, "customer downgraded") {
			t.Errorf("end reason is %q", all[0].EndReason)
		}
	})

	t.Run("the address is free again", func(t *testing.T) {
		reused, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{
			PoolID: p.ID, SubscriberID: 2, Address: got.Address,
		}, "meena")
		if err != nil {
			t.Fatalf("reallocate a released address: %v", err)
		}
		if reused.Address != got.Address {
			t.Errorf("got %s, want the released %s", reused.Address, got.Address)
		}
	})

	t.Run("releasing twice is refused, and needs a reason", func(t *testing.T) {
		if _, err := store.ReleaseStaticIP(ctx, got.ID, "again", "ravi"); !errors.Is(err, nas.ErrAssignmentNotActive) {
			t.Fatalf("want ErrAssignmentNotActive, got %v", err)
		}
		if _, err := store.ReleaseStaticIP(ctx, 999999, "x", "ravi"); !errors.Is(err, nas.ErrAssignmentNotFound) {
			t.Fatalf("want ErrAssignmentNotFound, got %v", err)
		}
		if _, err := store.ReleaseStaticIP(ctx, got.ID, "  ", "ravi"); !errors.Is(err, nas.ErrPoolInvalid) {
			t.Fatalf("a release with no reason: want ErrPoolInvalid, got %v", err)
		}
	})

	t.Run("an unbilled address releases without complaint", func(t *testing.T) {
		seedSubscriber(ctx, t, pool, 20, seedOpts{Username: "nobill", Balance: "0.00", MobileNumber: "+919820000001"})
		a, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: p.ID, SubscriberID: 20}, "meena")
		if err != nil {
			t.Fatal(err)
		}
		if r, err := store.ReleaseStaticIP(ctx, a.ID, "no longer needed", "ravi"); err != nil || r.Username != "nobill" {
			t.Errorf("releasing an unbilled address: %+v %v", r, err)
		}
	})
}

// The address has to reach the RADIUS hot-path query, or nothing on the
// network changes however correct the tables are.
func TestFR_NET_005_StaticAddressReachesTheAuthLookup(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store, p := poolFixture(ctx, t, database, pool)

	radiusStore := database.Radius()

	before, err := radiusStore.GetSubscriberByUsername(ctx, "biz")
	if err != nil {
		t.Fatal(err)
	}
	if before == nil {
		t.Fatal("no subscriber")
	}
	if before.StaticIP != "" {
		t.Errorf("a subscriber with no static address reports %q", before.StaticIP)
	}

	assigned, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{
		PoolID: p.ID, SubscriberID: 1, Address: mustAddr(t, "203.0.113.4"),
	}, "meena")
	if err != nil {
		t.Fatal(err)
	}

	after, err := radiusStore.GetSubscriberByUsername(ctx, "biz")
	if err != nil {
		t.Fatal(err)
	}
	// host() strips the mask INET carries: "203.0.113.4", not
	// "203.0.113.4/32", which net.ParseIP would reject.
	if after.StaticIP != "203.0.113.4" {
		t.Errorf("the auth lookup reports %q, want \"203.0.113.4\"", after.StaticIP)
	}

	t.Run("a released address stops being returned", func(t *testing.T) {
		if _, err := store.ReleaseStaticIP(ctx, assigned.ID, "released", "ravi"); err != nil {
			t.Fatal(err)
		}
		got, err := radiusStore.GetSubscriberByUsername(ctx, "biz")
		if err != nil {
			t.Fatal(err)
		}
		if got.StaticIP != "" {
			t.Errorf("a released address is still returned: %q", got.StaticIP)
		}
	})
}

func TestFR_NET_005_SubscriberAndPoolListings(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store, p := poolFixture(ctx, t, database, pool)

	a, err := store.AllocateStaticIP(ctx, nas.AllocateRequest{PoolID: p.ID, SubscriberID: 1}, "meena")
	if err != nil {
		t.Fatal(err)
	}

	current, err := store.GetSubscriberStaticIP(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.ID != a.ID {
		t.Fatalf("GetSubscriberStaticIP gave %+v", current)
	}

	none, err := store.GetSubscriberStaticIP(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if none != nil {
		t.Errorf("a subscriber with no address gave %+v", none)
	}

	if _, err := store.ReleaseStaticIP(ctx, a.ID, "moved house", "ravi"); err != nil {
		t.Fatal(err)
	}
	history, err := store.ListSubscriberStaticIPs(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Status != nas.AssignmentReleased {
		t.Errorf("history is %+v", history)
	}
	if history[0].ReleaseReason != "moved house" || history[0].ReleasedBy != "ravi" {
		t.Errorf("the release was not attributed: %+v", history[0])
	}

	holders, err := store.ListPoolAssignments(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0].Username != "biz" {
		t.Errorf("pool holders are %+v", holders)
	}
}
