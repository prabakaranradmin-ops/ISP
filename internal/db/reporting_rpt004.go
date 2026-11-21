package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/reporting"
)

// The reports added by FR-RPT-004.
//
// None of these needed a migration: the data was already being written, it
// just had no report reading it. They read the operational tables directly
// rather than the migration-032 views, because those views are
// pre-aggregated at a grain these questions do not share — and
// mv_ticket_resolution is materialised, so a report built on it would answer
// as of the last refresh rather than now.
//
// Every one carries the same franchise predicate as the original four, so a
// franchise-scoped caller sees their own rows and nobody else's.

// monthWindow is the trailing bound the month-windowed reports share. Written
// once because five of them need exactly the same expression, and five copies
// is five chances for one to drift to >= where the others have >.
const monthWindow = `date_trunc('month', NOW()) - make_interval(months => $1 - 1)`

// ExpiringSubscribers lists subscribers whose plan lapses within days.
//
// Expiries already past are included (days_left goes negative): a renewal
// call list that dropped them would lose exactly the customers who are
// already off service and most need the call.
func (s *ReportingStore) ExpiringSubscribers(ctx context.Context, days int, franchiseID *int) ([]reporting.ExpiringRow, error) {
	const q = `
		SELECT sub.id, sub.username, sub.mobile_number, p.name, sub.status, sub.franchise_id,
		       sub.plan_expiry,
		       (sub.plan_expiry::date - CURRENT_DATE) AS days_left,
		       p.price::text, sub.wallet_balance::text
		  FROM subscribers sub
		  JOIN plans p ON p.id = sub.plan_id
		 WHERE sub.status <> 'terminated'
		   AND sub.plan_expiry IS NOT NULL
		   AND sub.plan_expiry < NOW() + make_interval(days => $1)
		   AND ($2::int IS NULL OR sub.franchise_id = $2)
		 ORDER BY sub.plan_expiry, sub.id`

	rows, err := s.pool.Query(ctx, q, days, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: expiring subscribers: %w", err)
	}
	defer rows.Close()

	out := []reporting.ExpiringRow{}
	for rows.Next() {
		var r reporting.ExpiringRow
		var price, wallet string
		if err := rows.Scan(&r.SubscriberID, &r.Username, &r.MobileNumber, &r.PlanName,
			&r.Status, &r.FranchiseID, &r.PlanExpiry, &r.DaysLeft, &price, &wallet); err != nil {
			return nil, fmt.Errorf("db: scan expiring subscriber: %w", err)
		}
		if r.PlanPrice, err = parseDecimal(price); err != nil {
			return nil, err
		}
		if r.WalletBalance, err = parseDecimal(wallet); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PlanChangeHistory lists scheduled plan changes in the window.
//
// Keyed on scheduled_at, not effective_on: the question "what changed in
// October" is about decisions taken, and a change scheduled in October for
// January belongs to October's history. All three statuses are reported —
// what was withdrawn before it fired is part of the record, and a list of
// only the applied ones cannot explain a subscriber who expected a change
// and did not get one.
func (s *ReportingStore) PlanChangeHistory(ctx context.Context, months int, franchiseID *int) ([]reporting.PlanChangeRow, error) {
	const q = `
		SELECT c.subscriber_id, sub.username, fromp.name, top.name,
		       c.effective_on, c.status, COALESCE(c.reason, ''), COALESCE(c.scheduled_by, ''),
		       c.scheduled_at, c.applied_at, c.cancelled_at, COALESCE(c.cancelled_by, '')
		  FROM scheduled_plan_changes c
		  JOIN subscribers sub ON sub.id = c.subscriber_id
		  JOIN plans fromp ON fromp.id = c.from_plan_id
		  JOIN plans top   ON top.id   = c.new_plan_id
		 WHERE c.scheduled_at >= ` + monthWindow + `
		   AND ($2::int IS NULL OR sub.franchise_id = $2)
		 ORDER BY c.scheduled_at DESC, c.id DESC`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: plan change history: %w", err)
	}
	defer rows.Close()

	out := []reporting.PlanChangeRow{}
	for rows.Next() {
		var r reporting.PlanChangeRow
		if err := rows.Scan(&r.SubscriberID, &r.Username, &r.FromPlan, &r.ToPlan,
			&r.EffectiveOn, &r.Status, &r.Reason, &r.ScheduledBy, &r.ScheduledAt,
			&r.AppliedAt, &r.CancelledAt, &r.CancelledBy); err != nil {
			return nil, fmt.Errorf("db: scan plan change: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// paymentMoneyPredicate is what counts as money arriving, shared by the two
// collection reports so they cannot disagree with each other or with
// FR-COL-001's reconciliation.
//
// entry_type = 'credit', not amount > 0: wallet_ledgers stores both
// directions positive and keeps the direction in entry_type, so filtering on
// the sign excludes nothing at all. And status <> 'bounced', because
// migration 054 bounces a cheque with a reversing entry rather than flipping
// the original, which therefore stays — counting it would report money that
// came back as money taken.
const paymentMoneyPredicate = `
	    w.entry_type = 'credit'
	AND COALESCE(pd.status, '') <> 'bounced'`

// PaymentsByMethod reports what arrived and how, by month.
func (s *ReportingStore) PaymentsByMethod(ctx context.Context, months int, franchiseID *int) ([]reporting.PaymentMethodRow, error) {
	const q = `
		SELECT date_trunc('month', w.created_at) AS month,
		       COALESCE(pd.method, 'adjustment') AS method,
		       count(*), sum(w.amount)::text
		  FROM wallet_ledgers w
		  LEFT JOIN payment_details pd ON pd.wallet_ledger_id = w.id
		 WHERE ` + paymentMoneyPredicate + `
		   AND w.created_at >= ` + monthWindow + `
		   AND ($2::int IS NULL OR w.franchise_id = $2)
		 GROUP BY 1, 2
		 ORDER BY 1 DESC, 2`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: payments by method: %w", err)
	}
	defer rows.Close()

	out := []reporting.PaymentMethodRow{}
	for rows.Next() {
		var r reporting.PaymentMethodRow
		var total string
		if err := rows.Scan(&r.Month, &r.Method, &r.Payments, &total); err != nil {
			return nil, fmt.Errorf("db: scan payment method row: %w", err)
		}
		if r.Total, err = parseDecimal(total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CollectionByStaff reports who took which payments, by month.
//
// Grouped on the recorded_by text rather than a staff id, as FR-COL-001's
// takings are: deleting a staff account must not erase a cash trail.
//
// A credit with no payment_details row is a wallet adjustment, not a
// collection, and is excluded — it is money moved by an operator, not money
// handed over, and mixing the two would make a reconciliation unarguable
// either way.
func (s *ReportingStore) CollectionByStaff(ctx context.Context, months int, franchiseID *int) ([]reporting.StaffCollectionRow, error) {
	const q = `
		SELECT date_trunc('month', w.created_at) AS month,
		       COALESCE(NULLIF(pd.recorded_by, ''), 'unknown') AS recorded_by,
		       count(*),
		       COALESCE(sum(w.amount) FILTER (WHERE pd.method = 'cash'), 0)::text,
		       COALESCE(sum(w.amount) FILTER (WHERE pd.method <> 'cash'), 0)::text,
		       sum(w.amount)::text
		  FROM wallet_ledgers w
		  JOIN payment_details pd ON pd.wallet_ledger_id = w.id
		 WHERE ` + paymentMoneyPredicate + `
		   AND w.created_at >= ` + monthWindow + `
		   AND ($2::int IS NULL OR w.franchise_id = $2)
		 GROUP BY 1, 2
		 ORDER BY 1 DESC, 2`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: collection by staff: %w", err)
	}
	defer rows.Close()

	out := []reporting.StaffCollectionRow{}
	for rows.Next() {
		var r reporting.StaffCollectionRow
		var cash, digital, total string
		if err := rows.Scan(&r.Month, &r.RecordedBy, &r.Payments, &cash, &digital, &total); err != nil {
			return nil, fmt.Errorf("db: scan staff collection row: %w", err)
		}
		var err error
		if r.CashTotal, err = parseDecimal(cash); err != nil {
			return nil, err
		}
		if r.DigitalTotal, err = parseDecimal(digital); err != nil {
			return nil, err
		}
		if r.Total, err = parseDecimal(total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InvoiceRegister lists every invoice raised in the window.
func (s *ReportingStore) InvoiceRegister(ctx context.Context, months int, franchiseID *int) ([]reporting.InvoiceRegisterRow, error) {
	const q = `
		SELECT i.id, i.created_at, i.subscriber_id, sub.username, p.name, sub.franchise_id,
		       COALESCE(i.description, ''),
		       i.base_amount::text, i.cgst_amount::text, i.sgst_amount::text,
		       i.igst_amount::text, i.total_amount::text
		  FROM invoices i
		  JOIN subscribers sub ON sub.id = i.subscriber_id
		  JOIN plans p ON p.id = sub.plan_id
		 WHERE i.created_at >= ` + monthWindow + `
		   AND ($2::int IS NULL OR sub.franchise_id = $2)
		 ORDER BY i.created_at DESC, i.id DESC`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: invoice register: %w", err)
	}
	defer rows.Close()

	out := []reporting.InvoiceRegisterRow{}
	for rows.Next() {
		var r reporting.InvoiceRegisterRow
		var base, cgst, sgst, igst, total string
		if err := rows.Scan(&r.InvoiceID, &r.InvoiceDate, &r.SubscriberID, &r.Username,
			&r.PlanName, &r.FranchiseID, &r.Description,
			&base, &cgst, &sgst, &igst, &total); err != nil {
			return nil, fmt.Errorf("db: scan invoice register row: %w", err)
		}
		var err error
		if r.BaseAmount, err = parseDecimal(base); err != nil {
			return nil, err
		}
		if r.CGST, err = parseDecimal(cgst); err != nil {
			return nil, err
		}
		if r.SGST, err = parseDecimal(sgst); err != nil {
			return nil, err
		}
		if r.IGST, err = parseDecimal(igst); err != nil {
			return nil, err
		}
		if r.Total, err = parseDecimal(total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TaxCharged reports GST by month and rate.
//
// Grouped by the rate row rather than by the amounts, so a rate change
// mid-window produces two lines instead of one blended figure that
// reconciles against neither.
func (s *ReportingStore) TaxCharged(ctx context.Context, months int, franchiseID *int) ([]reporting.TaxRow, error) {
	const q = `
		SELECT date_trunc('month', i.created_at) AS month,
		       g.cgst_rate::text, g.sgst_rate::text, g.igst_rate::text,
		       count(*),
		       sum(i.base_amount)::text,
		       sum(i.cgst_amount)::text, sum(i.sgst_amount)::text, sum(i.igst_amount)::text,
		       sum(i.cgst_amount + i.sgst_amount + i.igst_amount)::text
		  FROM invoices i
		  JOIN gst_rates g ON g.id = i.gst_rate_id
		  JOIN subscribers sub ON sub.id = i.subscriber_id
		 WHERE i.created_at >= ` + monthWindow + `
		   AND ($2::int IS NULL OR sub.franchise_id = $2)
		 GROUP BY 1, 2, 3, 4
		 ORDER BY 1 DESC, 2`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: tax charged: %w", err)
	}
	defer rows.Close()

	out := []reporting.TaxRow{}
	for rows.Next() {
		var r reporting.TaxRow
		var cr, sr, ir, taxable, cgst, sgst, igst, total string
		if err := rows.Scan(&r.Month, &cr, &sr, &ir, &r.Invoices,
			&taxable, &cgst, &sgst, &igst, &total); err != nil {
			return nil, fmt.Errorf("db: scan tax row: %w", err)
		}
		var err error
		if r.CGSTRate, err = parseDecimal(cr); err != nil {
			return nil, err
		}
		if r.SGSTRate, err = parseDecimal(sr); err != nil {
			return nil, err
		}
		if r.IGSTRate, err = parseDecimal(ir); err != nil {
			return nil, err
		}
		if r.TaxableValue, err = parseDecimal(taxable); err != nil {
			return nil, err
		}
		if r.CGST, err = parseDecimal(cgst); err != nil {
			return nil, err
		}
		if r.SGST, err = parseDecimal(sgst); err != nil {
			return nil, err
		}
		if r.IGST, err = parseDecimal(igst); err != nil {
			return nil, err
		}
		if r.TaxTotal, err = parseDecimal(total); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TicketTurnaround reports how long tickets actually took.
//
// First arrival at resolved, not the last, matching mv_ticket_resolution's
// own CTE: a ticket closed, reopened and closed again is a support failure,
// and taking the last timestamp would report it as one slow success while
// hiding the reopen entirely.
//
// Read from ticket_status_history rather than from the materialised view
// beside it, so the figures are current rather than as of the last refresh —
// and because the view does not carry a P90 or a still-open count.
func (s *ReportingStore) TicketTurnaround(ctx context.Context, months int, franchiseID *int) ([]reporting.TurnaroundRow, error) {
	const q = `
		WITH first_resolution AS (
			SELECT ticket_id, min(occurred_at) AS resolved_at
			  FROM ticket_status_history
			 WHERE new_status = 'resolved'
			 GROUP BY ticket_id
		)
		SELECT date_trunc('month', t.created_at) AS month,
		       t.category, t.priority,
		       count(*),
		       count(fr.resolved_at),
		       percentile_cont(0.5) WITHIN GROUP (
		           ORDER BY EXTRACT(EPOCH FROM (fr.resolved_at - t.created_at)) / 3600.0),
		       percentile_cont(0.9) WITHIN GROUP (
		           ORDER BY EXTRACT(EPOCH FROM (fr.resolved_at - t.created_at)) / 3600.0),
		       max(EXTRACT(EPOCH FROM (fr.resolved_at - t.created_at)) / 3600.0),
		       count(*) FILTER (WHERE fr.resolved_at IS NULL)
		  FROM tickets t
		  LEFT JOIN first_resolution fr ON fr.ticket_id = t.id
		 WHERE t.created_at >= ` + monthWindow + `
		   AND ($2::int IS NULL OR t.franchise_id = $2)
		 GROUP BY 1, 2, 3
		 ORDER BY 1 DESC, 2, 3`

	rows, err := s.pool.Query(ctx, q, months, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: ticket turnaround: %w", err)
	}
	defer rows.Close()

	out := []reporting.TurnaroundRow{}
	for rows.Next() {
		var r reporting.TurnaroundRow
		if err := rows.Scan(&r.Month, &r.Category, &r.Priority, &r.Raised, &r.Resolved,
			&r.MedianHours, &r.P90Hours, &r.MaxHours, &r.StillOpen); err != nil {
			return nil, fmt.Errorf("db: scan turnaround row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TopDataUsers ranks consumption over the window.
//
// Every session that started in the window, open or closed — unlike the
// fair-use snapshot below, which counts only open ones. The questions differ:
// this is "who used the most last month", that is "who is over right now".
func (s *ReportingStore) TopDataUsers(ctx context.Context, months, limit int, franchiseID *int) ([]reporting.DataUserRow, error) {
	const q = `
		SELECT h.subscriber_id, sub.username, p.name, sub.franchise_id,
		       count(*),
		       COALESCE(sum(h.input_octets), 0),
		       COALESCE(sum(h.output_octets), 0),
		       COALESCE(sum(h.input_octets + h.output_octets), 0),
		       COALESCE(p.fup_threshold_bytes, 0)
		  FROM subscriber_session_history h
		  JOIN subscribers sub ON sub.id = h.subscriber_id
		  JOIN plans p ON p.id = sub.plan_id
		 WHERE h.start_time >= ` + monthWindow + `
		   AND ($2::int IS NULL OR sub.franchise_id = $2)
		 GROUP BY h.subscriber_id, sub.username, p.name, sub.franchise_id, p.fup_threshold_bytes
		 ORDER BY 8 DESC, h.subscriber_id
		 LIMIT $3`

	rows, err := s.pool.Query(ctx, q, months, franchiseID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: top data users: %w", err)
	}
	defer rows.Close()

	out := []reporting.DataUserRow{}
	for rows.Next() {
		var r reporting.DataUserRow
		if err := rows.Scan(&r.SubscriberID, &r.Username, &r.PlanName, &r.FranchiseID,
			&r.Sessions, &r.InputOctets, &r.OutputOctets, &r.TotalBytes,
			&r.AllowanceBytes); err != nil {
			return nil, fmt.Errorf("db: scan data user row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FUPExceeded lists subscribers currently over their fair-use allowance.
//
// Usage is summed across *open* sessions only, which is how the FUP scanner
// measures it (see liveSessionUsage in fup.go): a reconnect opens a new row
// while the quota is per cycle. Matching the scanner is the point — a report
// that counted closed sessions too would list subscribers whose throttle has
// already been lifted, and an operator comparing the two would conclude the
// scanner was broken.
//
// Top-ups raise the allowance rather than clearing a flag (FR-CAT-003), so
// they are added to the threshold here for the same reason.
func (s *ReportingStore) FUPExceeded(ctx context.Context, franchiseID *int) ([]reporting.FUPExceededRow, error) {
	const q = `
		WITH usage AS (
			SELECT h.subscriber_id, sum(h.input_octets + h.output_octets) AS bytes_used
			  FROM subscriber_session_history h
			 WHERE h.stop_time IS NULL
			 GROUP BY h.subscriber_id
		), topups AS (
			SELECT t.subscriber_id, COALESCE(sum(t.bytes), 0) AS topup_bytes
			  FROM data_topups t
			 WHERE t.expires_at > NOW()
			 GROUP BY t.subscriber_id
		)
		SELECT sub.id, sub.username, p.name, sub.franchise_id,
		       u.bytes_used,
		       COALESCE(p.fup_threshold_bytes, 0),
		       COALESCE(tp.topup_bytes, 0),
		       COALESCE(sub.fup_applied_rate, ''),
		       sub.fup_active
		  FROM usage u
		  JOIN subscribers sub ON sub.id = u.subscriber_id
		  JOIN plans p ON p.id = sub.plan_id
		  LEFT JOIN topups tp ON tp.subscriber_id = sub.id
		 WHERE sub.status <> 'terminated'
		   AND COALESCE(p.fup_threshold_bytes, 0) > 0
		   AND u.bytes_used > COALESCE(p.fup_threshold_bytes, 0) + COALESCE(tp.topup_bytes, 0)
		   AND ($1::int IS NULL OR sub.franchise_id = $1)
		 ORDER BY u.bytes_used DESC, sub.id`

	rows, err := s.pool.Query(ctx, q, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: fup exceeded: %w", err)
	}
	defer rows.Close()

	out := []reporting.FUPExceededRow{}
	for rows.Next() {
		var r reporting.FUPExceededRow
		if err := rows.Scan(&r.SubscriberID, &r.Username, &r.PlanName, &r.FranchiseID,
			&r.UsedBytes, &r.AllowanceBytes, &r.TopupBytes,
			&r.AppliedRate, &r.FUPActive); err != nil {
			return nil, fmt.Errorf("db: scan fup exceeded row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
