package db

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/staffui"
)

// ConsoleStore serves the operations console's cross-subscriber screens: the
// home dashboard and the filterable subscriber directory.
//
// These are reads over tables other stores already own (subscribers,
// live_sessions, subscriber_status_history, tickets). They live here, rather
// than on those stores, because each answers a console question ("how many
// are expiring this week") that no API endpoint asks, and a store per
// consumer is this package's rule (see db.go).
type ConsoleStore struct{ pool dbPool }

var _ staffui.ConsoleStore = (*ConsoleStore)(nil)

// Console returns the store backing the console's dashboard and directory.
func (d *DB) Console() *ConsoleStore { return &ConsoleStore{pool: d.pool} }

// liveSessionTTLSeconds matches cache.SessionTTL (30 minutes). A live_sessions
// row older than that is treated as offline by every other reader, and the
// dashboard must agree with them or "online" would count subscribers the
// subscriber page itself calls offline. Repeated rather than imported, since
// internal/cache is a consumer of this layer's tables, not a dependency of it.
const liveSessionTTLSeconds = 30 * 60

// DashboardCounts gathers the dashboard in a single statement, so every tile
// is read from one snapshot instead of drifting across several queries.
func (s *ConsoleStore) DashboardCounts(ctx context.Context) (*staffui.DashboardCounts, error) {
	const q = `
		SELECT
		  (SELECT count(*) FROM live_sessions
		    WHERE updated_at > now() - ($1 * interval '1 second')),
		  count(*),
		  count(*) FILTER (WHERE status = 'pending_payment'),
		  count(*) FILTER (WHERE status = 'active'),
		  count(*) FILTER (WHERE status = 'grace_period'),
		  count(*) FILTER (WHERE status = 'soft_suspended'),
		  count(*) FILTER (WHERE status = 'hard_suspended'),
		  count(*) FILTER (WHERE status = 'terminated'),
		  count(*) FILTER (WHERE status = 'active'
		                     AND plan_expiry >= now()
		                     AND plan_expiry <  now() + interval '7 days'),
		  count(*) FILTER (WHERE status <> 'terminated' AND plan_expiry < now()),
		  count(*) FILTER (WHERE created_at >= now() - interval '7 days'),
		  (SELECT count(DISTINCT subscriber_id) FROM subscriber_status_history
		    WHERE new_status = 'terminated' AND NOT is_baseline
		      AND occurred_at >= now() - interval '30 days'),
		  (SELECT count(*) FROM tickets WHERE status IN ('open','in_progress')),
		  (SELECT count(*) FROM tickets WHERE status IN ('open','in_progress')
		      AND sla_resolution_due_at < now()),
		  (SELECT count(*) FROM tickets WHERE status IN ('open','in_progress')
		      AND assigned_to IS NULL)
		FROM subscribers`

	c := &staffui.DashboardCounts{ByStatus: make(map[string]int, len(staffui.SubscriberStatuses))}
	var pending, active, grace, soft, hard, term int
	err := s.pool.QueryRow(ctx, q, liveSessionTTLSeconds).Scan(
		&c.Online, &c.Total,
		&pending, &active, &grace, &soft, &hard, &term,
		&c.ExpiringWeek, &c.Expired, &c.NewWeek, &c.Churned30d,
		&c.OpenTickets, &c.OverdueTickets, &c.UnassignedTickets,
	)
	if err != nil {
		return nil, fmt.Errorf("db: dashboard counts: %w", err)
	}
	c.ByStatus["pending_payment"] = pending
	c.ByStatus["active"] = active
	c.ByStatus["grace_period"] = grace
	c.ByStatus["soft_suspended"] = soft
	c.ByStatus["hard_suspended"] = hard
	c.ByStatus["terminated"] = term
	return c, nil
}

const defaultDirectoryLimit = 500

