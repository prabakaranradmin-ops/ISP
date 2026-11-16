//go:build integration

package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// seedConsoleFixture builds one subscriber per dashboard figure, so each
// assertion below has exactly one row that should count and at least one
// near-miss that should not.
//
//	1 alice   active, expires in 3 days, online (fresh session), joined today
//	2 bob     active, expires in 20 days, online but STALE session, joined 60 days ago
//	3 carol   grace_period, expired yesterday, joined 60 days ago
//	4 dave    terminated today (churned), expired last month
//	5 erin    pending_payment, no plan expiry, joined today, phone +919811112222
func seedConsoleFixture(ctx context.Context, t *testing.T) (*staffui.DashboardCounts, func(staffui.DirectoryFilter) *staffui.DirectoryPage) {
	t.Helper()
	database, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50M", "50M/50M", 0, "", "499.00")

	day := 24 * time.Hour
	in3d, in20d := time.Now().Add(3*day), time.Now().Add(20*day)
	yesterday, lastMonth := time.Now().Add(-day), time.Now().Add(-30*day)

	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "alice", PlanExpiry: &in3d, MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "bob", PlanExpiry: &in20d, MobileNumber: "+919800000002"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "carol", Status: "grace_period", DunningState: "grace_period", PlanExpiry: &yesterday, MobileNumber: "+919800000003"})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "dave", PlanExpiry: &lastMonth, MobileNumber: "+919800000004"})
	seedSubscriber(ctx, t, pool, 5, seedOpts{Username: "erin_50%", Status: "pending_payment", MobileNumber: "+919811112222"})

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`UPDATE subscribers SET created_at = now() - interval '60 days' WHERE id IN (2, 3, 4)`)
	mustExec(`UPDATE subscribers SET email = 'carol@example.net' WHERE id = 3`)
	// A real transition, so the status-history trigger records the churn the
	// way production would, rather than the test inserting history by hand.
	mustExec(`UPDATE subscribers SET status = 'terminated' WHERE id = 4`)

	mustExec(`INSERT INTO live_sessions (subscriber_id, session_id, assigned_ip, started_at, updated_at)
	          VALUES (1, 's-alice', '100.64.0.11', now() - interval '1 hour', now()),
	                 (2, 's-bob',   '100.64.0.12', now() - interval '3 hours', now() - interval '2 hours')`)

	seedStaffUser(ctx, t, pool, 1, "csr1", "csr")
	mustExec(`INSERT INTO tickets (subscriber_id, category, description) VALUES
	            (1, 'connectivity', 'slow'),
	            (2, 'billing', 'double charge'),
	            (3, 'other', 'already fixed')`)
	mustExec(`UPDATE tickets SET assigned_to = 1 WHERE subscriber_id = 2`)
	mustExec(`UPDATE tickets SET sla_resolution_due_at = now() - interval '1 hour' WHERE subscriber_id = 1`)
	mustExec(`UPDATE tickets SET status = 'resolved' WHERE subscriber_id = 3`)

	store := database.Console()
	counts, err := store.DashboardCounts(ctx)
	if err != nil {
		t.Fatalf("DashboardCounts: %v", err)
	}
	list := func(f staffui.DirectoryFilter) *staffui.DirectoryPage {
		t.Helper()
		p, err := store.ListDirectory(ctx, f)
		if err != nil {
			t.Fatalf("ListDirectory(%+v): %v", f, err)
		}
		return p
	}
	return counts, list
}

func TestConsole_DashboardCounts(t *testing.T) {
	ctx := context.Background()
	c, _ := seedConsoleFixture(ctx, t)

	check := func(name string, got, want int) {
		t.Helper()
		if got != want {
			t.Errorf("%s: want %d, got %d", name, want, got)
		}
	}
	check("online (bob's session is stale)", c.Online, 1)
	check("total", c.Total, 5)
	check("active", c.ByStatus["active"], 2)
	check("grace_period", c.ByStatus["grace_period"], 1)
	check("terminated", c.ByStatus["terminated"], 1)
	check("pending_payment", c.ByStatus["pending_payment"], 1)
	check("soft_suspended (present even at zero)", c.ByStatus["soft_suspended"], 0)
	if _, ok := c.ByStatus["hard_suspended"]; !ok {
		t.Error("every status should be keyed, even at zero")
	}
	check("expiring this week (alice only; bob is 20 days out)", c.ExpiringWeek, 1)
	check("expired, not terminated (carol; dave is terminated)", c.Expired, 1)
	check("joined this week (alice, erin)", c.NewWeek, 2)
	check("churned in 30 days (dave)", c.Churned30d, 1)
	check("open tickets (resolved one excluded)", c.OpenTickets, 2)
	check("overdue (alice's)", c.OverdueTickets, 1)
	check("unassigned (alice's; bob's is assigned)", c.UnassignedTickets, 1)
}

