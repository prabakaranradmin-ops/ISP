//go:build integration

// Field payment collection — FR-COL-001 | migration 068.
//
// What is tested here is what only a real database can show: that the round
// selects the right subscribers in the right order, that an assignment
// survives a collector's account being deleted in the way the schema
// promises, and — most importantly — that the takings a collector is asked
// to hand over are computed from the same rows the subscriber's receipt came
// from, rather than from a second record of the same money.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

func collectionFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
}

// assignable returns the id of a seeded staff account usable as a collector.
func assignable(ctx context.Context, t *testing.T, pool *pgxpool.Pool, username, role string) int {
	t.Helper()
	var id int
	err := pool.QueryRow(ctx, `
		INSERT INTO staff_users (username, password_hash, role, full_name)
		VALUES ($1, '$2a$12$seedhash', $2, $1)
		RETURNING id`, username, role).Scan(&id)
	if err != nil {
		t.Fatalf("seed staff %q: %v", username, err)
	}
	return id
}

func TestFR_COL_001_TheRoundSelectsTheRightSubscribersInVisitOrder(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	collectionFixture(ctx, t, pool)

	ravi := assignable(ctx, t, pool, "ravi", "technician")
	other := assignable(ctx, t, pool, "sunil", "technician")

	lapsed := time.Now().Add(-72 * time.Hour)
	soon := time.Now().Add(48 * time.Hour)
	later := time.Now().Add(40 * 24 * time.Hour)

	// 1 lapsed, on ravi's round        -> visit first
	// 2 due in 2 days, on ravi's round -> visit second
	// 3 due in 40 days, ravi           -> outside a 7-day window
	// 4 lapsed but on sunil's round    -> not ravi's problem
	// 5 lapsed, unassigned             -> nobody's round
	// 6 lapsed but terminated, ravi    -> they have left; never visit
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "lapsed@isp", PlanExpiry: &lapsed})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "soon@isp", PlanExpiry: &soon})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "later@isp", PlanExpiry: &later})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "theirs@isp", PlanExpiry: &lapsed})
	seedSubscriber(ctx, t, pool, 5, seedOpts{Username: "nobody@isp", PlanExpiry: &lapsed})
	seedSubscriber(ctx, t, pool, 6, seedOpts{Username: "gone@isp", Status: "terminated"})

	for _, id := range []int{1, 2, 3, 6} {
		if err := store.AssignCollector(ctx, id, &ravi); err != nil {
			t.Fatalf("assign %d: %v", id, err)
		}
	}
	if err := store.AssignCollector(ctx, 4, &other); err != nil {
		t.Fatalf("assign 4: %v", err)
	}

	round, err := store.ListCollectionRound(ctx, ravi, 7)
	if err != nil {
		t.Fatalf("round: %v", err)
	}
	if len(round) != 2 {
		t.Fatalf("round has %d subscribers, want 2 (lapsed and due soon): %+v", len(round), round)
	}
	// Soonest expiry first, so a collector works the off-service customers
	// before the ones who are merely approaching renewal.
	if round[0].Username != "lapsed@isp" {
		t.Errorf("first to visit is %q, want lapsed@isp", round[0].Username)
	}
	if round[1].Username != "soon@isp" {
		t.Errorf("second to visit is %q, want soon@isp", round[1].Username)
	}
	// The plan price and balance both come back, because the amount to ask
	// for is derived from them rather than stored.
	if !round[0].PlanPrice.Equal(decimal.RequireFromString("799.00")) {
		t.Errorf("plan price came back as %s", round[0].PlanPrice)
	}

	// A terminated subscriber must never appear, whatever the window.
	wide, err := store.ListCollectionRound(ctx, ravi, 90)
	if err != nil {
		t.Fatalf("wide round: %v", err)
	}
	for _, c := range wide {
		if c.Username == "gone@isp" {
			t.Error("a terminated subscriber is on a collection round")
		}
	}
	if len(wide) != 3 {
		t.Errorf("a 90-day window has %d subscribers, want 3", len(wide))
	}

	// Zero days means lapsed only, which is a meaningful view and not the
	// same as "no filter".
	lapsedOnly, err := store.ListCollectionRound(ctx, ravi, 0)
	if err != nil {
		t.Fatalf("lapsed-only round: %v", err)
	}
	if len(lapsedOnly) != 1 || lapsedOnly[0].Username != "lapsed@isp" {
		t.Errorf("the lapsed-only view returned %+v", lapsedOnly)
	}

	// A never-billed subscriber owes their first payment, so they belong on
	// the round whatever the window — but they sort last, because they are
	// often a signup from this morning and putting every new customer above
	// the people actually cut off would bury the urgent ones.
	t.Run("a never-billed subscriber is on the round but sorts last", func(t *testing.T) {
		seedSubscriber(ctx, t, pool, 7, seedOpts{
			Username: "never@isp", Status: "pending_payment",
		})
		if err := store.AssignCollector(ctx, 7, &ravi); err != nil {
			t.Fatalf("assign: %v", err)
		}

		round, err := store.ListCollectionRound(ctx, ravi, 0)
		if err != nil {
			t.Fatalf("round: %v", err)
		}
		var names []string
		for _, c := range round {
			names = append(names, c.Username)
		}
		if len(round) != 2 {
			t.Fatalf("lapsed-only round is %v, want the lapsed one and the never-billed one", names)
		}
		if round[0].Username != "lapsed@isp" {
			t.Errorf("round order is %v; the cut-off customer must come first", names)
		}
		if round[1].Username != "never@isp" {
			t.Errorf("round order is %v; the never-billed one must come last", names)
		}
		if round[1].PlanExpiry != nil {
			t.Errorf("never@isp came back with an expiry of %v", round[1].PlanExpiry)
		}
	})
}

