//go:build integration

// The report catalogue's nine new reports — FR-RPT-004.
//
// Nine queries against the operational tables, none of which needed a
// migration. What only a real database can show: that the SQL parses at all
// (every one of these uses window functions, FILTER clauses or percentiles
// that no fake exercises), that the money reports agree with FR-COL-001's
// reconciliation about what counts as money taken, and that the fair-use
// report counts usage the same way the scanner does — because a report that
// disagrees with the enforcement makes the operator distrust the one that is
// right.
package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/reporting"
	"github.com/shopspring/decimal"
)

// ── Expiring subscribers ────────────────────────────────────────────────────

func TestFR_RPT_004_ExpiringListsTheRenewalCallListSoonestFirst(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")

	soon := time.Now().Add(72 * time.Hour)
	sooner := time.Now().Add(24 * time.Hour)
	lapsed := time.Now().Add(-48 * time.Hour)
	far := time.Now().Add(60 * 24 * time.Hour)

	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "soon@isp", PlanExpiry: &soon})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "sooner@isp", PlanExpiry: &sooner})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "lapsed@isp", PlanExpiry: &lapsed})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "far@isp", PlanExpiry: &far})
	seedSubscriber(ctx, t, pool, 5, seedOpts{Username: "gone@isp", Status: "terminated", PlanExpiry: &sooner})

	// A wallet that already covers the plan, so the amount due is zero rather
	// than negative.
	if _, err := pool.Exec(ctx,
		`UPDATE subscribers SET wallet_balance = 1000.00 WHERE id = 1`); err != nil {
		t.Fatalf("fund wallet: %v", err)
	}

	rows, err := store.ExpiringSubscribers(ctx, 7, nil)
	if err != nil {
		t.Fatalf("expiring: %v", err)
	}

	var got []string
	for _, r := range rows {
		got = append(got, r.Username)
	}
	// Lapsed first (most negative days_left), then sooner, then soon. The one
	// 60 days out and the terminated one are absent.
	want := []string{"lapsed@isp", "sooner@isp", "soon@isp"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v — soonest (and already lapsed) first", got, want)
		}
	}

	byName := map[string]reporting.ExpiringRow{}
	for _, r := range rows {
		byName[r.Username] = r
	}

	// An expiry already past is a negative day count, not a zero or an
	// absence.
	if d := byName["lapsed@isp"].DaysLeft; d >= 0 {
		t.Errorf("a lapsed subscriber has days_left = %d; it should be negative", d)
	}
	// The amount to ask for, floored at zero.
	if due := byName["soon@isp"].AmountDue(); !due.IsZero() {
		t.Errorf("a subscriber whose wallet covers the plan owes %s, want 0", due)
	}
	if due := byName["sooner@isp"].AmountDue(); !due.Equal(decimal.RequireFromString("799.00")) {
		t.Errorf("amount due = %s, want 799.00", due)
	}
	if byName["sooner@isp"].MobileNumber == "" {
		t.Error("the call list carries no phone number, which is what it is for")
	}
}

// ── Plan change history ─────────────────────────────────────────────────────

