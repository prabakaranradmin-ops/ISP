// Package workflow implements second-approver sign-off for sensitive
// account actions and ad hoc field-task assignment.
//
// FR: FR-WFL-001..002 | MDS §4.15 | DBD §6.2 approval_requests, field_tasks
package workflow

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/shopspring/decimal"
)

// ExecutionFailuresTotal counts approved requests whose underlying action
// failed to execute — e.g. the wallet balance moved between request and
// decision. This is the one case in the whole gated-action flow where an
// operator must look, not just reconcile later: the request cleared its
// second-approver check and still did not happen.
var ExecutionFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "workflow_approval_execution_failures_total",
	Help: "Approved requests whose underlying action failed to execute",
}, []string{"action_type"})

// ActionType identifies which sensitive action an approval request gates.
type ActionType string

const (
	ActionWalletCredit ActionType = "wallet_credit"
	ActionRefund       ActionType = "refund"
	ActionTerminate    ActionType = "terminate"
	// ActionCreditNote issues a GST credit note against an invoice and
	// returns its value to the wallet (FR-BIL-012, migration 053). Amount is
	// the taxable value to credit; InvoiceID names the invoice.
	ActionCreditNote ActionType = "credit_note"
)

// Status is an approval request's lifecycle stage.
//
// StatusApproved is a real, persisted intermediate state — the atomic claim
// a decision makes before the underlying action has actually run — not a
// value the app skips through on the way to StatusExecuted. A crash between
// the claim and execution completing leaves a request visibly stuck at
// approved, which is the honest state to be in, rather than one that looks
// untouched (risking a second execution on retry) or one that silently
// discards the approver's decision.
type Status string

const (
	StatusPending         Status = "pending"
	StatusApproved        Status = "approved"
	StatusRejected        Status = "rejected"
	StatusExecuted        Status = "executed"
	StatusExecutionFailed Status = "execution_failed"
)

// ApprovalRequest is a sensitive account action pending, or having received,
// a second approver's sign-off.
type ApprovalRequest struct {
	ID             int
	ActionType     ActionType
	SubscriberID   int
	Amount         *decimal.Decimal // nil for ActionTerminate
	Reason         string
	Status         Status
	RequestedBy    string
	DecidedBy      *string
	DecisionReason *string
	ExecutionError *string
	LedgerEntryID  *int
	// InvoiceID is set only for ActionCreditNote (chk_approval_invoice_by_type).
	InvoiceID *int
	CreatedAt time.Time
	DecidedAt *time.Time
}

var (
	// ErrSelfApproval is returned when the staff member deciding a request
	// is the same one who created it — the guarantee this whole module
	// exists to enforce. Also enforced at the schema level
	// (chk_approval_distinct_approver), so this is a clean 403 for the
	// normal case, not the only line of defense.
	ErrSelfApproval = errors.New("workflow: an approval request cannot be decided by the staff member who requested it")
	// ErrAlreadyDecided is returned for a decision attempt on a request that
	// is no longer pending — already approved, rejected, or claimed by a
	// racing decision.
	ErrAlreadyDecided = errors.New("workflow: this approval request has already been decided")
)

// ValidateDecision is the one place both the approve and reject handlers
// check before attempting the database's atomic claim, so the self-approval
// and already-decided rules exist in exactly one function rather than two
// handlers that could drift.
func ValidateDecision(req *ApprovalRequest, actor string) error {
	if req.Status != StatusPending {
		return ErrAlreadyDecided
	}
	if actor == req.RequestedBy {
		return ErrSelfApproval
	}
	return nil
}

// Field-task status values (FR-WFL-002).
const (
	TaskOpen       = "open"
	TaskInProgress = "in_progress"
	TaskCompleted  = "completed"
	TaskCancelled  = "cancelled"
)

// FieldTask is an ad hoc, subscriber-optional unit of staff work, tracked
// independently of the ticket system — CRD-EXP-002's own wording, reflected
// here as a flat assign/track/complete record with no SLA engine or
// routing rules of its own.
// Serialized straight to the API response, so the json tags are load-bearing
// rather than decorative: every other endpoint in this codebase returns
// snake_case, and a client would have no way to know this one route
// alone answered in Go field names.
type FieldTask struct {
	ID           int        `json:"id"`
	Title        string     `json:"title"`
	Description  string     `json:"description,omitempty"`
	SubscriberID *int       `json:"subscriber_id,omitempty"`
	FranchiseID  *int       `json:"franchise_id,omitempty"`
	AssignedTo   string     `json:"assigned_to"`
	CreatedBy    string     `json:"created_by"`
	Status       string     `json:"status"`
	DueDate      *time.Time `json:"due_date,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}