// The assignment is a permission, so reading it back has to be exact.
func TestFR_COL_001_CollectorForIsTheAssignmentCheck(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	collectionFixture(ctx, t, pool)

	ravi := assignable(ctx, t, pool, "ravi", "technician")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "assigned@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "unassigned@isp"})

	if err := store.AssignCollector(ctx, 1, &ravi); err != nil {
		t.Fatalf("assign: %v", err)
	}

	got, err := store.CollectorFor(ctx, 1)
	if err != nil {
		t.Fatalf("collector for 1: %v", err)
	}
	if got == nil || *got != ravi {
		t.Errorf("subscriber 1 resolves to %v, want %d", got, ravi)
	}

	// Unassigned must be nil rather than zero: a zero would compare equal
	// to nothing and quietly refuse everybody, or worse, match a staff id
	// of zero if one ever existed.
	got, err = store.CollectorFor(ctx, 2)
	if err != nil {
		t.Fatalf("collector for 2: %v", err)
	}
	if got != nil {
		t.Errorf("an unassigned subscriber resolves to %v, want nil", got)
	}

	// Clearing it takes them off the round.
	if err := store.AssignCollector(ctx, 1, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err = store.CollectorFor(ctx, 1)
	if err != nil || got != nil {
		t.Errorf("after clearing, subscriber 1 resolves to %v (err %v)", got, err)
	}

	if _, err := store.CollectorFor(ctx, 999999); err == nil {
		t.Error("an unknown subscriber did not report not-found")
	}
}

// The schema promises a collector who leaves can be deleted, their
// subscribers falling back to unassigned rather than blocking it — and that
// their cash trail survives, because that trail lives in payment_details
// and not in the staff row.
func TestFR_COL_001_DeletingACollectorFreesTheirRoundAndKeepsTheirTrail(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	collectionFixture(ctx, t, pool)

	ravi := assignable(ctx, t, pool, "ravi", "technician")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "theirs@isp", Balance: "0.00"})
	if err := store.AssignCollector(ctx, 1, &ravi); err != nil {
		t.Fatalf("assign: %v", err)
	}

	// A payment ravi took.
	seedWalletCredit(ctx, t, pool, 1, "799.00", billing.MethodCash, "ravi")

	if _, err := pool.Exec(ctx, `DELETE FROM staff_users WHERE id = $1`, ravi); err != nil {
		t.Fatalf("deleting a collector was blocked by their round: %v", err)
	}

	// The subscriber is unassigned, not orphaned.
	got, err := store.CollectorFor(ctx, 1)
	if err != nil {
		t.Fatalf("collector for 1: %v", err)
	}
	if got != nil {
		t.Errorf("after deleting the collector, subscriber 1 resolves to %v, want nil", got)
	}

	// And the cash is still attributed to them.
	takings, err := store.CollectorTakings(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("takings: %v", err)
	}
	var found bool
	for _, tk := range takings {
		if tk.Collector == "ravi" {
			found = true
			if !tk.CashTotal().Equal(decimal.RequireFromString("799.00")) {
				t.Errorf("ravi's cash came back as %s, want 799.00", tk.CashTotal())
			}
		}
	}
	if !found {
		t.Error("deleting the staff account erased the cash trail, which is the one thing it must not do")
	}
}