func TestFR_RPT_004_PlanChangeHistoryShowsAllThreeOutcomes(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedPlan(ctx, t, pool, 2, "Fibre 200", "200M/200M", 0, "", "999.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})

	if _, err := pool.Exec(ctx, `
		INSERT INTO scheduled_plan_changes
		    (subscriber_id, new_plan_id, from_plan_id, effective_on, status, reason,
		     scheduled_by, scheduled_at, applied_at, cancelled_at, cancelled_by, cancel_reason)
		VALUES
		    (1, 2, 1, CURRENT_DATE + 10, 'scheduled', 'upgrade', 'csr', NOW(), NULL, NULL, NULL, NULL),
		    (1, 2, 1, CURRENT_DATE - 10, 'applied',   'upgrade', 'csr', NOW() - interval '20 days',
		     NOW() - interval '10 days', NULL, NULL, NULL),
		    -- chk_spc_status requires cancel_reason on a cancelled row: the
		    -- reason column says why the change was wanted, cancel_reason why
		    -- it was withdrawn, and the constraint will not accept one for
		    -- the other.
		    (1, 1, 2, CURRENT_DATE - 5,  'cancelled', 'changed mind', 'csr',
		     NOW() - interval '15 days', NULL, NOW() - interval '14 days', 'owner',
		     'customer withdrew the request')`); err != nil {
		t.Fatalf("seed changes: %v", err)
	}

	rows, err := store.PlanChangeHistory(ctx, 12, nil)
	if err != nil {
		t.Fatalf("plan change history: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows; all three outcomes should appear — what was withdrawn before it "+
			"fired is part of the record", len(rows))
	}

	seen := map[string]reporting.PlanChangeRow{}
	for _, r := range rows {
		seen[r.Status] = r
	}
	for _, status := range []string{"scheduled", "applied", "cancelled"} {
		if _, ok := seen[status]; !ok {
			t.Errorf("no %q row", status)
		}
	}
	// Plan names, not ids: "Fibre 100 → Fibre 200" is the point of keeping
	// from_plan_id.
	if r := seen["applied"]; r.FromPlan != "Fibre 100" || r.ToPlan != "Fibre 200" {
		t.Errorf("applied row reads %q → %q", r.FromPlan, r.ToPlan)
	}
	if seen["applied"].AppliedAt == nil {
		t.Error("the applied row has no applied_at")
	}
	if seen["cancelled"].CancelledBy != "owner" {
		t.Errorf("the cancelled row does not say who withdrew it: %q", seen["cancelled"].CancelledBy)
	}
	// Newest decision first.
	if rows[0].ScheduledAt.Before(rows[len(rows)-1].ScheduledAt) {
		t.Error("the history is oldest-first; the most recent decision should lead")
	}
}

// ── The money reports ───────────────────────────────────────────────────────

// seedCollectedPayment records a wallet credit with its payment detail, the
// way FR-BIL-013 does.
func seedCollectedPayment(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	subscriberID int, amount, method, by, status string, at time.Time) {
	t.Helper()
	var ledgerID int
	err := pool.QueryRow(ctx, `
		INSERT INTO wallet_ledgers (subscriber_id, entry_type, amount, balance_after,
		                            description, created_at, account)
		VALUES ($1, 'credit', $2::numeric, $2::numeric, 'payment', $3, 'subscriber_wallet')
		RETURNING id`, subscriberID, amount, at).Scan(&ledgerID)
	if err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	// A bounce is not a status flag. chk_payment_bounce_has_reversal requires
	// a bounced row to carry both its reversing ledger entry and the time it
	// bounced, which is migration 054's whole point: the original credit
	// stays and a debit undoes it. Seeding a bare status = 'bounced' would be
	// a fixture production never produces — and it is precisely the shape
	// that made an earlier takings query look correct while counting bounced
	// cheques as money taken.
	var reversalID *int
	var bouncedAt *time.Time
	if status == "bounced" {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO wallet_ledgers (subscriber_id, entry_type, amount, balance_after,
			                            description, created_at, account)
			VALUES ($1, 'debit', $2::numeric, 0.00, 'cheque bounced', $3, 'subscriber_wallet')
			RETURNING id`, subscriberID, amount, at).Scan(&id); err != nil {
			t.Fatalf("seed bounce reversal: %v", err)
		}
		reversalID, bouncedAt = &id, &at
	}

	// chk_payment_cheque_fields: a cheque carries its number, bank and date
	// or it is not a cheque. Left NULL for every other method, because the
	// same constraint would then be describing a payment that has none.
	var chequeNo, chequeBank *string
	var chequeDate *time.Time
	if method == "cheque" {
		no, bank := "100123", "State Bank"
		chequeNo, chequeBank, chequeDate = &no, &bank, &at
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO payment_details (wallet_ledger_id, method, status, recorded_by, created_at,
		                             cheque_number, cheque_bank, cheque_date,
		                             bounce_ledger_id, bounced_at, bounce_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''))`,
		ledgerID, method, status, by, at, chequeNo, chequeBank, chequeDate,
		reversalID, bouncedAt,
		map[bool]string{true: "insufficient funds", false: ""}[status == "bounced"]); err != nil {
		t.Fatalf("seed payment details: %v", err)
	}
}

func TestFR_RPT_004_PaymentsByMethodExcludesWhatNeverArrived(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})

	now := time.Now()
	seedCollectedPayment(ctx, t, pool, 1, "799.00", "cash", "tech", "cleared", now)
	seedCollectedPayment(ctx, t, pool, 1, "500.00", "cash", "tech", "cleared", now)
	seedCollectedPayment(ctx, t, pool, 1, "999.00", "upi", "csr", "cleared", now)
	// A bounced cheque, seeded the way the bounce path actually writes it:
	// the original credit stays and a reversing debit undoes it. So the
	// report has two chances to get this wrong — counting the credit (money
	// that came back) or counting the debit (money leaving) — and must do
	// neither.
	seedCollectedPayment(ctx, t, pool, 1, "5000.00", "cheque", "csr", "bounced", now)
	// A debit, which is money leaving.
	if _, err := pool.Exec(ctx, `
		INSERT INTO wallet_ledgers (subscriber_id, entry_type, amount, balance_after, description, account)
		VALUES (1, 'debit', 300.00, 0.00, 'renewal', 'subscriber_wallet')`); err != nil {
		t.Fatalf("seed debit: %v", err)
	}

	rows, err := store.PaymentsByMethod(ctx, 12, nil)
	if err != nil {
		t.Fatalf("payments by method: %v", err)
	}

	byMethod := map[string]reporting.PaymentMethodRow{}
	for _, r := range rows {
		byMethod[r.Method] = r
	}
	if r := byMethod["cash"]; r.Payments != 2 ||
		!r.Total.Equal(decimal.RequireFromString("1299.00")) {
		t.Errorf("cash = %d payments / %s, want 2 / 1299.00", r.Payments, r.Total)
	}
	if r := byMethod["upi"]; !r.Total.Equal(decimal.RequireFromString("999.00")) {
		t.Errorf("upi total = %s, want 999.00", r.Total)
	}
	if r, ok := byMethod["cheque"]; ok {
		t.Errorf("a bounced cheque is reported as money taken: %s", r.Total)
	}
	// The debit must not appear as a payment under any method.
	for _, r := range rows {
		if r.Total.Equal(decimal.RequireFromString("300.00")) {
			t.Errorf("a debit was counted as a payment: %+v", r)
		}
	}
}

