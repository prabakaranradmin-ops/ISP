//go:build integration

// Shared harness for the persistence-layer integration tests.
//
// These run against a real PostgreSQL with the real migrations applied: the
// whole point of this layer is the SQL, so a fake would test nothing. Bring the
// database up with ./scripts/run_db_tests.sh, which sets TEST_DB_DSN.
package db_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/shopspring/decimal"
)

// Tables are truncated in FK-dependency order so a test starts from a known
// empty state regardless of what ran before it.
var truncateOrder = []string{
	// Migration 069. Listed because nothing else truncated here would reach
	// subscriber_groups: groups would accumulate across tests until two of
	// them collided on uq_subscriber_group_name_active, which would fail
	// whichever test ran second rather than the one at fault.
	//
	// Order against announcements does not matter — announcements has an FK
	// into subscriber_groups, so CASCADE truncates it from here too, and
	// ON DELETE RESTRICT does not apply to TRUNCATE CASCADE. It is listed
	// first only to keep this slice's parents-last reading true.
	// subscriber_group_members is reached by CASCADE from either parent.
	// Migration 072. A single row nothing references, so CASCADE never
	// reaches it — and a branding row left by one test would make every later
	// test that renders the captive portal see a venue's name instead of the
	// defaults it asserts.
	"captive_portal_branding",
	"subscriber_group_members",
	"subscriber_groups",
	"lea_audit_log",
	// Migration 062. Nothing truncated here references it, so CASCADE
	// never reaches it and entries would accumulate across tests —
	// listed for the same reason products and cpe_device_types are.
	//
	// TRUNCATE works because the harness connects as the owner; bss_app,
	// which the services run as, is refused (see
	// TestFR_OBS_006_TheTrailIsAppendOnlyAgainstTheApplicationRole).
	"staff_audit_log",
	// Migration 063. Nothing truncated here references it, so it would
	// otherwise accumulate across tests — and this is the one table in
	// the suite that takes a row per authentication.
	"radius_auth_log",
	// Migration 064. References subscribers, so CASCADE would reach it —
	// listed for the order this slice documents, and because a leaked row
	// would show up in another test's reconciliation counts.
	"payment_gateway_log",
	"collections_forecast",
	"revenue_snapshots",
	"lco_ledger",
	"cgnat_allocations",
	"subscriber_session_history",
	// sla_events before tickets: CASCADE would take it anyway, but listing
	// it keeps the dependency order this slice documents actually true.
	//
	// The SLA lookup tables (sla_policies, category_priority_defaults,
	// ticket_routing_rules) are deliberately absent: migration 023 seeds
	// them and every ticket-creation path reads them, so truncating them
	// would break ticket creation everywhere rather than isolate a test.
	// Migration 033. webhook_deliveries before its parents; api_keys last of
	// the three since both others reference it.
	"webhook_deliveries",
	"webhook_endpoints",
	"api_keys",
	// Migration 034. grants and vouchers before nas_devices/subscribers.
	"hotspot_grants",
	"hotspot_vouchers",
	"hotspot_devices",
	"document_archives",
	"sla_events",
	// Migration 031. Both cascade from their parents, but they are listed
	// before them so the order this slice documents stays true — and so a
	// reader can see that capture history is reset between tests rather than
	// accumulating across them.
	"ticket_status_history",
	"subscriber_status_history",
	"tickets",
	"notification_log",
	"notification_templates",
	// Workflow (migration 026) and CRM/inventory (027). Most of these would
	// be taken by CASCADE from subscribers anyway, but cpe_device_types
	// references nothing that is truncated, so it would survive between
	// tests and collide on its unique name — listing them all keeps the
	// dependency order this slice documents actually true.
	"payment_refunds",
	"approval_requests",
	"field_tasks",
	"leads",
	// Migration 028. announcements references franchises/plans and would
	// survive a subscribers-only cascade, so it is listed explicitly for the
	// same reason cpe_device_types is.
	"announcements",
	"subscriber_push_tokens",
	"cpe_tasks",
	"cpe_purchases",
	"cpe_devices",
	"cpe_device_types",
	// Migration 042. Listed from migration 071 onwards, not before: nothing
	// truncated here has ever referenced purchase_orders, so CASCADE never
	// reached it and orders simply accumulated — harmless while the table was
	// standalone. Now that every order holds an ON DELETE RESTRICT reference
	// into vendors, a leaked order from one test blocks a vendor delete in
	// another, which fails for a reason that test cannot see.
	"purchase_orders",
	// Migration 071, after the three tables that reference it so this slice's
	// parents-last reading stays true. TRUNCATE ... CASCADE would take those
	// three from here anyway (CASCADE follows the references, and ON DELETE
	// RESTRICT does not apply to TRUNCATE), but vendors has to be listed in
	// its own right: nothing else truncated here references it, so suppliers
	// would accumulate across tests until two collided on
	// uq_vendors_name_active — failing whichever test ran second rather than
	// the one at fault, which is the exact failure subscriber_groups is
	// listed to prevent.
	"vendors",
	// billing_runs has no foreign key to anything truncated here, so
	// CASCADE would never reach it (migration 057).
	"billing_run_failures",
	"billing_runs",
	"wallet_ledgers",
	// Before invoices and gst_rates, which it references (migration 056).
	"proforma_invoices",
	// Migration 058, before invoices and subscribers, which both reference.
	// products is listed explicitly and not left to CASCADE: nothing
	// truncated here references it, so a catalogue entry would survive
	// between tests and the next test to create INSTALL would collide on
	// products_code_key — exactly what cpe_device_types is listed for.
	// invoice_lines and subscriber_products come first so the dependency
	// order this slice documents stays true.
	"invoice_lines",
	"subscriber_products",
	"products",
	// Migration 059. References subscribers, plans and invoices, so
	// CASCADE would reach it — listed anyway, for the reason this slice
	// documents its own order, and because a leaked pending row would
	// collide on uq_spc_one_pending in the next test to schedule one.
	"scheduled_plan_changes",
	// Migration 060. Assignments before pools (ON DELETE RESTRICT would
	// otherwise block the truncate rather than cascade), and ip_pools
	// before nas_devices, which it references.
	"static_ip_assignments",
	"ip_pool_exclusions",
	"ip_pools",
	"invoices",
	"gst_rates",
	"kyc_verifications",
	// Migration 046. CASCADE from subscribers would take it anyway, but it
	// is listed for the same reason cpe_device_types and announcements are:
	// so the dependency order this slice documents stays true, and so a
	// reader can see verifier state is reset between tests rather than
	// leaking a cached verifier from one test into the next.
	"radius_verifier_cache",
	"subscribers",
	"plans",
	// Migration 067. data_topups references subscribers so CASCADE reaches
	// it, but it is listed so a test that only touches top-ups starts
	// clean. fup_policies comes after plans, like bandwidth_profiles, since
	// plans.fup_policy_id references it; its tiers go by CASCADE.
	"data_topups",
	// Migration 066. Before plans would be taken by its own CASCADE
	// anyway, but listed so a test that only touches attributes still
	// starts clean.
	"plan_reply_attributes",
	// Migration 065. After plans, because plans.bandwidth_profile_id
	// references it: listed earlier, the CASCADE would reach plans and
	// everything behind it before the tables above had been cleared in
	// their intended order.
	"bandwidth_profiles",
	"fup_policies",
	// Migration 034 seeds nas_devices with explicit ids, so it must be reset
	// between tests or the second test to seed id=1 collides. plan_nas_profiles
	// references it and is taken by CASCADE.
	"nas_devices",
	// After tickets: tickets.assigned_to references it (migration 023).
	"staff_users",
	"franchises",
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set — run via ./scripts/run_db_tests.sh")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestDB returns a DB against a freshly emptied schema.
func newTestDB(t *testing.T) (*db.DB, *pgxpool.Pool) {
	t.Helper()
	pool := testPool(t)
	truncateAll(t, pool)
	return db.New(pool), pool
}

func truncateAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, table := range truncateOrder {
		if _, err := pool.Exec(ctx, "TRUNCATE TABLE "+table+" RESTART IDENTITY CASCADE"); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	restoreSeededReferenceData(t, pool)
}

// restoreSeededReferenceData puts back reference rows that a migration seeds
// and TRUNCATE ... CASCADE removes as collateral.
//
// ticket_routing_rules has an FK to franchises, so truncating franchises
// cascades into it and silently empties the routing table migration 023
// populated. Nothing in truncateOrder names it, which is exactly what makes
// this worth restoring explicitly: without it, every ticket created in a test
// comes out unrouted and the routing behaviour looks broken when only the
// harness is.
//
// sla_policies and category_priority_defaults have no such FK and survive
// truncation, so they need no restoration here.
func restoreSeededReferenceData(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO ticket_routing_rules (category, franchise_id, target_role, priority_order)
		VALUES ('connectivity', NULL, 'technician', 10),
		       ('billing',      NULL, 'csr',        10),
		       ('plan_change',  NULL, 'csr',        10),
		       ('other',        NULL, 'csr',        20)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		t.Fatalf("restore ticket_routing_rules: %v", err)
	}
}