// ListDirectory returns the subscribers matching f.
func (s *ConsoleStore) ListDirectory(ctx context.Context, f staffui.DirectoryFilter) (*staffui.DirectoryPage, error) {
	if f.Status != "" && !inList(staffui.SubscriberStatuses, f.Status) {
		return nil, fmt.Errorf("db: unknown subscriber status %q", f.Status)
	}
	if f.View != "" && !inList(staffui.DirectoryViews, f.View) {
		return nil, fmt.Errorf("db: unknown directory view %q", f.View)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultDirectoryLimit
	}

	query := strings.TrimSpace(f.Query)
	// Only a value that fits subscribers.id (int4) is tried as an id. A
	// phone number is all digits too, and casting 9876543210 to int fails
	// the whole statement with "out of range" — which is what a CSR typing
	// a caller's number would otherwise get instead of a match.
	var idQuery *int
	if n, err := strconv.ParseInt(query, 10, 32); err == nil && n > 0 {
		id := int(n)
		idQuery = &id
	}

	// One statement with NULL-guarded predicates, same reasoning as
	// RevenueStore.ListSubscribers: a second query string is a second place
	// for a filter to be forgotten. The LIKE pattern escapes the operator's
	// own % and _ so "50%" searches for that text rather than everything.
	const q = `
		WITH live AS (
		  SELECT subscriber_id, assigned_ip FROM live_sessions
		   WHERE updated_at > now() - ($1 * interval '1 second')
		),
		churned AS (
		  SELECT DISTINCT subscriber_id FROM subscriber_status_history
		   WHERE new_status = 'terminated' AND NOT is_baseline
		     AND occurred_at >= now() - interval '30 days'
		),
		matched AS (
		  SELECT s.id, s.username, s.status, s.mobile_number, p.name AS plan_name,
		         s.plan_expiry, s.created_at, (l.subscriber_id IS NOT NULL) AS online
		    FROM subscribers s
		    LEFT JOIN plans p ON p.id = s.plan_id
		    LEFT JOIN live l ON l.subscriber_id = s.id
		   WHERE ($2::text = '' OR s.status = $2)
		     AND ($3::text = ''
		          OR ($3 = 'expiring' AND s.status = 'active'
		              AND s.plan_expiry >= now() AND s.plan_expiry < now() + interval '7 days')
		          OR ($3 = 'expired' AND s.status <> 'terminated' AND s.plan_expiry < now())
		          OR ($3 = 'new' AND s.created_at >= now() - interval '7 days')
		          OR ($3 = 'churned' AND s.id IN (SELECT subscriber_id FROM churned))
		          OR ($3 = 'online' AND l.subscriber_id IS NOT NULL))
		     AND ($4::text = ''
		          OR s.username ILIKE $5 ESCAPE '\'
		          OR s.mobile_number ILIKE $5 ESCAPE '\'
		          OR s.email ILIKE $5 ESCAPE '\'
		          OR s.caf_number = $4
		          OR l.assigned_ip = $4
		          OR s.id = $6::int)
		)
		SELECT id, username, status, mobile_number, COALESCE(plan_name, ''),
		       plan_expiry, created_at, online,
		       (SELECT count(*) FROM matched),
		       (SELECT count(*) FROM subscribers)
		  FROM matched
		 ORDER BY id
		 LIMIT $7`

	rows, err := s.pool.Query(ctx, q, liveSessionTTLSeconds, f.Status, f.View,
		query, "%"+escapeLike(query)+"%", idQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list directory: %w", err)
	}
	defer rows.Close()

	page := &staffui.DirectoryPage{Rows: make([]staffui.DirectoryRow, 0, 32)}
	for rows.Next() {
		var r staffui.DirectoryRow
		if err := rows.Scan(&r.ID, &r.Username, &r.Status, &r.MobileNumber, &r.PlanName,
			&r.PlanExpiry, &r.CreatedAt, &r.Online, &page.Matched, &page.Total); err != nil {
			return nil, fmt.Errorf("db: scan directory row: %w", err)
		}
		page.Rows = append(page.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate directory: %w", err)
	}
	// No rows means the count columns were never read; the unfiltered total
	// is still worth showing ("0 of 340"), so fetch it on its own.
	if len(page.Rows) == 0 {
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM subscribers`).Scan(&page.Total); err != nil {
			return nil, fmt.Errorf("db: count subscribers: %w", err)
		}
	}
	page.Truncated = page.Matched > len(page.Rows)
	return page, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func inList(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

const defaultSessionLimit = 500

// ListLiveSessions returns subscribers online now, newest session first.
//
// "Online" uses the same freshness rule as every other reader of
// live_sessions (liveSessionTTLSeconds), so this list, the dashboard's count
// and the subscriber page's online badge cannot disagree.
func (s *ConsoleStore) ListLiveSessions(ctx context.Context, f staffui.SessionFilter) ([]staffui.LiveSession, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultSessionLimit
	}
	query := strings.TrimSpace(f.Query)
	const q = `
		SELECT l.subscriber_id, s.username, l.session_id, l.nas_ip, l.assigned_ip,
		       l.bytes_in, l.bytes_out, l.speed_profile, l.fup_throttled,
		       l.started_at, l.updated_at
		  FROM live_sessions l
		  JOIN subscribers s ON s.id = l.subscriber_id
		 WHERE l.updated_at > now() - ($1 * interval '1 second')
		   AND (NOT $2::bool OR l.fup_throttled)
		   AND ($3::text = ''
		        OR s.username ILIKE $4 ESCAPE '\'
		        OR l.assigned_ip = $3 OR l.nas_ip = $3 OR l.session_id = $3)
		 ORDER BY l.started_at DESC
		 LIMIT $5`
	rows, err := s.pool.Query(ctx, q, liveSessionTTLSeconds, f.FUPOnly,
		query, "%"+escapeLike(query)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("db: list live sessions: %w", err)
	}
	defer rows.Close()

	out := make([]staffui.LiveSession, 0, 32)
	for rows.Next() {
		var l staffui.LiveSession
		if err := rows.Scan(&l.SubscriberID, &l.Username, &l.SessionID, &l.NasIP, &l.AssignedIP,
			&l.BytesIn, &l.BytesOut, &l.SpeedProfile, &l.FUPThrottled,
			&l.StartedAt, &l.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: scan live session: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate live sessions: %w", err)
	}
	return out, nil
}

// ListCompletedSessions returns sessions that ended between f.From and f.To,
// most recent first.
//
// The start_time < To predicate is redundant for correctness (a session
// starts before it stops) but lets PostgreSQL skip the future monthly
// partitions of subscriber_session_history without scanning them.
func (s *ConsoleStore) ListCompletedSessions(ctx context.Context, f staffui.SessionFilter) ([]staffui.CompletedSession, error) {
	if f.From.IsZero() || f.To.IsZero() || !f.To.After(f.From) {
		return nil, fmt.Errorf("db: completed sessions need a From before To, got %v..%v", f.From, f.To)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultSessionLimit
	}
	query := strings.TrimSpace(f.Query)
	const q = `
		SELECT h.subscriber_id, s.username, h.session_id,
		       COALESCE(host(h.nas_ip_address), ''), COALESCE(host(h.assigned_ipv4), ''),
		       h.start_time, h.stop_time, h.input_octets, h.output_octets,
		       COALESCE(h.terminate_cause, '')
		  FROM subscriber_session_history h
		  JOIN subscribers s ON s.id = h.subscriber_id
		 WHERE h.stop_time IS NOT NULL
		   AND h.stop_time >= $1 AND h.stop_time < $2
		   AND h.start_time < $2
		   AND ($3::text = ''
		        OR s.username ILIKE $4 ESCAPE '\'
		        OR host(h.assigned_ipv4) = $3 OR host(h.nas_ip_address) = $3
		        OR h.session_id = $3)
		 ORDER BY h.stop_time DESC
		 LIMIT $5`
	rows, err := s.pool.Query(ctx, q, f.From, f.To, query, "%"+escapeLike(query)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("db: list completed sessions: %w", err)
	}
	defer rows.Close()

	out := make([]staffui.CompletedSession, 0, 32)
	for rows.Next() {
		var c staffui.CompletedSession
		if err := rows.Scan(&c.SubscriberID, &c.Username, &c.SessionID, &c.NasIP, &c.AssignedIP,
			&c.StartTime, &c.StopTime, &c.InputOctets, &c.OutputOctets, &c.TerminateCause); err != nil {
			return nil, fmt.Errorf("db: scan completed session: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate completed sessions: %w", err)
	}
	return out, nil
}

// ── Ticket queue (FR-CON-005) ───────────────────────────────────────────────

const defaultTicketLimit = 300

// ListTicketQueue returns tickets across all subscribers matching f.
//
// Ordering puts what needs attention first: overdue, then by priority
// (critical → low), then the soonest resolution deadline, then the oldest.
// The "closed" and "all" views order by most recently updated instead, since
// nothing in them is waiting on anyone.
func (s *ConsoleStore) ListTicketQueue(ctx context.Context, f staffui.TicketFilter) ([]staffui.QueueTicket, error) {
	view := f.View
	if view == "" {
		view = "open"
	}
	if !inList(staffui.TicketViews, view) {
		return nil, fmt.Errorf("db: unknown ticket view %q", f.View)
	}
	if f.Priority != "" && !inList(staffui.TicketPriorities, f.Priority) {
		return nil, fmt.Errorf("db: unknown ticket priority %q", f.Priority)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultTicketLimit
	}
	query := strings.TrimSpace(f.Query)
	var idQuery *int
	if n, err := strconv.ParseInt(query, 10, 32); err == nil && n > 0 {
		id := int(n)
		idQuery = &id
	}
	var subID *int
	if f.SubscriberID > 0 {
		subID = &f.SubscriberID
	}

	const q = `
		SELECT t.id, t.subscriber_id, s.username, t.category, t.description,
		       t.priority, t.status, t.assigned_to, COALESCE(NULLIF(u.full_name, ''), u.username, ''),
		       t.sla_response_due_at, t.sla_resolution_due_at, t.created_at, t.updated_at,
		       (t.status IN ('open','in_progress') AND t.sla_resolution_due_at < now()) AS overdue
		  FROM tickets t
		  JOIN subscribers s ON s.id = t.subscriber_id
		  LEFT JOIN staff_users u ON u.id = t.assigned_to
		 WHERE CASE $1::text
		         WHEN 'open'       THEN t.status IN ('open','in_progress')
		         WHEN 'overdue'    THEN t.status IN ('open','in_progress') AND t.sla_resolution_due_at < now()
		         WHEN 'unassigned' THEN t.status IN ('open','in_progress') AND t.assigned_to IS NULL
		         WHEN 'mine'       THEN t.status IN ('open','in_progress')
		                            AND t.assigned_to = (SELECT id FROM staff_users WHERE username = $2)
		         WHEN 'closed'     THEN t.status IN ('resolved','closed') AND t.updated_at >= now() - interval '30 days'
		         ELSE true
		       END
		   AND ($3::text = '' OR t.priority = $3)
		   AND ($4::int IS NULL OR t.subscriber_id = $4)
		   AND ($5::text = ''
		        OR t.id = $6::int OR t.subscriber_id = $6::int
		        OR s.username ILIKE $7 ESCAPE '\'
		        OR t.description ILIKE $7 ESCAPE '\')
		 ORDER BY
		   CASE WHEN $1 IN ('closed','all') THEN t.updated_at END DESC,
		   (t.status IN ('open','in_progress') AND t.sla_resolution_due_at < now()) DESC,
		   CASE t.priority WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
		   t.sla_resolution_due_at ASC NULLS LAST,
		   t.created_at ASC
		 LIMIT $8`
	rows, err := s.pool.Query(ctx, q, view, f.StaffUsername, f.Priority, subID,
		query, idQuery, "%"+escapeLike(query)+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("db: list ticket queue: %w", err)
	}
	defer rows.Close()

	out := make([]staffui.QueueTicket, 0, 32)
	for rows.Next() {
		var t staffui.QueueTicket
		if err := rows.Scan(&t.ID, &t.SubscriberID, &t.Username, &t.Category, &t.Description,
			&t.Priority, &t.Status, &t.AssignedTo, &t.AssigneeName,
			&t.ResponseDue, &t.ResolutionDue, &t.CreatedAt, &t.UpdatedAt, &t.Overdue); err != nil {
			return nil, fmt.Errorf("db: scan ticket: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate tickets: %w", err)
	}
	return out, nil
}

// HelpdeskSummary computes the figures above the queue.
//
// "Closed in 30 days" and the median both use the first time a ticket
// reached resolved or closed, from ticket_status_history (migration 031),
// the same definition FR-RPT-001's resolution report uses: a ticket reopened
// and closed again is not counted twice, and its time-to-close is not reset.
// Baseline rows are excluded — their occurred_at is when capture began, not
// when the ticket was resolved.
func (s *ConsoleStore) HelpdeskSummary(ctx context.Context) (*staffui.HelpdeskSummary, error) {
	const q = `
		WITH first_close AS (
		  SELECT t.id, t.created_at, min(h.occurred_at) AS closed_at
		    FROM tickets t
		    JOIN ticket_status_history h ON h.ticket_id = t.id
		   WHERE h.new_status IN ('resolved','closed') AND NOT h.is_baseline
		   GROUP BY t.id, t.created_at
		)
		SELECT
		  (SELECT count(*) FROM tickets WHERE status IN ('open','in_progress')),
		  (SELECT count(*) FROM tickets WHERE status IN ('open','in_progress') AND sla_resolution_due_at < now()),
		  (SELECT count(*) FROM tickets WHERE status IN ('open','in_progress') AND assigned_to IS NULL),
		  (SELECT count(*) FROM first_close WHERE closed_at >= now() - interval '30 days'),
		  (SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM closed_at - created_at) / 3600.0)
		     FROM first_close WHERE closed_at >= now() - interval '30 days')`
	var sum staffui.HelpdeskSummary
	if err := s.pool.QueryRow(ctx, q).Scan(&sum.Open, &sum.Overdue, &sum.Unassigned,
		&sum.Closed30d, &sum.MedianCloseHours); err != nil {
		return nil, fmt.Errorf("db: helpdesk summary: %w", err)
	}
	return &sum, nil
}

// ListTicketAssignees returns active staff in the roles the API lets update
// tickets (csrOrTech: csr, technician, isp_owner), so the console never
// offers an assignee who could not then work the ticket.
func (s *ConsoleStore) ListTicketAssignees(ctx context.Context) ([]staffui.Assignee, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, username, COALESCE(full_name, ''), role
		  FROM staff_users
		 WHERE active AND role IN ('csr','technician','isp_owner')
		 ORDER BY role, username`)
	if err != nil {
		return nil, fmt.Errorf("db: list ticket assignees: %w", err)
	}
	defer rows.Close()
	var out []staffui.Assignee
	for rows.Next() {
		var a staffui.Assignee
		if err := rows.Scan(&a.ID, &a.Username, &a.FullName, &a.Role); err != nil {
			return nil, fmt.Errorf("db: scan assignee: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── Invoices and payments (FR-CON-006) ──────────────────────────────────────

const defaultMoneyLimit = 500

// moneySearch splits a search box value into the forms ListInvoices and
// ListPayments match: an id (int4-safe) and an INV-nnnnnn invoice number.
func moneySearch(query string) (idQuery *int, invoiceNo *int) {
	if n, err := strconv.ParseInt(query, 10, 32); err == nil && n > 0 {
		id := int(n)
		idQuery = &id
	}
	upper := strings.ToUpper(query)
	if strings.HasPrefix(upper, "INV-") {
		if n, err := strconv.ParseInt(strings.TrimPrefix(upper, "INV-"), 10, 32); err == nil && n > 0 {
			id := int(n)
			invoiceNo = &id
		}
	}
	return idQuery, invoiceNo
}

func checkWindow(f staffui.MoneyFilter) error {
	if f.From.IsZero() || f.To.IsZero() || !f.To.After(f.From) {
		return fmt.Errorf("db: money lists need a From before To, got %v..%v", f.From, f.To)
	}
	return nil
}

// ListInvoices returns invoices raised in the window, newest first, and the
// totals over every matching invoice (not only the rows returned).
//
// Sums are computed in SQL on NUMERIC and returned as text, the same rule as
// the rest of this package: money never passes through float64.
func (s *ConsoleStore) ListInvoices(ctx context.Context, f staffui.MoneyFilter) ([]staffui.InvoiceRow, *staffui.InvoiceTotals, error) {
	if err := checkWindow(f); err != nil {
		return nil, nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultMoneyLimit
	}
	query := strings.TrimSpace(f.Query)
	idQuery, invNo := moneySearch(query)

	const q = `
		WITH credited AS (
		  SELECT invoice_id, sum(total_amount) AS amount FROM credit_notes GROUP BY invoice_id
		),
		matched AS (
		  SELECT i.id, i.subscriber_id, s.username, i.created_at, i.base_amount,
		         (i.cgst_amount + i.sgst_amount + i.igst_amount) AS tax, i.total_amount,
		         COALESCE(c.amount, 0) AS credited
		    FROM invoices i
		    JOIN subscribers s ON s.id = i.subscriber_id
		    LEFT JOIN credited c ON c.invoice_id = i.id
		   WHERE i.created_at >= $1 AND i.created_at < $2
		     AND ($3::text = ''
		          OR s.username ILIKE $4 ESCAPE '\'
		          OR i.id = $5::int OR i.subscriber_id = $5::int
		          OR i.id = $6::int)
		)
		SELECT id, subscriber_id, username, created_at,
		       base_amount::text, tax::text, total_amount::text, credited::text,
		       (SELECT count(*) FROM matched),
		       (SELECT COALESCE(sum(base_amount), 0)::text FROM matched),
		       (SELECT COALESCE(sum(tax), 0)::text FROM matched),
		       (SELECT COALESCE(sum(total_amount), 0)::text FROM matched),
		       (SELECT COALESCE(sum(credited), 0)::text FROM matched)
		  FROM matched
		 ORDER BY created_at DESC, id DESC
		 LIMIT $7`
	rows, err := s.pool.Query(ctx, q, f.From, f.To, query, "%"+escapeLike(query)+"%", idQuery, invNo, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("db: list invoices: %w", err)
	}
	defer rows.Close()

	out := make([]staffui.InvoiceRow, 0, 32)
	tot := &staffui.InvoiceTotals{Base: "0.00", Tax: "0.00", Total: "0.00", Credited: "0.00"}
	for rows.Next() {
		var r staffui.InvoiceRow
		if err := rows.Scan(&r.ID, &r.SubscriberID, &r.Username, &r.CreatedAt,
			&r.Base, &r.Tax, &r.Total, &r.Credited,
			&tot.Count, &tot.Base, &tot.Tax, &tot.Total, &tot.Credited); err != nil {
			return nil, nil, fmt.Errorf("db: scan invoice: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("db: iterate invoices: %w", err)
	}
	return out, tot, nil
}

// ListPayments returns wallet top-ups in the window, newest first, with the
// total over every matching payment.
//
// A payment is a credit to the subscriber wallet with no staff actor: the
// recharge path (gateway webhook, portal, counter recharge) never sets
// adjusted_by_username, while every staff adjustment and approved credit
// does (FR-BIL-010, FR-WFL-001). Debits — renewals, refunds — are not
// payments either.
func (s *ConsoleStore) ListPayments(ctx context.Context, f staffui.MoneyFilter) ([]staffui.PaymentRow, *staffui.PaymentTotals, error) {
	if err := checkWindow(f); err != nil {
		return nil, nil, err
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultMoneyLimit
	}
	query := strings.TrimSpace(f.Query)
	idQuery, _ := moneySearch(query)

	const q = `
		WITH matched AS (
		  SELECT w.id, w.subscriber_id, s.username, w.created_at, w.amount, w.balance_after,
		         regexp_replace(COALESCE(w.description, ''), '^recharge via ', '') AS method,
		         COALESCE(w.transaction_token, '') AS reference
		    FROM wallet_ledgers w
		    JOIN subscribers s ON s.id = w.subscriber_id
		   WHERE w.account = 'subscriber_wallet'
		     AND w.entry_type = 'credit'
		     AND w.adjusted_by_username IS NULL
		     AND w.created_at >= $1 AND w.created_at < $2
		     AND ($3::text = ''
		          OR s.username ILIKE $4 ESCAPE '\'
		          OR w.subscriber_id = $5::int
		          OR w.transaction_token = $3)
		)
		SELECT id, subscriber_id, username, created_at, amount::text, balance_after::text,
		       method, reference,
		       (SELECT count(*) FROM matched),
		       (SELECT COALESCE(sum(amount), 0)::text FROM matched)
		  FROM matched
		 ORDER BY created_at DESC, id DESC
		 LIMIT $6`
	rows, err := s.pool.Query(ctx, q, f.From, f.To, query, "%"+escapeLike(query)+"%", idQuery, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("db: list payments: %w", err)
	}
	defer rows.Close()

	out := make([]staffui.PaymentRow, 0, 32)
	tot := &staffui.PaymentTotals{Amount: "0.00"}
	for rows.Next() {
		var r staffui.PaymentRow
		if err := rows.Scan(&r.ID, &r.SubscriberID, &r.Username, &r.CreatedAt, &r.Amount,
			&r.BalanceAfter, &r.Method, &r.Reference, &tot.Count, &tot.Amount); err != nil {
			return nil, nil, fmt.Errorf("db: scan payment: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("db: iterate payments: %w", err)
	}
	return out, tot, nil
}