func TestFR_RPT_004_CollectionByStaffSeparatesCashFromDigital(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})

	now := time.Now()
	seedCollectedPayment(ctx, t, pool, 1, "500.00", "cash", "tech_user", "cleared", now)
	seedCollectedPayment(ctx, t, pool, 1, "300.00", "cash", "tech_user", "cleared", now)
	seedCollectedPayment(ctx, t, pool, 1, "999.00", "upi", "tech_user", "cleared", now)
	seedCollectedPayment(ctx, t, pool, 1, "100.00", "cash", "csr_user", "cleared", now)
	// A wallet adjustment — a credit with no payment record. Money moved by
	// an operator, not money handed over, so it is not a collection.
	if _, err := pool.Exec(ctx, `
		INSERT INTO wallet_ledgers (subscriber_id, entry_type, amount, balance_after,
		                            description, account, adjusted_by_username)
		VALUES (1, 'credit', 777.00, 0.00, 'goodwill', 'adjustment_clearing', 'owner')`); err != nil {
		t.Fatalf("seed adjustment: %v", err)
	}

	rows, err := store.CollectionByStaff(ctx, 12, nil)
	if err != nil {
		t.Fatalf("collection by staff: %v", err)
	}

	byWho := map[string]reporting.StaffCollectionRow{}
	for _, r := range rows {
		byWho[r.RecordedBy] = r
	}
	tech := byWho["tech_user"]
	if !tech.CashTotal.Equal(decimal.RequireFromString("800.00")) {
		t.Errorf("tech cash = %s, want 800.00 — this is the figure that has to be handed in",
			tech.CashTotal)
	}
	if !tech.DigitalTotal.Equal(decimal.RequireFromString("999.00")) {
		t.Errorf("tech digital = %s, want 999.00", tech.DigitalTotal)
	}
	if !tech.Total.Equal(decimal.RequireFromString("1799.00")) {
		t.Errorf("tech total = %s, want 1799.00", tech.Total)
	}
	if tech.Payments != 3 {
		t.Errorf("tech payments = %d, want 3", tech.Payments)
	}
	if !byWho["csr_user"].CashTotal.Equal(decimal.RequireFromString("100.00")) {
		t.Errorf("csr cash = %s, want 100.00", byWho["csr_user"].CashTotal)
	}
	// The adjustment is attributed to nobody here, because it is not a
	// collection at all.
	for _, r := range rows {
		if r.Total.Equal(decimal.RequireFromString("777.00")) {
			t.Errorf("a wallet adjustment is reported as a collection: %+v", r)
		}
	}
}

