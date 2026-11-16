package db

import (
	"context"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
)

// WorkflowStore serves approval-request and field-task persistence.
// Satisfies api.ApprovalQuerier and api.FieldTaskQuerier.
type WorkflowStore struct{ pool dbPool }

var (
	_ api.ApprovalQuerier  = (*WorkflowStore)(nil)
	_ api.FieldTaskQuerier = (*WorkflowStore)(nil)
)

// ── Approval requests ────────────────────────────────────────────────────────

const approvalColumns = `
	id, action_type, subscriber_id, amount::text, reason, status,
	requested_by_username, decided_by_username, decision_reason,
	execution_error, ledger_entry_id, created_at, decided_at, invoice_id`

// scanApproval reads one approval_requests row. action_type and status are
// scanned through plain string locals rather than directly into the named
// ActionType/Status fields — the same caution ListDunningCandidates already
// applies to billing.DunningState — before being cast.
func scanApproval(row interface{ Scan(dest ...any) error }) (*workflow.ApprovalRequest, error) {
	var (
		req                    workflow.ApprovalRequest
		actionType, status     string
		amountStr, executionEr *string
	)
	err := row.Scan(
		&req.ID, &actionType, &req.SubscriberID, &amountStr, &req.Reason, &status,
		&req.RequestedBy, &req.DecidedBy, &req.DecisionReason,
		&executionEr, &req.LedgerEntryID, &req.CreatedAt, &req.DecidedAt, &req.InvoiceID,
	)
	if err != nil {
		return nil, err
	}
	req.ActionType = workflow.ActionType(actionType)
	req.Status = workflow.Status(status)
	req.ExecutionError = executionEr
	if amountStr != nil {
		amt, err := parseDecimal(*amountStr)
		if err != nil {
			return nil, err
		}
		req.Amount = &amt
	}
	return &req, nil
}

// CreateApprovalRequest persists a new pending request. Nothing about the
// underlying action happens here — this is only ever the "someone asked"
// half of the workflow (MDS §4.15).
func (s *WorkflowStore) CreateApprovalRequest(ctx context.Context, req workflow.ApprovalRequest) (*workflow.ApprovalRequest, error) {
	const q = `
		WITH ins AS (
			INSERT INTO approval_requests (action_type, subscriber_id, amount, reason, requested_by_username, invoice_id)
			VALUES ($1, $2, $3::numeric, $4, $5, $6)
			RETURNING *
		)
		SELECT ` + approvalColumns + ` FROM ins`

	var amountParam *string
	if req.Amount != nil {
		v := req.Amount.String()
		amountParam = &v
	}

	created, err := scanApproval(s.pool.QueryRow(ctx, q,
		string(req.ActionType), req.SubscriberID, amountParam, req.Reason, req.RequestedBy, req.InvoiceID))
	if err != nil {
		return nil, fmt.Errorf("db: create approval request for subscriber %d: %w", req.SubscriberID, err)
	}
	return created, nil
}

// GetApprovalRequest loads one request. A missing row returns (nil, nil).
func (s *WorkflowStore) GetApprovalRequest(ctx context.Context, id int) (*workflow.ApprovalRequest, error) {
	const q = `SELECT ` + approvalColumns + ` FROM approval_requests WHERE id = $1`
	req, err := scanApproval(s.pool.QueryRow(ctx, q, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get approval request %d: %w", id, err)
	}
	return req, nil
}

// ListApprovalRequests lists requests, optionally filtered by status and/or
// subscriber. Both filters are optional so the same method serves "what's
// waiting on me" (status=pending) and a full history view.
func (s *WorkflowStore) ListApprovalRequests(ctx context.Context, status *workflow.Status, subscriberID *int) ([]workflow.ApprovalRequest, error) {
	const q = `
		SELECT ` + approvalColumns + `
		FROM approval_requests
		WHERE ($1::text IS NULL OR status = $1)
		  AND ($2::int IS NULL OR subscriber_id = $2)
		ORDER BY created_at DESC`

	var statusParam *string
	if status != nil {
		v := string(*status)
		statusParam = &v
	}

	rows, err := s.pool.Query(ctx, q, statusParam, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: list approval requests: %w", err)
	}
	defer rows.Close()

	var out []workflow.ApprovalRequest
	for rows.Next() {
		req, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan approval request: %w", err)
		}
		out = append(out, *req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate approval requests: %w", err)
	}
	return out, nil
}

// ClaimApprovalRequest atomically transitions a pending request to
// 'approved', attributing the decision to decidedBy. This is what makes two
// concurrent decisions on the same request resolve to exactly one winner:
// the UPDATE only matches a row still at status='pending', so a second,
// racing call sees zero rows affected. Returns (nil, nil) — not an error —
// when the claim did not land, so the caller can tell "already decided"
// apart from a real failure.
func (s *WorkflowStore) ClaimApprovalRequest(ctx context.Context, id int, decidedBy string) (*workflow.ApprovalRequest, error) {
	const q = `
		WITH upd AS (
			UPDATE approval_requests
			SET status = 'approved', decided_by_username = $2, decided_at = NOW()
			WHERE id = $1 AND status = 'pending'
			RETURNING *
		)
		SELECT ` + approvalColumns + ` FROM upd`

	req, err := scanApproval(s.pool.QueryRow(ctx, q, id, decidedBy))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: claim approval request %d: %w", id, err)
	}
	return req, nil
}