// ── Seed helpers ────────────────────────────────────────────────────────────

type seedOpts struct {
	Username     string
	Status       string
	DunningState string
	Balance      string
	FranchiseID  *int
	PlanID       int
	RegisteredSt string
	PlanExpiry   *time.Time
	FUPActive    bool
	MobileNumber string
	DndOptOut    bool
}

// seedStaffUser creates a console operator. Needed by anything that sets
// tickets.assigned_to since migration 023 added the FK to staff_users that
// migration 009 promised and never delivered — before it, assigned_to
// accepted any integer, including ones matching no real account.
func seedStaffUser(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int, username, role string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO staff_users (id, username, password_hash, full_name, role, active)
		VALUES ($1, $2, '$2a$12$seedhash', $3, $4, TRUE)`,
		id, username, username, role)
	if err != nil {
		t.Fatalf("seed staff user %d: %v", id, err)
	}
}

func seedFranchise(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int, name, ratePct, status string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO franchises (id, name, owner_name, mobile_number, commission_rate_pct, status)
		VALUES ($1, $2, 'Owner', '+919000000000', $3::numeric, $4)`,
		id, name, ratePct, status)
	if err != nil {
		t.Fatalf("seed franchise %d: %v", id, err)
	}
}

// seedPlan inserts a plan. thresholdBytes of 0 means an unlimited plan.
func seedPlan(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int, name, rateLimit string, thresholdBytes int64, throttle, price string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO plans (id, name, rate_limit_string, volume_gb, fup_threshold_bytes,
		                   fup_throttle_string, price, validity_days)
		VALUES ($1, $2, $3, 3300, $4, NULLIF($5,''), $6::numeric, 30)`,
		id, name, rateLimit, thresholdBytes, throttle, price)
	if err != nil {
		t.Fatalf("seed plan %d: %v", id, err)
	}
}

func seedSubscriber(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int, o seedOpts) {
	t.Helper()
	if o.Status == "" {
		o.Status = "active"
	}
	if o.DunningState == "" {
		o.DunningState = "active"
	}
	if o.Balance == "" {
		o.Balance = "0.00"
	}
	if o.PlanID == 0 {
		o.PlanID = 1
	}
	if o.RegisteredSt == "" {
		o.RegisteredSt = "TN"
	}
	if o.MobileNumber == "" {
		o.MobileNumber = "+919876543210"
	}
	// Since migration 048 (chk_authorised_subscriber_is_billable) every status
	// that grants service needs a plan_expiry. This helper used to insert
	// those with NULL, which failed every test built on it — 118 of them —
	// from 048 until this default was added. Thirty days out is "paid and not
	// due", so it changes nothing for a test that does not care about expiry;
	// any test that does sets PlanExpiry itself.
	if o.PlanExpiry == nil && o.Status != "pending_payment" && o.Status != "hard_suspended" && o.Status != "terminated" {
		paidUntil := time.Now().Add(30 * 24 * time.Hour)
		o.PlanExpiry = &paidUntil
	}

	_, err := pool.Exec(ctx, `
		INSERT INTO subscribers (
			id, caf_number, username, password_hash, mobile_number, plan_id, franchise_id,
			status, dunning_state, wallet_balance, registered_state, plan_expiry,
			fup_active, dnd_opt_out
		) VALUES ($1, $2, $3, '$2a$12$seedhash', $4, $5, $6, $7, $8, $9::numeric, $10, $11, $12, $13)`,
		id, "CAF-"+o.Username, o.Username, o.MobileNumber, o.PlanID, o.FranchiseID,
		o.Status, o.DunningState, o.Balance, o.RegisteredSt, o.PlanExpiry,
		o.FUPActive, o.DndOptOut)
	if err != nil {
		t.Fatalf("seed subscriber %d: %v", id, err)
	}

	// A throttled subscriber also carries the rate that was applied to them
	// (FR-CAT-003, migration 067 backfills this for existing rows). Setting
	// only the boolean would describe a state the live system never
	// produces: the FUP scanner would see "flagged throttled, but no rate
	// recorded", decide the rate is wrong, and re-throttle them — so a test
	// seeding FUPActive would get an extra row it did not ask for.
	if o.FUPActive {
		if _, err := pool.Exec(ctx, `
			UPDATE subscribers s
			   SET fup_applied_rate = NULLIF(p.fup_throttle_string, '')
			  FROM plans p
			 WHERE p.id = s.plan_id AND s.id = $1`, id); err != nil {
			t.Fatalf("seed applied rate for subscriber %d: %v", id, err)
		}
	}
}

// seedSession opens a live session (stop_time NULL) with the given usage.
func seedSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool, subscriberID int, sessionID, nasIP string, in, out int64) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO subscriber_session_history (
			subscriber_id, session_id, nas_ip_address, assigned_ipv4,
			start_time, input_octets, output_octets
		) VALUES ($1, $2, $3::inet, '100.64.0.1'::inet, NOW() - INTERVAL '1 hour', $4, $5)`,
		subscriberID, sessionID, nasIP, in, out)
	if err != nil {
		t.Fatalf("seed session for subscriber %d: %v", subscriberID, err)
	}
}