func TestFR_RPT_004_TheInvoiceRegisterAndTaxReportAgreeWithEachOther(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedGstRate(ctx, t, pool, 1)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})

	for i := 0; i < 2; i++ {
		if _, err := pool.Exec(ctx, `
			INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount,
			                      igst_amount, total_amount, gst_rate_id, gb_included,
			                      gb_used, description)
			VALUES (1, 799.00, 71.91, 71.91, 0.00, 942.82, 1, 3300, 100, 'Monthly rental')`); err != nil {
			t.Fatalf("seed invoice: %v", err)
		}
	}

	register, err := store.InvoiceRegister(ctx, 12, nil)
	if err != nil {
		t.Fatalf("invoice register: %v", err)
	}
	if len(register) != 2 {
		t.Fatalf("the register holds %d invoices, want 2", len(register))
	}
	if register[0].Username != "a@isp" || register[0].PlanName != "Fibre 100" {
		t.Errorf("row = %+v", register[0])
	}
	if register[0].Description != "Monthly rental" {
		t.Errorf("description = %q", register[0].Description)
	}

	tax, err := store.TaxCharged(ctx, 12, nil)
	if err != nil {
		t.Fatalf("tax charged: %v", err)
	}
	if len(tax) != 1 {
		t.Fatalf("tax is grouped into %d rows, want 1 (one month at one rate)", len(tax))
	}

	// The two reports describe the same invoices, so their totals must match.
	// A register that disagrees with the tax report is the kind of thing an
	// accountant finds and nobody can explain.
	var regBase, regTax decimal.Decimal
	for _, r := range register {
		regBase = regBase.Add(r.BaseAmount)
		regTax = regTax.Add(r.CGST).Add(r.SGST).Add(r.IGST)
	}
	if !tax[0].TaxableValue.Equal(regBase) {
		t.Errorf("tax report taxable %s vs register %s", tax[0].TaxableValue, regBase)
	}
	if !tax[0].TaxTotal.Equal(regTax) {
		t.Errorf("tax report tax %s vs register %s", tax[0].TaxTotal, regTax)
	}
	if tax[0].Invoices != len(register) {
		t.Errorf("tax report counts %d invoices, register holds %d", tax[0].Invoices, len(register))
	}
	// CGST and SGST stay in their own columns; summing them would reconcile
	// against neither.
	if !tax[0].CGST.Equal(tax[0].SGST) || tax[0].CGST.IsZero() {
		t.Errorf("CGST %s / SGST %s", tax[0].CGST, tax[0].SGST)
	}
	if !tax[0].IGST.IsZero() {
		t.Errorf("IGST = %s for an intra-state supply", tax[0].IGST)
	}
}

// ── Support ─────────────────────────────────────────────────────────────────

func TestFR_RPT_004_TurnaroundMeasuresFirstResolutionAndCountsWhatIsStillOpen(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a@isp"})
	seedStaffUser(ctx, t, pool, 1, "csr_user", "csr")

	// Two tickets resolved, one never resolved.
	var ids []int
	for i := 0; i < 3; i++ {
		var id int
		if err := pool.QueryRow(ctx, `
			INSERT INTO tickets (subscriber_id, category, description, status, priority, created_at)
			VALUES (1, 'billing', 'help', 'open', 'medium', NOW() - interval '5 days')
			RETURNING id`).Scan(&id); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
		ids = append(ids, id)
	}

	// The first resolves after 2 hours; the second after 10, but is reopened
	// and resolved again after 100 — the first arrival is what counts, or a
	// reopen would be reported as one slow success and hidden.
	if _, err := pool.Exec(ctx, `
		INSERT INTO ticket_status_history (ticket_id, old_status, new_status, changed_by, occurred_at)
		VALUES ($1, 'open', 'resolved', 'csr_user', NOW() - interval '5 days' + interval '2 hours'),
		       ($2, 'open', 'resolved', 'csr_user', NOW() - interval '5 days' + interval '10 hours'),
		       ($2, 'resolved', 'open', 'csr_user', NOW() - interval '5 days' + interval '11 hours'),
		       ($2, 'open', 'resolved', 'csr_user', NOW() - interval '5 days' + interval '100 hours')`,
		ids[0], ids[1]); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	rows, err := store.TicketTurnaround(ctx, 12, nil)
	if err != nil {
		t.Fatalf("turnaround: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (one month, one category, one priority)", len(rows))
	}
	r := rows[0]

	if r.Raised != 3 {
		t.Errorf("raised = %d, want 3", r.Raised)
	}
	if r.Resolved != 2 {
		t.Errorf("resolved = %d, want 2", r.Resolved)
	}
	if r.StillOpen != 1 {
		t.Errorf("still open = %d, want 1 — a fast median over a month that left tickets "+
			"open is not a fast month", r.StillOpen)
	}
	if r.MedianHours == nil || r.MaxHours == nil {
		t.Fatalf("median or max missing: %+v", r)
	}
	// Median of 2 and 10 hours is 6. If the reopened ticket's *last*
	// resolution were used it would be 51.
	if *r.MedianHours < 5.9 || *r.MedianHours > 6.1 {
		t.Errorf("median = %.2f hours, want ~6 — the first arrival at resolved, not the last",
			*r.MedianHours)
	}
	if *r.MaxHours > 10.1 {
		t.Errorf("worst = %.2f hours, want ~10; a reopen is being counted as one slow success",
			*r.MaxHours)
	}
}