// RejectApprovalRequest atomically transitions a pending request straight to
// 'rejected', using the same conditional claim ClaimApprovalRequest does —
// a reject racing an approve must not let both land.
func (s *WorkflowStore) RejectApprovalRequest(ctx context.Context, id int, decidedBy, reason string) (*workflow.ApprovalRequest, error) {
	const q = `
		WITH upd AS (
			UPDATE approval_requests
			SET status = 'rejected', decided_by_username = $2, decision_reason = $3, decided_at = NOW()
			WHERE id = $1 AND status = 'pending'
			RETURNING *
		)
		SELECT ` + approvalColumns + ` FROM upd`

	req, err := scanApproval(s.pool.QueryRow(ctx, q, id, decidedBy, reason))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: reject approval request %d: %w", id, err)
	}
	return req, nil
}

// FinalizeApprovalExecution records the outcome of actually running the
// action a claimed (status='approved') request authorized — 'executed' on
// success, 'execution_failed' (with the error) if the underlying action
// itself failed after approval, e.g. the wallet balance moved between
// request and decision.
func (s *WorkflowStore) FinalizeApprovalExecution(ctx context.Context, id int, status workflow.Status, executionError *string, ledgerEntryID *int) error {
	const q = `UPDATE approval_requests SET status = $2, execution_error = $3, ledger_entry_id = $4 WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, id, string(status), executionError, ledgerEntryID)
	if err != nil {
		return fmt.Errorf("db: finalize approval execution %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: approval request %d: %w", id, ErrNotFound)
	}
	return nil
}

// ── Field tasks ──────────────────────────────────────────────────────────────

const fieldTaskColumns = `
	id, title, COALESCE(description, ''), subscriber_id, franchise_id,
	assigned_to_username, created_by_username, status, due_date,
	created_at, updated_at, completed_at`

func scanFieldTask(row interface{ Scan(dest ...any) error }) (*workflow.FieldTask, error) {
	var t workflow.FieldTask
	err := row.Scan(
		&t.ID, &t.Title, &t.Description, &t.SubscriberID, &t.FranchiseID,
		&t.AssignedTo, &t.CreatedBy, &t.Status, &t.DueDate,
		&t.CreatedAt, &t.UpdatedAt, &t.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateFieldTask persists a new ad hoc task assignment.
func (s *WorkflowStore) CreateFieldTask(ctx context.Context, t workflow.FieldTask) (*workflow.FieldTask, error) {
	const q = `
		WITH ins AS (
			INSERT INTO field_tasks (
				title, description, subscriber_id, franchise_id,
				assigned_to_username, created_by_username, due_date
			) VALUES ($1, NULLIF($2,''), $3, $4, $5, $6, $7)
			RETURNING *
		)
		SELECT ` + fieldTaskColumns + ` FROM ins`

	created, err := scanFieldTask(s.pool.QueryRow(ctx, q,
		t.Title, t.Description, t.SubscriberID, t.FranchiseID, t.AssignedTo, t.CreatedBy, t.DueDate))
	if err != nil {
		return nil, fmt.Errorf("db: create field task: %w", err)
	}
	return created, nil
}

// GetFieldTask loads one task. A missing row returns (nil, nil).
func (s *WorkflowStore) GetFieldTask(ctx context.Context, id int) (*workflow.FieldTask, error) {
	const q = `SELECT ` + fieldTaskColumns + ` FROM field_tasks WHERE id = $1`
	t, err := scanFieldTask(s.pool.QueryRow(ctx, q, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get field task %d: %w", id, err)
	}
	return t, nil
}

// ListFieldTasks lists tasks, optionally filtered by assignee and/or status.
func (s *WorkflowStore) ListFieldTasks(ctx context.Context, assignedTo, status *string) ([]workflow.FieldTask, error) {
	const q = `
		SELECT ` + fieldTaskColumns + `
		FROM field_tasks
		WHERE ($1::text IS NULL OR assigned_to_username = $1)
		  AND ($2::text IS NULL OR status = $2)
		ORDER BY due_date NULLS LAST, created_at`

	rows, err := s.pool.Query(ctx, q, assignedTo, status)
	if err != nil {
		return nil, fmt.Errorf("db: list field tasks: %w", err)
	}
	defer rows.Close()

	var out []workflow.FieldTask
	for rows.Next() {
		t, err := scanFieldTask(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan field task: %w", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate field tasks: %w", err)
	}
	return out, nil
}

// UpdateFieldTask applies a partial update. A nil field is left untouched.
// completed_at moves with status: set to NOW() on a transition to
// 'completed', cleared on a transition to anything else, left alone when
// status is not part of this update.
func (s *WorkflowStore) UpdateFieldTask(ctx context.Context, id int, status, assignedTo *string, dueDate *time.Time) (*workflow.FieldTask, error) {
	const q = `
		WITH upd AS (
			UPDATE field_tasks
			SET status               = COALESCE($2, status),
			    assigned_to_username = COALESCE($3, assigned_to_username),
			    due_date             = COALESCE($4, due_date),
			    completed_at = CASE
			        WHEN $2 = 'completed' THEN NOW()
			        WHEN $2 IS NOT NULL THEN NULL
			        ELSE completed_at
			    END
			WHERE id = $1
			RETURNING *
		)
		SELECT ` + fieldTaskColumns + ` FROM upd`

	t, err := scanFieldTask(s.pool.QueryRow(ctx, q, id, status, assignedTo, dueDate))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: update field task %d: %w", id, err)
	}
	return t, nil
}