func TestConsole_ListDirectory(t *testing.T) {
	ctx := context.Background()
	_, list := seedConsoleFixture(ctx, t)

	usernames := func(p *staffui.DirectoryPage) []string {
		var out []string
		for _, r := range p.Rows {
			out = append(out, r.Username)
		}
		return out
	}
	expect := func(name string, f staffui.DirectoryFilter, want ...string) *staffui.DirectoryPage {
		t.Helper()
		p := list(f)
		got := usernames(p)
		if len(got) != len(want) {
			t.Errorf("%s: want %v, got %v", name, want, got)
			return p
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: want %v, got %v", name, want, got)
				break
			}
		}
		return p
	}

	all := expect("everyone", staffui.DirectoryFilter{}, "alice", "bob", "carol", "dave", "erin_50%")
	if all.Total != 5 || all.Matched != 5 || all.Truncated {
		t.Errorf("everyone: want 5/5 untruncated, got %+v", all)
	}
	if !all.Rows[0].Online || all.Rows[1].Online {
		t.Error("alice is online; bob's session is stale and must read offline")
	}
	if all.Rows[0].PlanName != "Home 50M" {
		t.Errorf("plan name should be joined in, got %q", all.Rows[0].PlanName)
	}

	expect("status filter", staffui.DirectoryFilter{Status: "grace_period"}, "carol")
	expect("view: expiring", staffui.DirectoryFilter{View: "expiring"}, "alice")
	expect("view: expired", staffui.DirectoryFilter{View: "expired"}, "carol")
	expect("view: new", staffui.DirectoryFilter{View: "new"}, "alice", "erin_50%")
	expect("view: churned", staffui.DirectoryFilter{View: "churned"}, "dave")
	expect("view: online", staffui.DirectoryFilter{View: "online"}, "alice")

	expect("search by id", staffui.DirectoryFilter{Query: "3"}, "carol")
	expect("search by phone fragment", staffui.DirectoryFilter{Query: "98111"}, "erin_50%")
	// A full number is all digits and too large for an int4 id; it must be
	// searched as a phone number, not abort the query with "out of range".
	expect("search by full phone number", staffui.DirectoryFilter{Query: "919811112222"}, "erin_50%")
	expect("search by 10-digit number", staffui.DirectoryFilter{Query: "9811112222"}, "erin_50%")
	expect("search by email, case-insensitive", staffui.DirectoryFilter{Query: "CAROL@EXAMPLE"}, "carol")
	expect("search by CAF number", staffui.DirectoryFilter{Query: "CAF-bob"}, "bob")
	expect("search by live IP", staffui.DirectoryFilter{Query: "100.64.0.11"}, "alice")
	expect("stale session IP does not match", staffui.DirectoryFilter{Query: "100.64.0.12"})
	// "%" and "_" must be literals, not wildcards that match everyone. A bare
	// metacharacter is the query that tells the two apart: escaped it finds
	// only erin_50%, unescaped it matches every row. ("_50%" would not do —
	// no other fixture contains "50", so it finds erin either way.)
	expect("% is literal", staffui.DirectoryFilter{Query: "%"}, "erin_50%")
	expect("_ is literal", staffui.DirectoryFilter{Query: "_"}, "erin_50%")
	expect("search and filter combine", staffui.DirectoryFilter{Query: "a", Status: "active"}, "alice")

	none := expect("no match still reports the total", staffui.DirectoryFilter{Query: "zzz"})
	if none.Total != 5 || none.Matched != 0 {
		t.Errorf("no match: want total 5 matched 0, got %+v", none)
	}

	capped := list(staffui.DirectoryFilter{Limit: 2})
	if len(capped.Rows) != 2 || capped.Matched != 5 || !capped.Truncated {
		t.Errorf("limit 2: want 2 rows of 5 matched and Truncated, got %d rows %+v", len(capped.Rows), capped)
	}
}