// ── Network ─────────────────────────────────────────────────────────────────

func TestFR_RPT_004_TopDataUsersRanksByVolumeAndRespectsTheLimit(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 1<<40, "10M/10M", "799.00")
	for i := 1; i <= 3; i++ {
		seedSubscriber(ctx, t, pool, i, seedOpts{Username: fmt.Sprintf("s%d@isp", i)})
	}

	// Subscriber 3 heaviest, 1 lightest; two sessions for 3 so the sum
	// across sessions is what ranks, not a single row.
	seedClosedSession(ctx, t, pool, 1, 1<<28, 1<<28)
	seedClosedSession(ctx, t, pool, 2, 1<<30, 1<<30)
	seedClosedSession(ctx, t, pool, 3, 1<<31, 1<<30)
	seedClosedSession(ctx, t, pool, 3, 1<<31, 1<<30)

	rows, err := store.TopDataUsers(ctx, 12, 50, nil)
	if err != nil {
		t.Fatalf("top data users: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if rows[0].SubscriberID != 3 {
		t.Errorf("heaviest is subscriber %d, want 3", rows[0].SubscriberID)
	}
	if rows[0].Sessions != 2 {
		t.Errorf("the heaviest shows %d sessions, want 2 — usage is summed across them",
			rows[0].Sessions)
	}
	if rows[0].TotalBytes != rows[0].InputOctets+rows[0].OutputOctets {
		t.Errorf("total %d is not input %d + output %d",
			rows[0].TotalBytes, rows[0].InputOctets, rows[0].OutputOctets)
	}
	// The plan's allowance travels with the row so a big number can be read
	// against what they bought.
	if rows[0].AllowanceBytes != 1<<40 {
		t.Errorf("allowance = %d, want the plan's threshold", rows[0].AllowanceBytes)
	}
	if rows[len(rows)-1].SubscriberID != 1 {
		t.Errorf("lightest is subscriber %d, want 1", rows[len(rows)-1].SubscriberID)
	}

	// A ranking needs a cut-off, or it is just the base sorted.
	capped, err := store.TopDataUsers(ctx, 12, 2, nil)
	if err != nil {
		t.Fatalf("top data users capped: %v", err)
	}
	if len(capped) != 2 {
		t.Errorf("a limit of 2 returned %d rows", len(capped))
	}
}

