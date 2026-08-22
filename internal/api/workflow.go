package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/inventory"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
)

// Task & approval workflow endpoints — FR-WFL-001..002 | MDS §4.15.
//
// The three sensitive actions MDS §4.14 built as immediate, single-operator
// actions (staff wallet credit, refund, termination) now create a pending
// approval_requests row and return 202 instead of executing. Nothing moves
// until a second, different staff member approves — which is the only way
// to honor CRD-EXP-002's "before taking effect" literally, rather than
// recording a sign-off after the money already moved.

// ApprovalQuerier is the persistence surface the approval gate needs.
// Satisfied by *db.WorkflowStore.
type ApprovalQuerier interface {
	CreateApprovalRequest(ctx context.Context, req workflow.ApprovalRequest) (*workflow.ApprovalRequest, error)
	GetApprovalRequest(ctx context.Context, id int) (*workflow.ApprovalRequest, error)
	ListApprovalRequests(ctx context.Context, status *workflow.Status, subscriberID *int) ([]workflow.ApprovalRequest, error)
	// ClaimApprovalRequest and RejectApprovalRequest must be atomic
	// conditional updates that only match a still-pending row, returning
	// (nil, nil) when the claim did not land — that is what stops two
	// concurrent decisions from both executing the underlying action.
	ClaimApprovalRequest(ctx context.Context, id int, decidedBy string) (*workflow.ApprovalRequest, error)
	RejectApprovalRequest(ctx context.Context, id int, decidedBy, reason string) (*workflow.ApprovalRequest, error)
	FinalizeApprovalExecution(ctx context.Context, id int, status workflow.Status, executionError *string, ledgerEntryID *int) error
}

// FieldTaskQuerier is the persistence surface for ad hoc task assignment
// (FR-WFL-002). Satisfied by *db.WorkflowStore.
type FieldTaskQuerier interface {
	CreateFieldTask(ctx context.Context, t workflow.FieldTask) (*workflow.FieldTask, error)
	GetFieldTask(ctx context.Context, id int) (*workflow.FieldTask, error)
	ListFieldTasks(ctx context.Context, assignedTo, status *string) ([]workflow.FieldTask, error)
	UpdateFieldTask(ctx context.Context, id int, status, assignedTo *string, dueDate *time.Time) (*workflow.FieldTask, error)
}

