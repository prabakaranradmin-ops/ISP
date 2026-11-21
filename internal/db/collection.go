package db

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// Field payment collection — FR-COL-001 | migration 068.
// The domain rules live in internal/billing/collection.go.

// AssignCollector puts a subscriber on a collector's round, or takes them
// off it with a nil collectorID.
func (s *BillingStore) AssignCollector(ctx context.Context, subscriberID int, collectorID *int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE subscribers SET collector_id = $2 WHERE id = $1`, subscriberID, collectorID)
	if err != nil {
		return fmt.Errorf("db: assign collector for subscriber %d: %w", subscriberID, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("db: subscriber %d not found", subscriberID)
	}
	return nil
}

// ListCollectionRound returns the subscribers on one collector's round,
// soonest expiry first — FR-COL-001.
//
// Includes subscribers whose wallet already covers the plan rather than
// filtering them out: a row that silently vanishes between one refresh and
// the next looks like a bug to the collector working the round, and
// CollectionItem.AlreadyCovered marks them instead.
//
// Terminated subscribers are excluded. They have left, and sending somebody
// to their door is the one outcome a collection list must not produce.
//
// windowDays is how far ahead to look: 0 means only those already lapsed.
// Served by idx_subscribers_collector.
func (s *BillingStore) ListCollectionRound(ctx context.Context, collectorID, windowDays int) ([]billing.CollectionItem, error) {
	const q = `
		SELECT s.id, s.username, COALESCE(s.mobile_number, ''),
		       p.name, p.price, s.wallet_balance, s.plan_expiry,
		       s.status, s.dunning_state
		  FROM subscribers s
		  JOIN plans p ON p.id = s.plan_id
		 WHERE s.collector_id = $1
		   AND s.status <> 'terminated'
		   AND (s.plan_expiry IS NULL
		        OR s.plan_expiry <= NOW() + ($2 * INTERVAL '1 day'))
		 -- Soonest expiry first, so the off-service customers are visited
		 -- before the ones merely approaching renewal. Never-billed
		 -- subscribers (NULL expiry) sort LAST rather than first: they owe
		 -- their first payment and so belong on the round, but they are
		 -- often a signup from this morning, and putting every new customer
		 -- at the top of a collector's list would bury the people who are
		 -- actually cut off.
		 ORDER BY s.plan_expiry ASC NULLS LAST, s.username`

	rows, err := s.pool.Query(ctx, q, collectorID, windowDays)
	if err != nil {
		return nil, fmt.Errorf("db: list collection round for collector %d: %w", collectorID, err)
	}
	defer rows.Close()

	var out []billing.CollectionItem
	for rows.Next() {
		var c billing.CollectionItem
		if err := rows.Scan(&c.SubscriberID, &c.Username, &c.MobileNumber,
			&c.PlanName, &c.PlanPrice, &c.Balance, &c.PlanExpiry,
			&c.Status, &c.DunningState); err != nil {
			return nil, fmt.Errorf("db: scan collection item: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CollectorFor returns the staff id collecting from a subscriber, or nil
// when they are not on anybody's round.
//
// Read before a collector records a payment: the assignment is a permission,
// not a convenience (see billing.ErrNotYourCollection).
func (s *BillingStore) CollectorFor(ctx context.Context, subscriberID int) (*int, error) {
	var collectorID *int
	err := s.pool.QueryRow(ctx,
		`SELECT collector_id FROM subscribers WHERE id = $1`, subscriberID).Scan(&collectorID)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: subscriber %d not found: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: collector for subscriber %d: %w", subscriberID, err)
	}
	return collectorID, nil
}

// ListCollectors returns the staff who have subscribers assigned, with how
// many, for the owner's view and the assignment dropdown.
func (s *BillingStore) ListCollectors(ctx context.Context) ([]billing.CollectorLoad, error) {
	const q = `
		SELECT u.id, u.username, u.role, count(s.id)
		  FROM staff_users u
		  LEFT JOIN subscribers s
		         ON s.collector_id = u.id AND s.status <> 'terminated'
		 GROUP BY u.id, u.username, u.role
		 ORDER BY u.username`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list collectors: %w", err)
	}
	defer rows.Close()

	var out []billing.CollectorLoad
	for rows.Next() {
		var c billing.CollectorLoad
		if err := rows.Scan(&c.StaffID, &c.Username, &c.Role, &c.Assigned); err != nil {
			return nil, fmt.Errorf("db: scan collector: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CollectorTakings sums what each collector has recorded between from and
// to, split by payment method — FR-COL-001.
//
// Read from payment_details joined to the wallet ledger, which is the same
// pair of rows the subscriber's receipt came from. That matters: a separate
// collections table would be a second record of the same money, free to
// disagree with the first, and the figure a collector is asked to hand over
// has to be the one the customer was given a receipt for.
//
// Grouped by recorded_by, which is the username text rather than a staff id,
// so a collector's trail survives their account being deleted. The cost is
// that renaming a staff account splits their history; the alternative —
// deleting an account erasing a cash trail — is worse.
func (s *BillingStore) CollectorTakings(ctx context.Context, from, to time.Time) ([]billing.CollectorTakings, error) {
	const q = `
		SELECT pd.recorded_by, pd.method, SUM(w.amount), count(*)
		  FROM payment_details pd
		  JOIN wallet_ledgers w ON w.id = pd.wallet_ledger_id
		 WHERE pd.recorded_by IS NOT NULL AND btrim(pd.recorded_by) <> ''
		   AND w.created_at >= $1 AND w.created_at < $2
		   -- Credits only, keyed on entry_type and NOT on the sign of the
		   -- amount. wallet_ledgers stores amount positive for both
		   -- directions and carries the direction in entry_type — see
		   -- diagnostics.go and revenue.go, which both sum
		   -- CASE entry_type WHEN 'credit' THEN amount ELSE -amount END.
		   -- An "amount > 0" filter therefore excludes nothing, and a
		   -- refund or reversal would be counted as money taken, showing a
		   -- collector owing less cash at the counter than they hold.
		   AND w.entry_type = 'credit'
		   -- A bounced cheque is money that did not arrive. Its original
		   -- credit is still in the ledger — migration 054 bounces by
		   -- writing a reversal rather than flipping a status and leaving
		   -- the money in — so the credit-only filter above keeps it, and
		   -- the takings would overstate by the bounced amount. Excluded
		   -- here, which also means a collector is not asked to account
		   -- for a cheque the bank refused.
		   AND pd.status <> 'bounced'
		 GROUP BY pd.recorded_by, pd.method
		 ORDER BY pd.recorded_by, pd.method`

	rows, err := s.pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, fmt.Errorf("db: collector takings: %w", err)
	}
	defer rows.Close()

	byCollector := map[string]*billing.CollectorTakings{}
	var order []string
	for rows.Next() {
		var (
			who, method string
			amount      decimal.Decimal
			count       int
		)
		if err := rows.Scan(&who, &method, &amount, &count); err != nil {
			return nil, fmt.Errorf("db: scan takings: %w", err)
		}
		t, ok := byCollector[who]
		if !ok {
			t = &billing.CollectorTakings{
				Collector: who,
				ByMethod:  map[string]decimal.Decimal{},
			}
			byCollector[who] = t
			order = append(order, who)
		}
		t.ByMethod[method] = t.ByMethod[method].Add(amount)
		t.Total = t.Total.Add(amount)
		t.Count += count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]billing.CollectorTakings, 0, len(order))
	for _, who := range order {
		out = append(out, *byCollector[who])
	}
	return out, nil
}

// StaffIDForUsername resolves a signed-in staff username to its row id.
//
// Needed because a session carries the username while the assignment is a
// foreign key to staff_users. Deliberately here rather than reusing
// StaffStore.GetStaffByUsername: that returns a staffui type, and internal/api
// has no business importing the console's view models to answer "which staff
// row is this".
func (s *BillingStore) StaffIDForUsername(ctx context.Context, username string) (int, error) {
	var id int
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM staff_users WHERE username = $1`, username).Scan(&id)
	if isNoRows(err) {
		return 0, fmt.Errorf("db: no staff account %q: %w", username, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("db: staff id for %q: %w", username, err)
	}
	return id, nil
}