// The reconciliation figure: what each collector took, split by method, read
// from the same rows the receipt came from.
func TestFR_COL_001_TakingsSplitByMethodAndExcludeMoneyLeaving(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	collectionFixture(ctx, t, pool)

	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp", Balance: "0.00"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b@isp", Balance: "0.00"})

	seedWalletCredit(ctx, t, pool, 1, "799.00", billing.MethodCash, "ravi")
	seedWalletCredit(ctx, t, pool, 2, "1200.00", billing.MethodCash, "ravi")
	seedWalletCredit(ctx, t, pool, 1, "500.00", billing.MethodUPI, "ravi")
	seedWalletCredit(ctx, t, pool, 2, "300.00", billing.MethodCash, "sunil")
	// A refund is money leaving. Seeded with a POSITIVE amount, because
	// that is how wallet_ledgers stores it — the direction lives in
	// entry_type. A negative fixture here would let a takings query that
	// filtered on the sign of the amount pass while counting refunds as
	// money taken in production.
	seedWalletDebit(ctx, t, pool, 1, "200.00", billing.MethodCash, "ravi")

	takings, err := store.CollectorTakings(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("takings: %v", err)
	}

	byWho := map[string]billing.CollectorTakings{}
	for _, tk := range takings {
		byWho[tk.Collector] = tk
	}

	ravi, ok := byWho["ravi"]
	if !ok {
		t.Fatalf("ravi has no takings: %+v", takings)
	}
	if !ravi.CashTotal().Equal(decimal.RequireFromString("1999.00")) {
		t.Errorf("ravi's cash is %s, want 1999.00 (799 + 1200, the refund not netted off)",
			ravi.CashTotal())
	}
	if !ravi.Total.Equal(decimal.RequireFromString("2499.00")) {
		t.Errorf("ravi's total is %s, want 2499.00", ravi.Total)
	}
	if got := ravi.ByMethod[billing.MethodUPI]; !got.Equal(decimal.RequireFromString("500.00")) {
		t.Errorf("ravi's UPI is %s, want 500.00", got)
	}
	if ravi.Count != 3 {
		t.Errorf("ravi recorded %d payments, want 3", ravi.Count)
	}

	// A bounced cheque is money that never arrived, so it must not appear
	// in what a collector is asked to account for. Its original credit is
	// still in the ledger (migration 054 bounces with a reversal rather
	// than flipping a status and leaving the money in), so the credit-only
	// filter alone would keep it.
	t.Run("a bounced cheque is not counted as taken", func(t *testing.T) {
		ledgerID := seedWalletCredit(ctx, t, pool, 1, "5000.00", billing.MethodCheque, "ravi")
		if _, err := pool.Exec(ctx, `
			UPDATE payment_details
			   SET status = 'bounced', bounced_at = NOW(), bounce_reason = 'insufficient funds',
			       bounce_ledger_id = $1
			 WHERE wallet_ledger_id = $1`, ledgerID); err != nil {
			t.Fatalf("bounce: %v", err)
		}

		again, err := store.CollectorTakings(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("takings: %v", err)
		}
		for _, tk := range again {
			if tk.Collector != "ravi" {
				continue
			}
			if got := tk.ByMethod[billing.MethodCheque]; !got.IsZero() {
				t.Errorf("a bounced cheque counts %s toward ravi's takings", got)
			}
			if !tk.Total.Equal(decimal.RequireFromString("2499.00")) {
				t.Errorf("ravi's total is %s after a bounce, want 2499.00 unchanged", tk.Total)
			}
		}
	})

	sunil, ok := byWho["sunil"]
	if !ok {
		t.Fatal("sunil has no takings")
	}
	if !sunil.CashTotal().Equal(decimal.RequireFromString("300.00")) {
		t.Errorf("sunil's cash is %s, want 300.00 — collectors must not be mixed up", sunil.CashTotal())
	}
}

// Takings are bounded by the period asked for, or a day's hand-in would
// include last week's money.
func TestFR_COL_001_TakingsAreBoundedByThePeriod(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	collectionFixture(ctx, t, pool)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp", Balance: "0.00"})

	seedWalletCredit(ctx, t, pool, 1, "799.00", billing.MethodCash, "ravi")
	// Backdated a week.
	if _, err := pool.Exec(ctx, `
		UPDATE wallet_ledgers SET created_at = NOW() - interval '7 days'
		 WHERE subscriber_id = 1`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	today := time.Now()
	start := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())
	takings, err := store.CollectorTakings(ctx, start, start.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("takings: %v", err)
	}
	if len(takings) != 0 {
		t.Errorf("last week's money appears in today's hand-in: %+v", takings)
	}
}

func TestFR_COL_001_ListCollectorsCountsLoadAndIncludesTheUnloaded(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	collectionFixture(ctx, t, pool)

	ravi := assignable(ctx, t, pool, "ravi", "technician")
	assignable(ctx, t, pool, "idle", "technician")

	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b@isp"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "gone@isp", Status: "terminated"})
	for _, id := range []int{1, 2, 3} {
		if err := store.AssignCollector(ctx, id, &ravi); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}

	collectors, err := store.ListCollectors(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	byName := map[string]billing.CollectorLoad{}
	for _, c := range collectors {
		byName[c.Username] = c
	}
	// Terminated subscribers are not a load: nobody is going to visit them.
	if got := byName["ravi"].Assigned; got != 2 {
		t.Errorf("ravi's load is %d, want 2 (the terminated one excluded)", got)
	}
	// Staff with nobody assigned must still be listed, or somebody could
	// never be given their first subscriber.
	if _, ok := byName["idle"]; !ok {
		t.Error("a collector with nobody assigned is missing from the list")
	}
	if got := byName["idle"].Assigned; got != 0 {
		t.Errorf("an unloaded collector reports %d assigned", got)
	}
}

func TestFR_COL_001_StaffIDForUsernameResolvesTheSession(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Billing()
	collectionFixture(ctx, t, pool)

	ravi := assignable(ctx, t, pool, "ravi", "technician")
	got, err := store.StaffIDForUsername(ctx, "ravi")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != ravi {
		t.Errorf("resolved %d, want %d", got, ravi)
	}
	if _, err := store.StaffIDForUsername(ctx, "nobody"); err == nil {
		t.Error("an unknown username resolved to a staff id")
	}
}