// approvalResponse is the JSON shape for an approval request. Amount is a
// fixed-2dp string (or omitted for a terminate request) so a JSON consumer
// never sees money as a float — the same rule SubscriberRecord.WalletBalance
// follows.
type approvalResponse struct {
	ID             int        `json:"id"`
	ActionType     string     `json:"action_type"`
	SubscriberID   int        `json:"subscriber_id"`
	Amount         string     `json:"amount,omitempty"`
	Reason         string     `json:"reason"`
	Status         string     `json:"status"`
	RequestedBy    string     `json:"requested_by"`
	DecidedBy      string     `json:"decided_by,omitempty"`
	DecisionReason string     `json:"decision_reason,omitempty"`
	ExecutionError string     `json:"execution_error,omitempty"`
	LedgerEntryID  *int       `json:"ledger_entry_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
}

func toApprovalResponse(r *workflow.ApprovalRequest) approvalResponse {
	resp := approvalResponse{
		ID: r.ID, ActionType: string(r.ActionType), SubscriberID: r.SubscriberID,
		Reason: r.Reason, Status: string(r.Status), RequestedBy: r.RequestedBy,
		LedgerEntryID: r.LedgerEntryID, CreatedAt: r.CreatedAt, DecidedAt: r.DecidedAt,
	}
	if r.Amount != nil {
		resp.Amount = r.Amount.StringFixed(2)
	}
	if r.DecidedBy != nil {
		resp.DecidedBy = *r.DecidedBy
	}
	if r.DecisionReason != nil {
		resp.DecisionReason = *r.DecisionReason
	}
	if r.ExecutionError != nil {
		resp.ExecutionError = *r.ExecutionError
	}
	return resp
}

// requestApproval creates the pending request the gated endpoints return
// instead of acting. Shared by all three so the 202 shape and the audit
// entry are written in exactly one place.
func (h *Handler) requestApproval(w http.ResponseWriter, r *http.Request, req workflow.ApprovalRequest) {
	req.RequestedBy = middleware.SubjectFromContext(r.Context())

	created, err := h.approvals.CreateApprovalRequest(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not create approval request")
		return
	}

	billing.LifecycleActionsTotal.WithLabelValues(string(req.ActionType) + "_requested").Inc()
	middleware.Audit(r.Context(), "approval.request", strconv.Itoa(created.ID), map[string]any{
		"action_type": string(req.ActionType), "subscriber_id": req.SubscriberID,
	})
	writeJSON(w, http.StatusAccepted, toApprovalResponse(created))
}

// ── FR-WFL-001: Decisions ───────────────────────────────────────────────────

// ApproveRequest handles POST /api/v1/approvals/{id}/approve.
//
// Claims the request atomically (so two racing approvers cannot both
// execute), then runs the underlying action and records its outcome.
func (h *Handler) ApproveRequest(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.approvals == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "approval store not configured")
		return
	}

	existing, err := h.approvals.GetApprovalRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "approval lookup failed")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "approval request not found")
		return
	}

	actor := middleware.SubjectFromContext(r.Context())
	if err := workflow.ValidateDecision(existing, actor); err != nil {
		// The requirement this module exists for: the person who asked for a
		// sensitive action can never be the one who signs it off. 403 rather
		// than 422 — this is an authorization boundary, not bad input.
		if errors.Is(err, workflow.ErrSelfApproval) {
			writeError(w, http.StatusForbidden, "ERR_SELF_APPROVAL", err.Error())
			return
		}
		writeError(w, http.StatusConflict, "ERR_ALREADY_DECIDED", err.Error())
		return
	}

	claimed, err := h.approvals.ClaimApprovalRequest(r.Context(), id, actor)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not claim approval request")
		return
	}
	if claimed == nil {
		// Lost the race: another decision claimed this request between the
		// read above and this conditional update.
		writeError(w, http.StatusConflict, "ERR_ALREADY_DECIDED", workflow.ErrAlreadyDecided.Error())
		return
	}

	executed := h.executeApprovedAction(r, claimed)

	middleware.Audit(r.Context(), "approval.approve", strconv.Itoa(id), map[string]any{
		"action_type": string(claimed.ActionType), "subscriber_id": claimed.SubscriberID,
		"requested_by": claimed.RequestedBy, "status": string(executed.Status),
	})
	writeJSON(w, http.StatusOK, toApprovalResponse(executed))
}

// executeApprovedAction runs the action a claimed request authorized and
// records the outcome, returning the request in its final state.
//
// A failure here is never propagated as an HTTP error: the approval itself
// succeeded and is durably recorded, so the honest answer to the caller is
// "your approval landed, and the action it authorized failed" — carried in
// the returned status/execution_error, not a 500 that would suggest the
// decision itself did not stick.
func (h *Handler) executeApprovedAction(r *http.Request, req *workflow.ApprovalRequest) *workflow.ApprovalRequest {
	ledgerEntryID, execErr := h.performGatedAction(r, req)

	finalStatus := workflow.StatusExecuted
	var execErrStr *string
	if execErr != nil {
		finalStatus = workflow.StatusExecutionFailed
		msg := execErr.Error()
		execErrStr = &msg
		workflow.ExecutionFailuresTotal.WithLabelValues(string(req.ActionType)).Inc()
		log.Error().Err(execErr).Int("approval_id", req.ID).
			Str("action_type", string(req.ActionType)).
			Msg("api: approved action failed to execute")
	} else {
		billing.LifecycleActionsTotal.WithLabelValues(string(req.ActionType) + "_approved").Inc()
	}

	if err := h.approvals.FinalizeApprovalExecution(r.Context(), req.ID, finalStatus, execErrStr, ledgerEntryID); err != nil {
		// The action itself already ran; failing to record that is a
		// reconciliation problem, not a reason to claim it did not happen.
		log.Error().Err(err).Int("approval_id", req.ID).Msg("api: could not record approval execution outcome")
	}

	req.Status = finalStatus
	req.ExecutionError = execErrStr
	req.LedgerEntryID = ledgerEntryID
	return req
}

// performGatedAction dispatches to the real implementation of whichever
// action was approved. These are the same store/service calls the ungated
// MDS §4.14 handlers make — the approval flow decides whether and when they
// run, it does not reimplement what they do.
// auditGatedAction records a completed approval-gated action against the
// subscriber it happened to.
//
// The approval flow already audits approval.request and approval.approve,
// but both target the *approval's* id, not the subscriber's. FR-LC-003
// (BO-007, CRD-REG-001) requires every lifecycle-affecting action - plan
// change, termination, adjustment, refund - to be traceable against the
// subscriber with staff attribution, and the first two of those already
// are: they execute inline and audit as subscriber.plan_change and
// subscriber.adjustment. Termination and refund do not, because they
// execute here instead, which left the exact question an auditor asks -
// "what was done to subscriber 42, and by whom" - answerable for two of
// the four actions and not the other two.
//
// Both parties are recorded. middleware.Audit takes actor_id from the
// request context, which during execution is the approver; the requester
// is the one whose judgment call it was, so it goes in the detail
// alongside the approval id that ties the two entries together.
func auditGatedAction(ctx context.Context, req *workflow.ApprovalRequest, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["approval_id"] = req.ID
	detail["requested_by"] = req.RequestedBy
	detail["reason"] = req.Reason
	middleware.Audit(ctx, "subscriber."+string(req.ActionType),
		strconv.Itoa(req.SubscriberID), detail)
}

func (h *Handler) performGatedAction(r *http.Request, req *workflow.ApprovalRequest) (*int, error) {
	ctx := r.Context()

	switch req.ActionType {
	case workflow.ActionWalletCredit, workflow.ActionRefund:
		if h.walletSvc == nil {
			return nil, errors.New("wallet service not configured")
		}
		if req.Amount == nil {
			// Unreachable through the API (chk_approval_amount_by_type also
			// forbids it at the schema), but a nil deref here would be a
			// panic in a money path, so it is checked rather than assumed.
			return nil, errors.New("approval request has no amount")
		}

		direction, description := "credit", "staff adjustment: "+req.Reason
		counterAccount := billing.AccountAdjustmentClearing
		if req.ActionType == workflow.ActionRefund {
			direction, description = "debit", "refund: "+req.Reason
		}
		// The ledger attributes the movement to the requester — whose
		// judgment call it fundamentally was — with the approver folded into
		// the description. approval_requests itself is the complete record
		// of both parties (MDS §4.15).
		decidedBy := ""
		if req.DecidedBy != nil {
			decidedBy = *req.DecidedBy
		}
		tx, err := h.walletSvc.Post(ctx, billing.PostRequest{
			SubscriberID:   req.SubscriberID,
			Amount:         *req.Amount,
			Direction:      direction,
			CounterAccount: counterAccount,
			AdjustedBy:     req.RequestedBy,
			Description:    description + " (approved by " + decidedBy + ")",
		})
		if err != nil {
			return nil, err
		}

		// Audited here, against the subscriber, the moment the money has
		// actually moved - see auditGatedAction on why the approval entry
		// alone does not satisfy FR-LC-003. Emitted before the refund
		// record below so that a CreateRefund failure, which leaves the
		// debit committed, still leaves the movement in the audit trail.
		auditGatedAction(ctx, req, map[string]any{
			"amount":     req.Amount.String(),
			"direction":  direction,
			"ledger_txn": tx.ID,
		})

		if req.ActionType == workflow.ActionRefund {
			if h.refunds == nil {
				return &tx.ID, errors.New("refund store not configured")
			}
			if _, err := h.refunds.CreateRefund(ctx, req.SubscriberID, tx.ID, *req.Amount, req.Reason, req.RequestedBy); err != nil {
				// The debit committed; report the failure but keep the
				// ledger id so the money movement stays traceable.
				return &tx.ID, err
			}
		}
		return &tx.ID, nil

	case workflow.ActionTerminate:
		if h.lifecycle == nil {
			return nil, errors.New("lifecycle store not configured")
		}
		updated, err := h.lifecycle.TerminateSubscriber(ctx, req.SubscriberID)
		if err != nil {
			return nil, err
		}
		if updated == nil {
			return nil, errors.New("subscriber not found")
		}
		if h.subCache != nil {
			if err := h.subCache.InvalidateSubscriber(ctx, updated.Username); err != nil {
				log.Error().Err(err).Int("subscriber_id", req.SubscriberID).
					Msg("api: auth-cache invalidation failed after approved termination")
			}
		}
		auditGatedAction(ctx, req, map[string]any{"username": updated.Username})

		enqueuePoDIfSessionActive(ctx, h, req.SubscriberID)
		h.openCPERecoveryTasks(ctx, req.SubscriberID, updated.Username)
		return nil, nil

	default:
		return nil, errors.New("unknown action type: " + string(req.ActionType))
	}
}

// openCPERecoveryTasks files a field task for each device a terminated
// subscriber still holds (MDS §4.16).
//
// The device deliberately stays 'issued' rather than being auto-returned to
// stock: flipping it would make the count FR-INV-003's reorder alerts are
// computed from claim hardware is on the shelf while it is still in a
// former customer's flat. Someone has to physically collect it, and that is
// what the task tracks.
//
// Best-effort throughout — a subscriber whose service has been terminated
// stays terminated whether or not the recovery paperwork could be filed.
func (h *Handler) openCPERecoveryTasks(ctx context.Context, subscriberID int, username string) {
	if h.inventory == nil || h.fieldTasks == nil {
		return
	}

	issued := inventory.StatusIssued
	devices, err := h.inventory.ListDevices(ctx, &issued, nil, &subscriberID)
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", subscriberID).
			Msg("api: could not list CPE for a terminated subscriber; recovery tasks not created")
		return
	}

	for _, d := range devices {
		sid := subscriberID
		if _, err := h.fieldTasks.CreateFieldTask(ctx, workflow.FieldTask{
			Title: "Recover CPE " + d.SerialNumber + " from " + username,
			Description: "Subscriber terminated. Collect " + d.DeviceType +
				" (serial " + d.SerialNumber + ") and mark it returned or faulty in inventory.",
			SubscriberID: &sid,
			AssignedTo:   cpeRecoveryQueue,
			CreatedBy:    middleware.SubjectFromContext(ctx),
		}); err != nil {
			log.Error().Err(err).Str("serial", d.SerialNumber).
				Msg("api: could not create a CPE recovery task")
		}
	}
}

// cpeRecoveryQueue is where unassigned recovery work lands. field_tasks
// assigns to a username rather than a role (DBD §6.2), and termination has
// no way to pick a specific technician — so it goes to a queue name a
// dispatcher reassigns from, rather than being guessed at here.
const cpeRecoveryQueue = "field_queue"

type rejectRequest struct {
	Reason string `json:"reason"`
}

// RejectRequest handles POST /api/v1/approvals/{id}/reject.
//
// Uses the same atomic claim as approve — a reject racing an approve must
// not let both land — and never executes the underlying action.
func (h *Handler) RejectRequest(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.approvals == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "approval store not configured")
		return
	}

	var body rejectRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	// Required on reject but not on approve: a refusal is the decision
	// somebody will later have to explain, and "why" is not recoverable
	// after the fact from anything else on the row.
	if body.Reason == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "reason is required to reject a request")
		return
	}

	existing, err := h.approvals.GetApprovalRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "approval lookup failed")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "approval request not found")
		return
	}

	actor := middleware.SubjectFromContext(r.Context())
	if err := workflow.ValidateDecision(existing, actor); err != nil {
		if errors.Is(err, workflow.ErrSelfApproval) {
			writeError(w, http.StatusForbidden, "ERR_SELF_APPROVAL", err.Error())
			return
		}
		writeError(w, http.StatusConflict, "ERR_ALREADY_DECIDED", err.Error())
		return
	}

	rejected, err := h.approvals.RejectApprovalRequest(r.Context(), id, actor, body.Reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not reject approval request")
		return
	}
	if rejected == nil {
		writeError(w, http.StatusConflict, "ERR_ALREADY_DECIDED", workflow.ErrAlreadyDecided.Error())
		return
	}

	middleware.Audit(r.Context(), "approval.reject", strconv.Itoa(id), map[string]any{
		"action_type": string(rejected.ActionType), "requested_by": rejected.RequestedBy, "reason": body.Reason,
	})
	writeJSON(w, http.StatusOK, toApprovalResponse(rejected))
}

// ListApprovals handles GET /api/v1/approvals?status=&subscriber_id=.
func (h *Handler) ListApprovals(w http.ResponseWriter, r *http.Request) {
	if h.approvals == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "approval store not configured")
		return
	}

	var statusFilter *workflow.Status
	if v := r.URL.Query().Get("status"); v != "" {
		s := workflow.Status(v)
		statusFilter = &s
	}
	var subscriberFilter *int
	if v := r.URL.Query().Get("subscriber_id"); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_id must be an integer")
			return
		}
		subscriberFilter = &id
	}

	list, err := h.approvals.ListApprovalRequests(r.Context(), statusFilter, subscriberFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list approvals failed")
		return
	}

	out := make([]approvalResponse, 0, len(list))
	for i := range list {
		out = append(out, toApprovalResponse(&list[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// GetApproval handles GET /api/v1/approvals/{id}.
func (h *Handler) GetApproval(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.approvals == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "approval store not configured")
		return
	}

	req, err := h.approvals.GetApprovalRequest(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "approval lookup failed")
		return
	}
	if req == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "approval request not found")
		return
	}
	writeJSON(w, http.StatusOK, toApprovalResponse(req))
}

// ── FR-WFL-002: Field tasks ─────────────────────────────────────────────────

type fieldTaskCreateRequest struct {
	Title        string  `json:"title"`
	Description  string  `json:"description"`
	SubscriberID *int    `json:"subscriber_id"`
	FranchiseID  *int    `json:"franchise_id"`
	AssignedTo   string  `json:"assigned_to"`
	DueDate      *string `json:"due_date"` // YYYY-MM-DD
}

// CreateFieldTask handles POST /api/v1/field-tasks.
func (h *Handler) CreateFieldTask(w http.ResponseWriter, r *http.Request) {
	if h.fieldTasks == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "field task store not configured")
		return
	}

	var req fieldTaskCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Title == "" || req.AssignedTo == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "title and assigned_to are required")
		return
	}

	dueDate, err := parseOptionalDate(req.DueDate)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	created, err := h.fieldTasks.CreateFieldTask(r.Context(), workflow.FieldTask{
		Title: req.Title, Description: req.Description,
		SubscriberID: req.SubscriberID, FranchiseID: req.FranchiseID,
		AssignedTo: req.AssignedTo, CreatedBy: middleware.SubjectFromContext(r.Context()),
		DueDate: dueDate,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create field task failed")
		return
	}

	middleware.Audit(r.Context(), "field_task.create", strconv.Itoa(created.ID), map[string]any{
		"assigned_to": created.AssignedTo, "title": created.Title,
	})
	writeJSON(w, http.StatusCreated, created)
}

// ListFieldTasks handles GET /api/v1/field-tasks?assigned_to=&status=.
func (h *Handler) ListFieldTasks(w http.ResponseWriter, r *http.Request) {
	if h.fieldTasks == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "field task store not configured")
		return
	}

	var assignedTo, status *string
	if v := r.URL.Query().Get("assigned_to"); v != "" {
		assignedTo = &v
	}
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}

	list, err := h.fieldTasks.ListFieldTasks(r.Context(), assignedTo, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list field tasks failed")
		return
	}
	if list == nil {
		list = []workflow.FieldTask{}
	}
	writeJSON(w, http.StatusOK, list)
}

type fieldTaskUpdateRequest struct {
	Status     *string `json:"status"`
	AssignedTo *string `json:"assigned_to"`
	DueDate    *string `json:"due_date"`
}

// UpdateFieldTask handles PATCH /api/v1/field-tasks/{id}.
func (h *Handler) UpdateFieldTask(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.fieldTasks == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "field task store not configured")
		return
	}

	var req fieldTaskUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	// Validated here rather than left to the CHECK constraint: an unknown
	// status would otherwise surface as a raw constraint-violation 500
	// instead of telling the caller what the legal values are.
	if req.Status != nil && !validFieldTaskStatus(*req.Status) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"status must be one of open, in_progress, completed, cancelled")
		return
	}

	dueDate, err := parseOptionalDate(req.DueDate)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}

	updated, err := h.fieldTasks.UpdateFieldTask(r.Context(), id, req.Status, req.AssignedTo, dueDate)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "update field task failed")
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "field task not found")
		return
	}

	middleware.Audit(r.Context(), "field_task.update", strconv.Itoa(id), map[string]any{
		"status": req.Status, "assigned_to": req.AssignedTo,
	})
	writeJSON(w, http.StatusOK, updated)
}

func validFieldTaskStatus(s string) bool {
	switch s {
	case workflow.TaskOpen, workflow.TaskInProgress, workflow.TaskCompleted, workflow.TaskCancelled:
		return true
	default:
		return false
	}
}

// parseOptionalDate parses a nil-able YYYY-MM-DD field. A malformed date is
// rejected rather than silently dropped — a due date that quietly vanished
// is worse than one the caller was told to fix.
func parseOptionalDate(s *string) (*time.Time, error) {
	if s == nil || *s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", *s)
	if err != nil {
		return nil, errors.New("due_date must be a YYYY-MM-DD date")
	}
	return &t, nil
}

// amountPtr is a small helper the gated handlers use to attach a validated
// amount to an approval request.
func amountPtr(d decimal.Decimal) *decimal.Decimal { return &d }
