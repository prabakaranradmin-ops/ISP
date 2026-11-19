package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Scheduled plan changes — FR-BIL-023 | migration 059. See
// internal/billing/plan_change.go for the domain rules.

// planChangeColumns is the stored row joined to both plans. The new plan's
// price, validity and volume come along because the renewal path prices
// the next cycle from them.
const planChangeColumns = `
	c.id, c.subscriber_id, s.username,
	c.new_plan_id, np.name, np.price::text, np.validity_days, np.volume_gb,
	c.from_plan_id, fp.name,
	c.effective_on, c.status, COALESCE(c.reason, ''), c.scheduled_by, c.scheduled_at,
	c.applied_at, c.applied_invoice_id,
	c.cancelled_at, COALESCE(c.cancelled_by, ''), COALESCE(c.cancel_reason, '')`

const planChangeFrom = `
	FROM scheduled_plan_changes c
	JOIN subscribers s ON s.id = c.subscriber_id
	JOIN plans np      ON np.id = c.new_plan_id
	JOIN plans fp      ON fp.id = c.from_plan_id`

func scanPlanChange(row pgx.Row, c *billing.ScheduledPlanChange) error {
	var price string
	if err := row.Scan(&c.ID, &c.SubscriberID, &c.Username,
		&c.NewPlanID, &c.NewPlanName, &price, &c.NewPlanValidityDays, &c.NewPlanVolumeGB,
		&c.FromPlanID, &c.FromPlanName,
		&c.EffectiveOn, &c.Status, &c.Reason, &c.ScheduledBy, &c.ScheduledAt,
		&c.AppliedAt, &c.AppliedInvoiceID,
		&c.CancelledAt, &c.CancelledBy, &c.CancelReason); err != nil {
		return err
	}
	var err error
	c.NewPlanPrice, err = parseDecimal(price)
	return err
}