// The report and the fair-use scanner must agree about who is over, or an
// operator comparing the two concludes the scanner is broken.
func TestFR_RPT_004_FUPExceededCountsUsageTheWayTheScannerDoes(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	const allowance = int64(1) << 30 // 1 GB
	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", allowance, "10M/10M", "799.00")
	seedPlan(ctx, t, pool, 2, "Unlimited", "100M/100M", 0, "", "1299.00")

	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "over@isp"})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "under@isp"})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "toppedup@isp"})
	seedSubscriber(ctx, t, pool, 4, seedOpts{Username: "closed@isp"})
	seedSubscriber(ctx, t, pool, 5, seedOpts{Username: "unlimited@isp", PlanID: 2})

	// Open sessions: 1 is over, 2 is under, 3 is over the plan but under it
	// once their top-up is counted, 5 is on an unlimited plan.
	seedOpenSession(ctx, t, pool, 1, allowance, allowance)
	seedOpenSession(ctx, t, pool, 2, 1<<20, 1<<20)
	seedOpenSession(ctx, t, pool, 3, allowance, 1<<20)
	seedOpenSession(ctx, t, pool, 5, allowance*8, allowance*8)

	// Subscriber 4's usage is in a *closed* session. The scanner does not
	// count it, so neither may this report — listing them would name somebody
	// whose throttle has already been lifted.
	seedClosedSession(ctx, t, pool, 4, allowance*4, allowance*4)

	if _, err := pool.Exec(ctx, `
		INSERT INTO data_topups (subscriber_id, bytes, expires_at, purchased_by)
		VALUES (3, $1, NOW() + interval '20 days', 'csr')`, allowance*4); err != nil {
		t.Fatalf("seed topup: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE subscribers SET fup_active = TRUE, fup_applied_rate = '10M/10M' WHERE id = 1`); err != nil {
		t.Fatalf("set applied rate: %v", err)
	}

	rows, err := store.FUPExceeded(ctx, nil)
	if err != nil {
		t.Fatalf("fup exceeded: %v", err)
	}

	byName := map[string]reporting.FUPExceededRow{}
	for _, r := range rows {
		byName[r.Username] = r
	}
	if _, ok := byName["over@isp"]; !ok {
		t.Error("a subscriber over their allowance is not listed")
	}
	if _, ok := byName["under@isp"]; ok {
		t.Error("a subscriber under their allowance is listed")
	}
	if _, ok := byName["toppedup@isp"]; ok {
		t.Error("a subscriber brought back under by a top-up is still listed — a top-up raises " +
			"the allowance rather than clearing a flag")
	}
	if _, ok := byName["closed@isp"]; ok {
		t.Error("usage from a closed session was counted, so this report disagrees with the " +
			"scanner about who is over")
	}
	if _, ok := byName["unlimited@isp"]; ok {
		t.Error("a subscriber on an unlimited plan is listed as over an allowance they do not have")
	}

	over := byName["over@isp"]
	if over.UsedBytes != allowance*2 {
		t.Errorf("used = %d, want %d (summed across the open session)", over.UsedBytes, allowance*2)
	}
	if over.AllowanceBytes != allowance {
		t.Errorf("allowance = %d, want %d", over.AllowanceBytes, allowance)
	}
	if over.AppliedRate != "10M/10M" || !over.FUPActive {
		t.Errorf("the throttle actually applied is not reported: %q / %v",
			over.AppliedRate, over.FUPActive)
	}
	if pct := over.OveragePct(); pct == nil || *pct < 199 || *pct > 201 {
		t.Errorf("overage = %v, want ~200%%", pct)
	}
}

// ── The franchise predicate ─────────────────────────────────────────────────