func TestConsole_ListDirectoryRejectsUnknownFilters(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Console()
	if _, err := store.ListDirectory(ctx, staffui.DirectoryFilter{Status: "zombie"}); err == nil {
		t.Error("an unknown status must be an error, not 'everyone'")
	}
	if _, err := store.ListDirectory(ctx, staffui.DirectoryFilter{View: "vip"}); err == nil {
		t.Error("an unknown view must be an error, not 'everyone'")
	}
}

func TestConsole_Sessions(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50M", "50M/50M", 0, "", "499.00")
	in30d := time.Now().Add(30 * 24 * time.Hour)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "alice", PlanExpiry: &in30d, MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "bob", PlanExpiry: &in30d, MobileNumber: "+919800000002"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "carol", PlanExpiry: &in30d, MobileNumber: "+919800000003"})

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// alice fresh and throttled, bob fresh, carol stale (Accounting-Stop lost).
	mustExec(`INSERT INTO live_sessions (subscriber_id, session_id, nas_ip, assigned_ip, bytes_in, bytes_out,
	                                     fup_throttled, started_at, updated_at) VALUES
	            (1, 's-alice', '10.0.0.1', '100.64.0.11', 10, 20, true,  now() - interval '1 hour', now()),
	            (2, 's-bob',   '10.0.0.2', '100.64.0.12', 30, 40, false, now() - interval '2 hours', now()),
	            (3, 's-carol', '10.0.0.1', '100.64.0.13', 50, 60, false, now() - interval '5 hours', now() - interval '3 hours')`)

	// Completed history, kept inside the current month so each row routes to
	// an existing partition (see seed_local.sql on why that matters).
	base := `GREATEST(date_trunc('month', now()) + interval '1 hour', now() - interval '%s')`
	ins := func(sub int, sid, startAgo, stopAgo string, stopped bool) {
		t.Helper()
		stop := "NULL"
		if stopped {
			stop = fmt.Sprintf(base, stopAgo)
		}
		mustExec(fmt.Sprintf(`INSERT INTO subscriber_session_history
		    (subscriber_id, session_id, nas_ip_address, assigned_ipv4, start_time, stop_time,
		     input_octets, output_octets, terminate_cause)
		    VALUES ($1, $2, '10.0.0.1'::inet, '100.64.0.21'::inet, %s, %s, 100, 200, 'User-Request')`,
			fmt.Sprintf(base, startAgo), stop), sub, sid)
	}
	ins(1, "h-alice-old", "50 minutes", "40 minutes", true)
	ins(2, "h-bob-old", "30 minutes", "20 minutes", true)
	ins(1, "h-alice-open", "10 minutes", "", false)

	store := database.Console()
	names := func(ls []staffui.LiveSession) []string {
		var out []string
		for _, l := range ls {
			out = append(out, l.Username)
		}
		return out
	}
	live := func(f staffui.SessionFilter) []string {
		t.Helper()
		ls, err := store.ListLiveSessions(ctx, f)
		if err != nil {
			t.Fatalf("ListLiveSessions(%+v): %v", f, err)
		}
		return names(ls)
	}
	eq := func(name string, got []string, want ...string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: want %v, got %v", name, want, got)
		}
	}

	eq("live, newest first, stale excluded", live(staffui.SessionFilter{}), "alice", "bob")
	eq("FUP only", live(staffui.SessionFilter{FUPOnly: true}), "alice")
	eq("by assigned IP", live(staffui.SessionFilter{Query: "100.64.0.12"}), "bob")
	eq("by NAS IP", live(staffui.SessionFilter{Query: "10.0.0.1"}), "alice")
	eq("by username fragment", live(staffui.SessionFilter{Query: "BO"}), "bob")
	eq("stale session's IP finds nothing", live(staffui.SessionFilter{Query: "100.64.0.13"}))
	eq("limit", live(staffui.SessionFilter{Limit: 1}), "alice")

	window := staffui.SessionFilter{From: time.Now().Add(-2 * time.Hour), To: time.Now().Add(time.Minute)}
	cs, err := store.ListCompletedSessions(ctx, window)
	if err != nil {
		t.Fatalf("ListCompletedSessions: %v", err)
	}
	var sids []string
	for _, c := range cs {
		sids = append(sids, c.SessionID)
	}
	// The month-start clamp can collapse both rows onto one instant early on
	// the 1st; only assert order when they really differ.
	if len(sids) != 2 || (sids[0] != "h-bob-old" && cs[0].StopTime.After(cs[1].StopTime)) {
		t.Errorf("completed: want the two stopped sessions, most recent first; got %v", sids)
	}
	if len(cs) > 0 && (cs[0].NasIP != "10.0.0.1" || cs[0].AssignedIP != "100.64.0.21" || cs[0].TerminateCause != "User-Request") {
		t.Errorf("completed row fields not mapped: %+v", cs[0])
	}

	byUser := window
	byUser.Query = "alice"
	cs, err = store.ListCompletedSessions(ctx, byUser)
	if err != nil || len(cs) != 1 || cs[0].SessionID != "h-alice-old" {
		t.Errorf("completed by user: want only h-alice-old (the open one is not completed), got %+v err=%v", cs, err)
	}

	future := staffui.SessionFilter{From: time.Now().Add(time.Hour), To: time.Now().Add(2 * time.Hour)}
	if cs, err := store.ListCompletedSessions(ctx, future); err != nil || len(cs) != 0 {
		t.Errorf("a window with nothing in it: want none, got %+v err=%v", cs, err)
	}
	if _, err := store.ListCompletedSessions(ctx, staffui.SessionFilter{}); err == nil {
		t.Error("a missing window must be an error, not an unbounded scan")
	}
}

