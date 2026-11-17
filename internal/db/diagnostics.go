package db

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Billing diagnostics — FR-BIL-019. See billing/diagnostics.go for what
// each check means. Each query returns its rows with a window count of the
// whole match, so the screen can say "showing 200 of 1,240".

// uninvoicedQuery: served now (a status RADIUS authorises, inside a paid
// cycle) on a plan that costs something, and no invoice covering the
// cycle. Grace, soft and hard suspension are past expiry and so outside
// any cycle; terminated and pending_payment are not served.
//
// Only plan invoices count (description IS NULL): a one-off charge such as
// an installation, invoiced from a proforma, does not pay for a cycle.
const uninvoicedQuery = `
	SELECT s.id, s.username, s.status, p.name, p.price::text,
	       s.plan_expiry - make_interval(days => p.validity_days), s.plan_expiry,
	       (SELECT max(i.created_at) FROM invoices i WHERE i.subscriber_id = s.id AND i.description IS NULL),
	       count(*) OVER ()
	  FROM subscribers s
	  JOIN plans p ON p.id = s.plan_id
	 WHERE s.status = 'active'
	   AND s.plan_expiry > now()
	   AND p.price > 0
	   AND NOT EXISTS (
	         SELECT 1 FROM invoices i
	          WHERE i.subscriber_id = s.id AND i.description IS NULL
	            AND i.created_at >= s.plan_expiry - make_interval(days => p.validity_days) - interval '1 day')
	   AND (SELECT count(*) FROM invoices i
	         WHERE i.subscriber_id = s.id AND i.description IS NULL
	           AND i.created_at >= s.plan_expiry - make_interval(days => 2 * p.validity_days) - interval '1 day') < 2
	 ORDER BY s.plan_expiry - make_interval(days => p.validity_days), s.id
	 LIMIT $1`

// mismatchQuery: stored balance against the wallet legs. Only legs on the
// subscriber_wallet account count — counter-legs carry the subscriber's id
// too, and adding them in would double every posting.
const mismatchQuery = `
	WITH legs AS (
		SELECT subscriber_id,
		       sum(CASE entry_type WHEN 'credit' THEN amount ELSE -amount END) AS ledger_sum,
		       count(*) AS n
		  FROM wallet_ledgers
		 WHERE account = 'subscriber_wallet'
		 GROUP BY subscriber_id
	), latest AS (
		SELECT DISTINCT ON (subscriber_id) subscriber_id, balance_after
		  FROM wallet_ledgers
		 WHERE account = 'subscriber_wallet'
		 ORDER BY subscriber_id, id DESC
	)
	SELECT s.id, s.username, s.wallet_balance::text, COALESCE(l.ledger_sum, 0)::text,
	       t.balance_after::text, COALESCE(l.n, 0), count(*) OVER ()
	  FROM subscribers s
	  LEFT JOIN legs l   ON l.subscriber_id = s.id
	  LEFT JOIN latest t ON t.subscriber_id = s.id
	 WHERE s.wallet_balance <> COALESCE(l.ledger_sum, 0)
	    OR (t.balance_after IS NOT NULL AND s.wallet_balance <> t.balance_after)
	 ORDER BY abs(s.wallet_balance - COALESCE(l.ledger_sum, 0)) DESC, s.id
	 LIMIT $1`

// driftQuery: the newest plan invoice (description IS NULL — a one-off
// charge carries its own description) against today's catalogue price.
const driftQuery = `
	SELECT s.id, s.username, p.name, p.price::text, li.base_amount::text, li.id, li.created_at,
	       count(*) OVER ()
	  FROM subscribers s
	  JOIN plans p ON p.id = s.plan_id
	  JOIN LATERAL (
	        SELECT i.id, i.base_amount, i.created_at FROM invoices i
	         WHERE i.subscriber_id = s.id AND i.description IS NULL
	         ORDER BY i.created_at DESC, i.id DESC
	         LIMIT 1) li ON true
	 WHERE s.status <> 'terminated'
	   AND li.base_amount <> p.price
	 ORDER BY abs(li.base_amount - p.price) DESC, s.id
	 LIMIT $1`

// BillingDiagnostics runs all three checks, each list capped at limit.
func (s *BillingStore) BillingDiagnostics(ctx context.Context, limit int) (*billing.Diagnostics, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	out := &billing.Diagnostics{RanAt: time.Now()}

	rows, err := s.pool.Query(ctx, uninvoicedQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("db: diagnostics: uninvoiced: %w", err)
	}
	for rows.Next() {
		var (
			u     billing.UninvoicedCycle
			price string
		)
		if err := rows.Scan(&u.SubscriberID, &u.Username, &u.Status, &u.PlanName, &price,
			&u.CycleStart, &u.PlanExpiry, &u.LastInvoice, &out.UninvoicedCount); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: diagnostics: scan uninvoiced: %w", err)
		}
		if u.PlanPrice, err = parseDecimal(price); err != nil {
			rows.Close()
			return nil, err
		}
		out.Uninvoiced = append(out.Uninvoiced, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: diagnostics: uninvoiced: %w", err)
	}

	rows, err = s.pool.Query(ctx, mismatchQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("db: diagnostics: balances: %w", err)
	}
	for rows.Next() {
		var (
			m           billing.BalanceMismatch
			stored, sum string
			after       *string
		)
		if err := rows.Scan(&m.SubscriberID, &m.Username, &stored, &sum, &after, &m.Entries, &out.MismatchCount); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: diagnostics: scan balance: %w", err)
		}
		if m.Stored, err = parseDecimal(stored); err == nil {
			m.LedgerSum, err = parseDecimal(sum)
		}
		if err == nil && after != nil {
			v, perr := parseDecimal(*after)
			m.LastAfter, err = &v, perr
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		out.Mismatches = append(out.Mismatches, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: diagnostics: balances: %w", err)
	}

	rows, err = s.pool.Query(ctx, driftQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("db: diagnostics: prices: %w", err)
	}
	for rows.Next() {
		var (
			d             billing.PriceDrift
			price, billed string
		)
		if err := rows.Scan(&d.SubscriberID, &d.Username, &d.PlanName, &price, &billed,
			&d.InvoiceID, &d.InvoicedAt, &out.DriftCount); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: diagnostics: scan price: %w", err)
		}
		if d.CataloguePrice, err = parseDecimal(price); err == nil {
			d.BilledBase, err = parseDecimal(billed)
		}
		if err != nil {
			rows.Close()
			return nil, err
		}
		out.Drifts = append(out.Drifts, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: diagnostics: prices: %w", err)
	}
	return out, nil
}