// Every report carries the same franchise predicate as the original four, so
// a franchise-scoped caller sees their own rows and nobody else's. Checked
// together because a report that forgot it would be a cross-partner leak.
func TestFR_RPT_004_EveryReportHonoursTheFranchiseScope(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Reporting()

	seedFranchise(ctx, t, pool, 1, "North", "10.00", "active")
	seedFranchise(ctx, t, pool, 2, "South", "10.00", "active")
	seedPlan(ctx, t, pool, 1, "Fibre 100", "100M/100M", 1<<30, "10M/10M", "799.00")
	seedPlan(ctx, t, pool, 2, "Fibre 200", "200M/200M", 1<<30, "10M/10M", "999.00")
	seedGstRate(ctx, t, pool, 1)
	seedStaffUser(ctx, t, pool, 1, "csr_user", "csr")

	north, south := 1, 2
	expiry := time.Now().Add(24 * time.Hour)
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "north@isp", FranchiseID: &north, PlanExpiry: &expiry})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "south@isp", FranchiseID: &south, PlanExpiry: &expiry})

	now := time.Now()
	for _, id := range []int{1, 2} {
		seedCollectedPayment(ctx, t, pool, id, "799.00", "cash", "tech", "cleared", now)
		if _, err := pool.Exec(ctx, `
			UPDATE wallet_ledgers SET franchise_id = $1 WHERE subscriber_id = $2`,
			id, id); err != nil {
			t.Fatalf("scope ledger: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount,
			                      igst_amount, total_amount, gst_rate_id, gb_included, gb_used)
			VALUES ($1, 799.00, 71.91, 71.91, 0.00, 942.82, 1, 3300, 10)`, id); err != nil {
			t.Fatalf("seed invoice: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO tickets (subscriber_id, category, description, status, priority, franchise_id)
			VALUES ($1, 'billing', 'help', 'open', 'medium', $2)`, id, id); err != nil {
			t.Fatalf("seed ticket: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO scheduled_plan_changes (subscriber_id, new_plan_id, from_plan_id,
			                                    effective_on, status, scheduled_by)
			VALUES ($1, 2, 1, CURRENT_DATE + 5, 'scheduled', 'csr')`, id); err != nil {
			t.Fatalf("seed change: %v", err)
		}
		seedOpenSession(ctx, t, pool, id, 1<<30, 1<<30)
	}

	// Each report, scoped to North, must return exactly one row — North's.
	checks := []struct {
		name string
		run  func() (int, error)
	}{
		{"expiring", func() (int, error) {
			r, err := store.ExpiringSubscribers(ctx, 7, &north)
			return len(r), err
		}},
		{"plan changes", func() (int, error) {
			r, err := store.PlanChangeHistory(ctx, 12, &north)
			return len(r), err
		}},
		{"payments by method", func() (int, error) {
			r, err := store.PaymentsByMethod(ctx, 12, &north)
			return len(r), err
		}},
		{"collection by staff", func() (int, error) {
			r, err := store.CollectionByStaff(ctx, 12, &north)
			return len(r), err
		}},
		{"invoice register", func() (int, error) {
			r, err := store.InvoiceRegister(ctx, 12, &north)
			return len(r), err
		}},
		{"tax charged", func() (int, error) {
			r, err := store.TaxCharged(ctx, 12, &north)
			return len(r), err
		}},
		{"ticket turnaround", func() (int, error) {
			r, err := store.TicketTurnaround(ctx, 12, &north)
			return len(r), err
		}},
		{"top data users", func() (int, error) {
			r, err := store.TopDataUsers(ctx, 12, 50, &north)
			return len(r), err
		}},
		{"fup exceeded", func() (int, error) {
			r, err := store.FUPExceeded(ctx, &north)
			return len(r), err
		}},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			n, err := c.run()
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if n != 1 {
				t.Errorf("%s returned %d rows for one franchise; a report that ignores the "+
					"scope shows one partner another's figures", c.name, n)
			}
		})
	}

	// And unscoped sees both, so the predicate is a filter rather than a
	// blanket restriction.
	if r, err := store.ExpiringSubscribers(ctx, 7, nil); err != nil || len(r) != 2 {
		t.Errorf("unscoped expiring returned %d rows (%v), want 2", len(r), err)
	}
}

// seedOpenSession and seedClosedSession write accounting rows with octets.
//
// Open means stop_time IS NULL, which is what the fair-use scanner — and so
// the over-fair-use report — counts. Closed sessions still count towards the
// top-users report, which asks a different question.
func seedOpenSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	subscriberID int, in, out int64) {
	t.Helper()
	// The session id is built here rather than with || in SQL: reusing $1 as
	// both an integer subscriber id and an operand of || makes Postgres
	// deduce text for it and refuse the whole statement with 42P08.
	sessionID := fmt.Sprintf("open-%d-%d-%d", subscriberID, in, out)
	_, err := pool.Exec(ctx, `
		INSERT INTO subscriber_session_history
		    (subscriber_id, session_id, nas_ip_address, start_time, input_octets, output_octets)
		VALUES ($1, $2, '10.20.0.5'::inet, NOW() - INTERVAL '1 hour', $3, $4)`,
		subscriberID, sessionID, in, out)
	if err != nil {
		t.Fatalf("seed open session for %d: %v", subscriberID, err)
	}
}

func seedClosedSession(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	subscriberID int, in, out int64) {
	t.Helper()
	sessionID := fmt.Sprintf("closed-%d-%d-%d", subscriberID, in, out)
	_, err := pool.Exec(ctx, `
		INSERT INTO subscriber_session_history
		    (subscriber_id, session_id, nas_ip_address, start_time, stop_time,
		     input_octets, output_octets)
		VALUES ($1, $2, '10.20.0.5'::inet,
		        NOW() - INTERVAL '3 hours', NOW() - INTERVAL '2 hours', $3, $4)`,
		subscriberID, sessionID, in, out)
	if err != nil {
		t.Fatalf("seed closed session for %d: %v", subscriberID, err)
	}
}