func TestConsole_TicketQueue(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50M", "50M/50M", 0, "", "499.00")
	in30d := time.Now().Add(30 * 24 * time.Hour)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "alice", PlanExpiry: &in30d, MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "bob", PlanExpiry: &in30d, MobileNumber: "+919800000002"})
	seedStaffUser(ctx, t, pool, 7, "csr1", "csr")
	seedStaffUser(ctx, t, pool, 8, "tech1", "technician")
	seedStaffUser(ctx, t, pool, 9, "bill1", "billing_admin")

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// ids 1..5 in insertion order.
	mustExec(`INSERT INTO tickets (subscriber_id, category, description) VALUES
	            (1, 'connectivity', 'router keeps rebooting'),   -- 1: overdue, unassigned
	            (2, 'billing', 'charged twice'),                  -- 2: critical, assigned to csr1
	            (1, 'other', 'change wifi name'),                 -- 3: low, assigned to tech1
	            (2, 'connectivity', 'slow speeds at night'),      -- 4: resolved after 10h
	            (1, 'billing', 'invoice copy 50%_off')            -- 5: resolved after 20h`)
	mustExec(`UPDATE tickets SET priority = 'high', sla_resolution_due_at = now() - interval '1 hour' WHERE id = 1`)
	mustExec(`UPDATE tickets SET priority = 'critical', assigned_to = 7, sla_resolution_due_at = now() + interval '2 hours' WHERE id = 2`)
	mustExec(`UPDATE tickets SET priority = 'low', assigned_to = 8, sla_resolution_due_at = now() + interval '3 days' WHERE id = 3`)
	mustExec(`UPDATE tickets SET created_at = now() - interval '10 hours' WHERE id = 4`)
	mustExec(`UPDATE tickets SET created_at = now() - interval '20 hours' WHERE id = 5`)
	// Real transitions, so the history trigger records the close time.
	mustExec(`UPDATE tickets SET status = 'resolved' WHERE id IN (4, 5)`)

	store := database.Console()
	ids := func(f staffui.TicketFilter) []int {
		t.Helper()
		ts, err := store.ListTicketQueue(ctx, f)
		if err != nil {
			t.Fatalf("ListTicketQueue(%+v): %v", f, err)
		}
		var out []int
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}
	eq := func(name string, got []int, want ...int) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: want %v, got %v", name, want, got)
		}
	}

	// Overdue first, then priority, then deadline.
	eq("open (default), attention order", ids(staffui.TicketFilter{}), 1, 2, 3)
	eq("overdue", ids(staffui.TicketFilter{View: "overdue"}), 1)
	eq("unassigned", ids(staffui.TicketFilter{View: "unassigned"}), 1)
	eq("mine (csr1)", ids(staffui.TicketFilter{View: "mine", StaffUsername: "csr1"}), 2)
	eq("closed in 30 days", len2(ids(staffui.TicketFilter{View: "closed"})), 2)
	eq("all", len2(ids(staffui.TicketFilter{View: "all"})), 5)
	eq("priority filter", ids(staffui.TicketFilter{Priority: "critical"}), 2)
	eq("one subscriber", ids(staffui.TicketFilter{SubscriberID: 2}), 2)
	eq("search description", ids(staffui.TicketFilter{View: "all", Query: "WIFI"}), 3)
	eq("search username", ids(staffui.TicketFilter{Query: "bob"}), 2)
	eq("search ticket id", ids(staffui.TicketFilter{View: "all", Query: "4"}), 4)
	eq("LIKE metacharacters are literal", ids(staffui.TicketFilter{View: "all", Query: "%_"}), 5)

	ts, err := store.ListTicketQueue(ctx, staffui.TicketFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !ts[0].Overdue || ts[1].Overdue || ts[1].AssigneeName != "csr1" || ts[0].Username != "alice" {
		t.Errorf("row fields wrong: %+v / %+v", ts[0], ts[1])
	}

	if _, err := store.ListTicketQueue(ctx, staffui.TicketFilter{View: "vip"}); err == nil {
		t.Error("unknown view must be an error")
	}
	if _, err := store.ListTicketQueue(ctx, staffui.TicketFilter{Priority: "urgent"}); err == nil {
		t.Error("unknown priority must be an error")
	}

	sum, err := store.HelpdeskSummary(ctx)
	if err != nil {
		t.Fatalf("HelpdeskSummary: %v", err)
	}
	if sum.Open != 3 || sum.Overdue != 1 || sum.Unassigned != 1 || sum.Closed30d != 2 {
		t.Errorf("summary counts: %+v", sum)
	}
	if sum.MedianCloseHours == nil || *sum.MedianCloseHours < 14.9 || *sum.MedianCloseHours > 15.1 {
		t.Errorf("median of 10h and 20h should be 15h, got %v", sum.MedianCloseHours)
	}

	as, err := store.ListTicketAssignees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		if a.Role == "billing_admin" {
			t.Error("a billing admin cannot work tickets and must not be offered as an assignee")
		}
	}
	if len(as) != 2 {
		t.Errorf("want csr1 and tech1, got %+v", as)
	}
}