func seedGstRate(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO gst_rates (id, cgst_rate, sgst_rate, igst_rate, effective_from)
		VALUES ($1, 9.00, 9.00, 18.00, NOW() - INTERVAL '1 day')`, id)
	if err != nil {
		t.Fatalf("seed gst rate: %v", err)
	}
}

func seedTemplate(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id, name, event string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO notification_templates (id, channel, template_name, event_trigger, active)
		VALUES ($1, 'whatsapp', $2, $3, TRUE)`, id, name, event)
	if err != nil {
		t.Fatalf("seed template %s: %v", id, err)
	}
}

func seedEncryptionKey(ctx context.Context, t *testing.T, pool *pgxpool.Pool, version string) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO encryption_keys (version_id, key_hash, status)
		VALUES ($1, 'testhash', 'active')
		ON CONFLICT (version_id) DO NOTHING`, version)
	if err != nil {
		t.Fatalf("seed encryption key %s: %v", version, err)
	}
}

// ── Assertions ──────────────────────────────────────────────────────────────

func mustDecimal(t *testing.T, s string) decimal.Decimal {
	t.Helper()
	d, err := decimal.NewFromString(s)
	if err != nil {
		t.Fatalf("parse decimal %q: %v", s, err)
	}
	return d
}

func assertDecimalEqual(t *testing.T, label string, got decimal.Decimal, want string) {
	t.Helper()
	expected := mustDecimal(t, want)
	if !got.Equal(expected) {
		t.Errorf("%s: want %s, got %s", label, expected, got)
	}
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func scanString(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, query, args...).Scan(&s); err != nil {
		t.Fatalf("scan query %q: %v", query, err)
	}
	return s
}

// seedWalletCredit writes a wallet credit with its payment detail, the pair
// a real payment produces (FR-BIL-013), so a test can exercise reads that
// join them — the collector takings in particular (FR-COL-001).
func seedWalletCredit(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	subscriberID int, amount, method, recordedBy string) int {
	t.Helper()
	return seedWalletEntry(ctx, t, pool, subscriberID, amount, method, recordedBy, "credit")
}

// seedWalletDebit writes the money-leaving half: a refund or a reversal.
//
// The amount is positive, like a credit: wallet_ledgers carries the
// direction in entry_type, not in the sign (see diagnostics.go and
// revenue.go). Seeding a negative amount here would be a fixture production
// never produces, and would let a reader that filtered on the sign pass.
func seedWalletDebit(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	subscriberID int, amount, method, recordedBy string) int {
	t.Helper()
	return seedWalletEntry(ctx, t, pool, subscriberID, amount, method, recordedBy, "debit")
}

func seedWalletEntry(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	subscriberID int, amount, method, recordedBy, entryType string) int {
	t.Helper()
	var id int
	err := pool.QueryRow(ctx, `
		INSERT INTO wallet_ledgers (subscriber_id, entry_type, amount, balance_after,
		                            description, account, created_at)
		VALUES ($1, $2, $3::numeric, 0, 'seeded', 'subscriber_wallet', NOW())
		RETURNING id`, subscriberID, entryType, amount).Scan(&id)
	if err != nil {
		t.Fatalf("seed wallet %s for subscriber %d: %v", entryType, subscriberID, err)
	}
	// A cheque has to carry its number, bank and date: chk_payment_cheque_fields
	// (migration 054) enforces that, so a seeded cheque with none is a row
	// production cannot produce and the insert fails. Supplied here rather
	// than at each call site, so a test asking for a cheque gets a valid one.
	var chequeNo, chequeBank *string
	var chequeDate *time.Time
	if method == "cheque" {
		no, bank := "SEED-"+strconv.Itoa(id), "Seed Bank"
		date := time.Now().AddDate(0, 0, -1)
		chequeNo, chequeBank, chequeDate = &no, &bank, &date
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_details (wallet_ledger_id, method, status, recorded_by,
		                             cheque_number, cheque_bank, cheque_date, created_at)
		VALUES ($1, $2, 'cleared', $3, $4, $5, $6, NOW())`,
		id, method, recordedBy, chequeNo, chequeBank, chequeDate); err != nil {
		t.Fatalf("seed payment detail for ledger %d: %v", id, err)
	}
	return id
}
