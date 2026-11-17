package db

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// DunningPolicy reads the ladder timing (FR-BIL-017, migration 055).
func (s *BillingStore) DunningPolicy(ctx context.Context) (billing.DunningPolicy, error) {
	var p billing.DunningPolicy
	var by *string
	err := s.pool.QueryRow(ctx, `
		SELECT remind_first_days, remind_second_days, remind_last_days, grace_days,
		       soft_suspend_days, updated_by, updated_at
		  FROM billing_settings WHERE id = 1`).Scan(
		&p.RemindFirstDays, &p.RemindSecondDays, &p.RemindLastDays, &p.GraceDays,
		&p.SoftSuspendDays, &by, &p.UpdatedAt)
	if err != nil {
		return billing.DunningPolicy{}, fmt.Errorf("db: read dunning policy: %w", err)
	}
	if by != nil {
		p.UpdatedBy = *by
	}
	return p, nil
}

// SetDunningPolicy saves the ladder timing. The table's CHECKs are the last
// word on what is valid; the caller validates first for a readable reason.
func (s *BillingStore) SetDunningPolicy(ctx context.Context, p billing.DunningPolicy, by string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE billing_settings
		   SET remind_first_days = $1, remind_second_days = $2, remind_last_days = $3,
		       grace_days = $4, soft_suspend_days = $5, updated_by = $6, updated_at = now()
		 WHERE id = 1`,
		p.RemindFirstDays, p.RemindSecondDays, p.RemindLastDays, p.GraceDays, p.SoftSuspendDays, by)
	if err != nil {
		return fmt.Errorf("db: save dunning policy: %w", err)
	}
	return nil
}

// SetGraceExtension holds a subscriber in grace until a date (FR-BIL-016).
// It does not change their state itself: the dunning scanner applies it on
// its next pass, through the state machine, so a grace change follows the
// same path and leaves the same audit trail as every other dunning move.
func (s *BillingStore) SetGraceExtension(ctx context.Context, subscriberID int, until time.Time, reason, by string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE subscribers
		   SET grace_until = $2, grace_reason = $3, grace_set_by = $4
		 WHERE id = $1 AND status <> 'terminated'
		   AND plan_expiry IS NOT NULL AND plan_expiry <= now()`,
		subscriberID, until, reason, by)
	if err != nil {
		return fmt.Errorf("db: set grace for subscriber %d: %w", subscriberID, err)
	}
	if tag.RowsAffected() == 0 {
		return billing.ErrGraceNotApplicable
	}
	return nil
}

// ClearGraceExtension ends a subscriber's grace extension; the ordinary
// ladder applies again from the next scan.
func (s *BillingStore) ClearGraceExtension(ctx context.Context, subscriberID int) error {
	if _, err := s.pool.Exec(ctx, `
		UPDATE subscribers SET grace_until = NULL, grace_reason = NULL, grace_set_by = NULL
		 WHERE id = $1`, subscriberID); err != nil {
		return fmt.Errorf("db: clear grace for subscriber %d: %w", subscriberID, err)
	}
	return nil
}