// len2 turns a slice into its length, for asserting counts through eq.
func len2(xs []int) []int { return []int{len(xs)} }

func TestConsole_InvoicesAndPayments(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50M", "50M/50M", 0, "", "499.00")
	seedGstRate(ctx, t, pool, 1)
	in30d := time.Now().Add(30 * 24 * time.Hour)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "alice", PlanExpiry: &in30d, MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "=bob", PlanExpiry: &in30d, MobileNumber: "+919800000002"})

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Three invoices in the window, one 40 days before it.
	mustExec(`INSERT INTO invoices (id, subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount,
	                                total_amount, gst_rate_id, gb_included, gb_used, created_at) VALUES
	            (101, 1, 499.00, 44.91, 44.91, 0, 588.82, 1, 0, 0, now() - interval '2 days'),
	            (102, 2, 799.00, 71.91, 71.91, 0, 942.82, 1, 0, 0, now() - interval '1 day'),
	            (103, 1, 100.00, 0, 0, 18.00, 118.00, 1, 0, 0, now() - interval '3 hours'),
	            (104, 1, 499.00, 44.91, 44.91, 0, 588.82, 1, 0, 0, now() - interval '40 days')`)
	mustExec(`INSERT INTO credit_notes (invoice_id, subscriber_id, base_amount, cgst_amount, sgst_amount,
	                                    igst_amount, total_amount, reason)
	          VALUES (102, 2, 100.00, 9.00, 9.00, 0, 118.00, 'overcharge')`)

	// Payments are subscriber_wallet credits with no staff actor. Everything
	// else here is a near-miss that must not be counted.
	mustExec(`INSERT INTO wallet_ledgers (subscriber_id, account, entry_type, amount, balance_after,
	                                      transaction_token, description, adjusted_by_username, created_at) VALUES
	   (1, 'subscriber_wallet',        'credit', 500.00, 500.00, 'pay_A', 'recharge via razorpay webhook', NULL, now() - interval '2 days'),
	   (1, 'payment_gateway_clearing', 'debit',  500.00, 500.00, NULL, 'counter-entry: recharge via razorpay webhook', NULL, now() - interval '2 days'),
	   (2, 'subscriber_wallet',        'credit', 300.00, 300.00, NULL, 'recharge via cash', NULL, now() - interval '1 hour'),
	   (2, 'subscriber_wallet',        'credit',  50.00, 350.00, NULL, 'staff adjustment: goodwill', 'owner', now() - interval '30 minutes'),
	   (1, 'subscriber_wallet',        'debit',  499.00,   1.00, NULL, 'renewal', NULL, now() - interval '20 minutes'),
	   (1, 'subscriber_wallet',        'credit', 999.00, 999.00, 'pay_OLD', 'recharge via razorpay webhook', NULL, now() - interval '40 days')`)

	store := database.Console()
	window := staffui.MoneyFilter{From: time.Now().Add(-7 * 24 * time.Hour), To: time.Now().Add(time.Minute)}

	inv, tot, err := store.ListInvoices(ctx, window)
	if err != nil {
		t.Fatalf("ListInvoices: %v", err)
	}
	var ids []int
	for _, i := range inv {
		ids = append(ids, i.ID)
	}
	if fmt.Sprint(ids) != "[103 102 101]" {
		t.Errorf("invoices in window, newest first: want [103 102 101], got %v", ids)
	}
	if tot.Count != 3 || tot.Base != "1398.00" || tot.Tax != "251.64" || tot.Total != "1649.64" || tot.Credited != "118.00" {
		t.Errorf("totals over the window: %+v", tot)
	}
	if len(inv) == 3 && (inv[1].Credited != "118.00" || inv[0].Credited != "0" && inv[0].Credited != "0.00") {
		t.Errorf("credited per invoice: %+v", inv)
	}

	one := func(q string) []int {
		t.Helper()
		f := window
		f.Query = q
		rows, _, err := store.ListInvoices(ctx, f)
		if err != nil {
			t.Fatalf("ListInvoices(%q): %v", q, err)
		}
		var out []int
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}
	if got := one("INV-000102"); fmt.Sprint(got) != "[102]" {
		t.Errorf("by invoice number: %v", got)
	}
	if got := one("inv-102"); fmt.Sprint(got) != "[102]" {
		t.Errorf("by lower-case invoice number: %v", got)
	}
	if got := one("=bo"); fmt.Sprint(got) != "[102]" {
		t.Errorf("by username fragment: %v", got)
	}
	if got := one("1"); fmt.Sprint(got) != "[103 101]" {
		t.Errorf("by subscriber id 1: %v", got)
	}

	capped := window
	capped.Limit = 1
	if rows, tot, err := store.ListInvoices(ctx, capped); err != nil || len(rows) != 1 || tot.Count != 3 || tot.Total != "1649.64" {
		t.Errorf("a capped page must still total everything: rows=%d tot=%+v err=%v", len(rows), tot, err)
	}

	pay, ptot, err := store.ListPayments(ctx, window)
	if err != nil {
		t.Fatalf("ListPayments: %v", err)
	}
	if len(pay) != 2 || ptot.Count != 2 || ptot.Amount != "800.00" {
		t.Fatalf("payments: want the two recharges totalling 800.00, got %+v %+v", pay, ptot)
	}
	if pay[0].Method != "cash" || pay[1].Method != "razorpay webhook" || pay[1].Reference != "pay_A" {
		t.Errorf("method/reference mapping: %+v", pay)
	}
	byRef := window
	byRef.Query = "pay_A"
	if rows, _, err := store.ListPayments(ctx, byRef); err != nil || len(rows) != 1 || rows[0].Amount != "500.00" {
		t.Errorf("by reference: %+v err=%v", rows, err)
	}

	if _, _, err := store.ListInvoices(ctx, staffui.MoneyFilter{}); err == nil {
		t.Error("a missing window must be an error, not an unbounded scan")
	}
}

