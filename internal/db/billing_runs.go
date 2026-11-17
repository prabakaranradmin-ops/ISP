package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Recurring-billing run history — FR-BIL-020 | migration 057.

// maxRunMessage bounds a stored error. Errors are wrapped chains that can
// repeat a long SQL error several times; the start of one says what failed.
const maxRunMessage = 500

func clip(s string) string {
	if r := []rune(s); len(r) > maxRunMessage {
		return string(r[:maxRunMessage-1]) + "…"
	}
	return s
}

// StartRun opens a run. A run of the same kind still "running" an hour
// after it began belongs to a process that stopped mid-pass; it is closed as
// abandoned here, since nothing else will ever finish it.
func (s *BillingStore) StartRun(ctx context.Context, kind string) (int, error) {
	var id int
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE billing_runs SET status = 'abandoned', finished_at = now()
			 WHERE kind = $1 AND status = 'running' AND started_at < now() - interval '1 hour'`, kind); err != nil {
			return fmt.Errorf("db: close abandoned runs: %w", err)
		}
		return tx.QueryRow(ctx, `INSERT INTO billing_runs (kind) VALUES ($1) RETURNING id`, kind).Scan(&id)
	})
	if err != nil {
		return 0, fmt.Errorf("db: start billing run: %w", err)
	}
	return id, nil
}

// FinishRun closes a run with what it did, and its failures, together.
func (s *BillingStore) FinishRun(ctx context.Context, runID int, status string, sum billing.RunSummary) error {
	var errText *string
	if sum.Error != "" {
		e := clip(sum.Error)
		errText = &e
	}
	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE billing_runs
			   SET status = $2, finished_at = now(), candidates = $3, renewed = $4, activated = $5,
			       insufficient_balance = $6, failed = $7, invoices_raised = $8, invoice_failures = $9,
			       amount_charged = $10::numeric, error = $11
			 WHERE id = $1 AND status = 'running'`,
			runID, status, sum.Candidates, sum.Renewed, sum.Activated, sum.InsufficientBalance,
			sum.Failed, sum.InvoicesRaised, sum.InvoiceFailures, sum.AmountCharged.StringFixed(2), errText)
		if err != nil {
			return fmt.Errorf("db: finish billing run %d: %w", runID, err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("db: finish billing run %d: not running", runID)
		}
		for _, f := range sum.Failures {
			if _, err := tx.Exec(ctx, `
				INSERT INTO billing_run_failures (run_id, subscriber_id, username, stage, message, charged)
				VALUES ($1, (SELECT id FROM subscribers WHERE id = $2), $3, $4, $5, $6)`,
				runID, f.SubscriberID, f.Username, f.Stage, clip(f.Message), f.Charged); err != nil {
				return fmt.Errorf("db: record run %d failure: %w", runID, err)
			}
		}
		return nil
	})
}

const billingRunColumns = `
	id, kind, started_at, finished_at, status, candidates, renewed, activated,
	insufficient_balance, failed, invoices_raised, invoice_failures,
	amount_charged::text, COALESCE(error, '')`

func scanBillingRun(row pgx.Row) (*billing.BillingRun, error) {
	var (
		r       billing.BillingRun
		charged string
	)
	if err := row.Scan(&r.ID, &r.Kind, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Candidates, &r.Renewed,
		&r.Activated, &r.InsufficientBalance, &r.Failed, &r.InvoicesRaised, &r.InvoiceFailures,
		&charged, &r.Error); err != nil {
		return nil, err
	}
	v, err := parseDecimal(charged)
	if err != nil {
		return nil, err
	}
	r.AmountCharged = v
	return &r, nil
}

// ListBillingRuns lists recurring-billing runs, newest first. Idle passes
// (nobody to renew, nothing wrong) are left out unless asked for.
func (s *BillingStore) ListBillingRuns(ctx context.Context, f billing.BillingRunFilter) ([]billing.BillingRun, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+billingRunColumns+`
		  FROM billing_runs
		 WHERE kind = $1
		   AND ($2 OR candidates > 0 OR status <> 'completed')
		   AND (NOT $3 OR status IN ('failed', 'abandoned') OR failed > 0 OR invoice_failures > 0
		        OR EXISTS (SELECT 1 FROM billing_run_failures x WHERE x.run_id = billing_runs.id))
		 ORDER BY started_at DESC, id DESC
		 LIMIT $4`, billing.RunKindRecurringBilling, f.IncludeIdle, f.ProblemsOnly, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("db: list billing runs: %w", err)
	}
	defer rows.Close()
	out := []billing.BillingRun{}
	for rows.Next() {
		r, err := scanBillingRun(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan billing run: %w", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetBillingRun loads one run with its failures; nil if there is none.
func (s *BillingStore) GetBillingRun(ctx context.Context, id int) (*billing.BillingRun, error) {
	r, err := scanBillingRun(s.pool.QueryRow(ctx, `SELECT `+billingRunColumns+` FROM billing_runs WHERE id = $1`, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get billing run %d: %w", id, err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(subscriber_id, 0), username, stage, message, charged
		  FROM billing_run_failures WHERE run_id = $1
		 ORDER BY charged DESC, id`, id)
	if err != nil {
		return nil, fmt.Errorf("db: billing run %d failures: %w", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var f billing.RunFailure
		if err := rows.Scan(&f.SubscriberID, &f.Username, &f.Stage, &f.Message, &f.Charged); err != nil {
			return nil, fmt.Errorf("db: scan run failure: %w", err)
		}
		r.Failures = append(r.Failures, f)
	}
	return r, rows.Err()
}