// SchedulePlanChange records a plan change for a future renewal.
//
// The subscriber's current plan becomes from_plan_id, read inside the
// transaction under a row lock so it cannot be the plan they were on a
// moment ago.
func (s *BillingStore) SchedulePlanChange(ctx context.Context, req billing.SchedulePlanChangeRequest, by string) (*billing.ScheduledPlanChange, error) {
	req, err := req.Normalise(time.Now())
	if err != nil {
		return nil, err
	}

	var out billing.ScheduledPlanChange
	err = inTx(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			currentPlan int
			status      string
		)
		err := tx.QueryRow(ctx, `
			SELECT plan_id, status FROM subscribers WHERE id = $1 FOR UPDATE`,
			req.SubscriberID).Scan(&currentPlan, &status)
		if isNoRows(err) {
			return fmt.Errorf("db: subscriber %d: %w", req.SubscriberID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("db: schedule plan change: load subscriber %d: %w", req.SubscriberID, err)
		}
		if status == "terminated" {
			return fmt.Errorf("%w: the subscriber is terminated", billing.ErrPlanChangeInvalid)
		}
		if currentPlan == req.NewPlanID {
			return billing.ErrPlanChangeSamePlan
		}

		row := tx.QueryRow(ctx, `
			WITH ins AS (
				INSERT INTO scheduled_plan_changes
					(subscriber_id, new_plan_id, from_plan_id, effective_on, reason, scheduled_by)
				VALUES ($1, $2, $3, $4::date, $5, $6)
				RETURNING *
			)
			SELECT `+planChangeColumns+`
			  FROM ins c
			  JOIN subscribers s ON s.id = c.subscriber_id
			  JOIN plans np      ON np.id = c.new_plan_id
			  JOIN plans fp      ON fp.id = c.from_plan_id`,
			req.SubscriberID, req.NewPlanID, currentPlan,
			req.EffectiveOn.Format("2006-01-02"), nullIfBlank(req.Reason), by)
		if err := scanPlanChange(row, &out); err != nil {
			switch {
			case uniqueViolation(err, "uq_spc_one_pending"):
				return billing.ErrPlanChangeAlreadyScheduled
			case foreignKeyViolation(err, "scheduled_plan_changes_new_plan_id_fkey"):
				return fmt.Errorf("%w: no plan with that id", billing.ErrPlanChangeInvalid)
			}
			return fmt.Errorf("db: schedule plan change for subscriber %d: %w", req.SubscriberID, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// nullIfBlank maps "" to SQL NULL so an absent note stays NULL rather than
// becoming an empty string that reads as "a note was given".
func nullIfBlank(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// CancelPlanChange withdraws a pending change, with a reason.
func (s *BillingStore) CancelPlanChange(ctx context.Context, id int, reason, by string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: give a reason for cancelling", billing.ErrPlanChangeInvalid)
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE scheduled_plan_changes
		   SET status = 'cancelled', cancelled_at = now(), cancelled_by = $3, cancel_reason = $2
		 WHERE id = $1 AND status = 'scheduled'`, id, reason, by)
	if err != nil {
		return fmt.Errorf("db: cancel plan change %d: %w", id, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var status string
	if err := s.pool.QueryRow(ctx,
		`SELECT status FROM scheduled_plan_changes WHERE id = $1`, id).Scan(&status); err != nil {
		if isNoRows(err) {
			return fmt.Errorf("db: plan change %d: %w", id, billing.ErrPlanChangeNotFound)
		}
		return fmt.Errorf("db: cancel plan change %d: %w", id, err)
	}
	return fmt.Errorf("%w: it is %s", billing.ErrPlanChangeNotPending, status)
}

// GetPlanChange loads one scheduled change.
func (s *BillingStore) GetPlanChange(ctx context.Context, id int) (*billing.ScheduledPlanChange, error) {
	var out billing.ScheduledPlanChange
	err := scanPlanChange(s.pool.QueryRow(ctx,
		`SELECT `+planChangeColumns+planChangeFrom+` WHERE c.id = $1`, id), &out)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: plan change %d: %w", id, billing.ErrPlanChangeNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get plan change %d: %w", id, err)
	}
	return &out, nil
}

// ListPlanChanges returns scheduled changes, pending first then newest.
func (s *BillingStore) ListPlanChanges(ctx context.Context, f billing.PlanChangeFilter) ([]billing.ScheduledPlanChange, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 200
	}
	const q = `
		SELECT ` + planChangeColumns + planChangeFrom + `
		 WHERE ($1 = '' OR c.status = $1)
		   AND ($2 = 0 OR c.subscriber_id = $2)
		 ORDER BY c.status <> 'scheduled', c.effective_on, c.scheduled_at DESC
		 LIMIT $3`

	rows, err := s.pool.Query(ctx, q, f.Status, f.SubscriberID, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: list plan changes: %w", err)
	}
	defer rows.Close()

	out := make([]billing.ScheduledPlanChange, 0, 16)
	for rows.Next() {
		var c billing.ScheduledPlanChange
		if err := scanPlanChange(rows, &c); err != nil {
			return nil, fmt.Errorf("db: scan plan change: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate plan changes: %w", err)
	}
	return out, nil
}

// GetDuePlanChange returns the change the renewal path should apply for
// this subscriber on the given day, or (nil, nil) if there is none.
//
// Read before the cycle is priced, so the invoice is raised at the new
// plan's price — the subscriber pays for what they are about to get.
func (s *BillingStore) GetDuePlanChange(ctx context.Context, subscriberID int, on time.Time) (*billing.ScheduledPlanChange, error) {
	var out billing.ScheduledPlanChange
	err := scanPlanChange(s.pool.QueryRow(ctx,
		`SELECT `+planChangeColumns+planChangeFrom+`
		  WHERE c.subscriber_id = $1 AND c.status = 'scheduled'
		    AND c.effective_on <= $2::date
		  ORDER BY c.effective_on
		  LIMIT 1`,
		subscriberID, billing.DayOf(on).Format("2006-01-02")), &out)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get due plan change for subscriber %d: %w", subscriberID, err)
	}
	return &out, nil
}

// SetPlanChangeInvoice links an applied change to the invoice that billed
// its first cycle on the new plan.
//
// Separate from ApplyDuePlanChange because the invoice does not exist yet
// when the change is applied: the renewal path writes the invoice last, so
// that a failure there cannot undo a move the subscriber has already paid
// for. The link is a convenience for answering "what did this change cost
// them", so the caller logs a failure here and carries on rather than
// failing a renewal that has otherwise completed.
func (s *BillingStore) SetPlanChangeInvoice(ctx context.Context, changeID, invoiceID int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE scheduled_plan_changes SET applied_invoice_id = $2
		 WHERE id = $1 AND status = 'applied' AND applied_invoice_id IS NULL`,
		changeID, invoiceID)
	if err != nil {
		return fmt.Errorf("db: link plan change %d to invoice %d: %w", changeID, invoiceID, err)
	}
	return nil
}

// ApplyDuePlanChange moves the subscriber onto the new plan, stamps the new
// expiry, and marks the change applied — in one transaction.
//
// Called by the renewal path *after* the wallet debit has committed, so a
// subscriber who could not pay is left on their old plan with the change
// still waiting, rather than moved onto a plan they have not bought.
//
// `activate` clears a first-cycle subscriber's pending_payment status at
// the same time, for the reason migration 048 exists: the expiry and the
// status must not be settable independently.
//
// The subscriber's current plan is not required to still be from_plan_id.
// An immediate change may have moved them in the meantime, and the
// instruction recorded here is "put them on new_plan_id" — honouring it is
// less surprising than silently dropping it because the starting point
// moved. If they are already on the target plan the change is marked
// applied without touching plan_id, since there is nothing left to do.
func (s *BillingStore) ApplyDuePlanChange(ctx context.Context, changeID, subscriberID, newPlanID int, newExpiry time.Time, activate bool, invoiceID *int) error {
	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The guard is on the change, not the subscriber: exactly one
		// transaction may take a pending change, so two overlapping
		// renewal passes cannot both apply it.
		tag, err := tx.Exec(ctx, `
			UPDATE scheduled_plan_changes
			   SET status = 'applied', applied_at = now(), applied_invoice_id = $2
			 WHERE id = $1 AND status = 'scheduled'`, changeID, invoiceID)
		if err != nil {
			return fmt.Errorf("db: apply plan change %d: mark applied: %w", changeID, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("db: plan change %d: %w", changeID, billing.ErrPlanChangeNotPending)
		}

		if activate {
			_, err = tx.Exec(ctx, `
				UPDATE subscribers
				   SET plan_id = $2, plan_expiry = $3, status = 'active'
				 WHERE id = $1`, subscriberID, newPlanID, newExpiry)
		} else {
			_, err = tx.Exec(ctx, `
				UPDATE subscribers
				   SET plan_id = $2, plan_expiry = $3
				 WHERE id = $1`, subscriberID, newPlanID, newExpiry)
		}
		if err != nil {
			return fmt.Errorf("db: apply plan change %d: move subscriber %d: %w", changeID, subscriberID, err)
		}
		return nil
	})
}