func TestConsole_ListACSDevices(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50M", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "alice", MobileNumber: "+919800000001"})

	inv := database.Inventory()
	dt, err := inv.CreateDeviceType(ctx, inventory.DeviceType{Name: "HG8245", Vendor: "Huawei"})
	if err != nil {
		t.Fatalf("CreateDeviceType: %v", err)
	}
	for _, sn := range []string{"SN-FRESH", "SN-SILENT", "SN-FAULT", "SN-STOCK"} {
		if _, err := inv.CreateDevice(ctx, inventory.Device{DeviceTypeID: dt.ID, SerialNumber: sn}); err != nil {
			t.Fatalf("CreateDevice %s: %v", sn, err)
		}
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`UPDATE cpe_devices SET last_inform_at = now() - interval '1 hour', provisioning_state = 'provisioned',
	                 product_class = 'HG8245H', status = 'issued', subscriber_id = 1 WHERE serial_number = 'SN-FRESH'`)
	mustExec(`UPDATE cpe_devices SET last_inform_at = now() - interval '3 days', provisioning_state = 'provisioned'
	           WHERE serial_number = 'SN-SILENT'`)
	mustExec(`UPDATE cpe_devices SET last_inform_at = now() - interval '2 hours', provisioning_state = 'fault'
	           WHERE serial_number = 'SN-FAULT'`)
	fresh := scanString(ctx, t, pool, `SELECT id::text FROM cpe_devices WHERE serial_number = 'SN-FRESH'`)
	mustExec(`INSERT INTO cpe_tasks (device_id, rpc_type, params, status, priority, created_by) VALUES
	            ($1::int, 'Reboot', '{}', 'pending', 10, 'noc'),
	            ($1::int, 'Reboot', '{}', 'completed', 10, 'noc')`, fresh)

	store := database.Console()
	list := func(view, q string) []string {
		t.Helper()
		rows, err := store.ListACSDevices(ctx, view, q, 0)
		if err != nil {
			t.Fatalf("ListACSDevices(%q,%q): %v", view, q, err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.SerialNumber)
		}
		return out
	}
	eq := func(name string, got []string, want ...string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: want %v, got %v", name, want, got)
		}
	}
	eq("managed, most recent check-in first", list("", ""), "SN-FRESH", "SN-FAULT", "SN-SILENT")
	eq("silent", list("silent", ""), "SN-SILENT")
	eq("fault", list("fault", ""), "SN-FAULT")
	eq("all includes never-connected stock last", list("all", ""), "SN-FRESH", "SN-FAULT", "SN-SILENT", "SN-STOCK")
	eq("search by subscriber", list("managed", "ALI"), "SN-FRESH")
	eq("search by model", list("managed", "8245h"), "SN-FRESH")

	rows, err := store.ListACSDevices(ctx, "managed", "SN-FRESH", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("one row: %+v %v", rows, err)
	}
	if rows[0].Username != "alice" || rows[0].PendingTasks != 1 || rows[0].Silent {
		t.Errorf("fresh row: want alice, 1 pending (completed excluded), not silent; got %+v", rows[0])
	}
	if rows, _ := store.ListACSDevices(ctx, "silent", "", 0); len(rows) != 1 || !rows[0].Silent {
		t.Errorf("the silent row must carry Silent=true: %+v", rows)
	}
	if _, err := store.ListACSDevices(ctx, "bogus", "", 0); err == nil {
		t.Error("an unknown view must be an error")
	}
}

func TestConsole_CountAnnouncementAudience(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	seedPlan(ctx, t, pool, 1, "Home 50M", "50M/50M", 0, "", "499.00")
	seedPlan(ctx, t, pool, 2, "Home 100M", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a", PlanID: 1, MobileNumber: "+919800000001"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b", PlanID: 2, MobileNumber: "+919800000002"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "c", PlanID: 2, Status: "pending_payment", MobileNumber: "+919800000003"})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "d", PlanID: 2, Status: "terminated", MobileNumber: "+919800000004"})

	store := database.Console()
	count := func(plan *int, status *string) int {
		t.Helper()
		n, err := store.CountAnnouncementAudience(ctx, nil, plan, status)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	two, active := 2, "active"
	if n := count(nil, nil); n != 3 {
		t.Errorf("everyone except terminated: want 3, got %d", n)
	}
	if n := count(&two, nil); n != 2 {
		t.Errorf("plan 2 (terminated excluded): want 2, got %d", n)
	}
	if n := count(&two, &active); n != 1 {
		t.Errorf("plan 2, active: want 1, got %d", n)
	}
}
