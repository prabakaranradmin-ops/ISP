package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/rs/zerolog/log"
)

// FieldTaskStore backs the field-task half of the Tasks screen: ad hoc
// staff work assignment, independent of the ticket system. The console
// counterpart to internal/api/workflow.go's field-task endpoints, which
// have been routed and tested since CRD-EXP-002 with no console screen.
//
// Redefined per package rather than sharing api.FieldTaskQuerier, matching
// every other store interface in this file. Satisfied by *db.WorkflowStore.
type FieldTaskStore interface {
	CreateFieldTask(ctx context.Context, t workflow.FieldTask) (*workflow.FieldTask, error)
	ListFieldTasks(ctx context.Context, assignedTo, status *string) ([]workflow.FieldTask, error)
	UpdateFieldTask(ctx context.Context, id int, status, assignedTo *string, dueDate *time.Time) (*workflow.FieldTask, error)
}

// ApprovalStore backs the read/reject half of the approvals panel: listing
// pending requests, and rejecting one (a pure store write with no side
// effect beyond the row itself). Same *db.WorkflowStore instance as
// FieldTaskStore above.
type ApprovalStore interface {
	ListApprovalRequests(ctx context.Context, status *workflow.Status, subscriberID *int) ([]workflow.ApprovalRequest, error)
	GetApprovalRequest(ctx context.Context, id int) (*workflow.ApprovalRequest, error)
	RejectApprovalRequest(ctx context.Context, id int, decidedBy, reason string) (*workflow.ApprovalRequest, error)
}

// ApprovalExecutor claims and executes an approved request — the wallet-
// credit/refund/terminate logic that lives in internal/api, behind stores
// staffui does not hold. Satisfied by *api.Handler's ExecuteApprovedRequest,
// the same instance already wired as SubscriberCreator/BulkActions: the
// console calls straight into it rather than reimplementing execution here.
type ApprovalExecutor interface {
	ExecuteApprovedRequest(ctx context.Context, id int, actor string) (*workflow.ApprovalRequest, error)
}

type tasksData struct {
	FieldTasks []workflow.FieldTask
	Approvals  []workflow.ApprovalRequest
	// ShowFieldTaskActions gates creating/updating field tasks to
	// csr/technician/isp_owner, matching internal/api/routes.go's csrOrTech
	// gate exactly — everyone who can reach this screen can see the queue
	// (matches staffRead), not everyone can dispatch it.
	ShowFieldTaskActions bool
	// ShowApprovals gates the whole approvals panel to billing_admin/
	// isp_owner, matching the API's own admin gate on every /approvals
	// route. A csr or technician cannot decide these regardless of who
	// requested them.
	ShowApprovals bool
}

// Tasks shows the field-task queue and, for owner/billing, the pending
// approval queue. Available to all five staff roles (field-task visibility
// is staff-wide); the two action panels within it are gated separately.
func (h *Handler) Tasks(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "tasks")
	if !ok {
		return
	}
	h.renderTasks(w, r, s, "", "")
}

func (h *Handler) renderTasks(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	d := h.page(s, "Tasks", "tasks")
	d.Message, d.Error = message, errMsg

	td := tasksData{
		ShowFieldTaskActions: s.Role == "csr" || s.Role == "technician" || s.Role == "isp_owner",
		ShowApprovals:        s.Role == "billing_admin" || s.Role == "isp_owner",
	}

	if h.fieldTasks != nil {
		tasks, err := h.fieldTasks.ListFieldTasks(r.Context(), nil, nil)
		if err != nil {
			log.Error().Err(err).Msg("staffui: list field tasks failed")
			d.Error = "Could not load field tasks."
		} else {
			td.FieldTasks = tasks
		}
	}

	if td.ShowApprovals && h.approvals != nil {
		pending := workflow.StatusPending
		approvals, err := h.approvals.ListApprovalRequests(r.Context(), &pending, nil)
		if err != nil {
			log.Error().Err(err).Msg("staffui: list approvals failed")
			if d.Error == "" {
				d.Error = "Could not load pending approvals."
			}
		} else {
			td.Approvals = approvals
		}
	}

	d.Data = td
	h.render(w, "tasks", d)
}

// CreateFieldTaskForm files a new ad hoc task.
func (h *Handler) CreateFieldTaskForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "tasks")
	if !ok {
		return
	}
	if s.Role != "csr" && s.Role != "technician" && s.Role != "isp_owner" {
		h.renderError(w, r, s, http.StatusForbidden, "Only CSR, technician or owner can assign field tasks.")
		return
	}
	if h.fieldTasks == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Task management is not configured.")
		return
	}

	title := strings.TrimSpace(r.PostFormValue("title"))
	assignedTo := strings.TrimSpace(r.PostFormValue("assigned_to"))
	if title == "" || assignedTo == "" {
		h.renderTasks(w, r, s, "", "Title and assignee are required.")
		return
	}
	description := strings.TrimSpace(r.PostFormValue("description"))

	var subscriberID *int
	if v := strings.TrimSpace(r.PostFormValue("subscriber_id")); v != "" {
		id, err := strconv.Atoi(v)
		if err != nil {
			h.renderTasks(w, r, s, "", "Subscriber id must be a number.")
			return
		}
		subscriberID = &id
	}

	var dueDate *time.Time
	if v := strings.TrimSpace(r.PostFormValue("due_date")); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			h.renderTasks(w, r, s, "", "Due date must be a date like 2026-08-31.")
			return
		}
		dueDate = &t
	}

	created, err := h.fieldTasks.CreateFieldTask(r.Context(), workflow.FieldTask{
		Title: title, Description: description, SubscriberID: subscriberID,
		AssignedTo: assignedTo, CreatedBy: s.Username, DueDate: dueDate,
	})
	if err != nil {
		log.Error().Err(err).Msg("staffui: create field task failed")
		h.renderTasks(w, r, s, "", "Could not create that task.")
		return
	}
	h.renderTasks(w, r, s, fmt.Sprintf("Task %q assigned to %s.", created.Title, created.AssignedTo), "")
}

// UpdateFieldTaskForm changes a task's status and/or reassigns it.
func (h *Handler) UpdateFieldTaskForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "tasks")
	if !ok {
		return
	}
	if s.Role != "csr" && s.Role != "technician" && s.Role != "isp_owner" {
		h.renderError(w, r, s, http.StatusForbidden, "Only CSR, technician or owner can update field tasks.")
		return
	}
	if h.fieldTasks == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Task management is not configured.")
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderTasks(w, r, s, "", "Invalid task id.")
		return
	}

	var status *string
	if v := strings.TrimSpace(r.PostFormValue("status")); v != "" {
		status = &v
	}
	var assignedTo *string
	if v := strings.TrimSpace(r.PostFormValue("assigned_to")); v != "" {
		assignedTo = &v
	}

	updated, err := h.fieldTasks.UpdateFieldTask(r.Context(), id, status, assignedTo, nil)
	if err != nil {
		log.Error().Err(err).Int("id", id).Msg("staffui: update field task failed")
		h.renderTasks(w, r, s, "", "Could not update that task.")
		return
	}
	if updated == nil {
		h.renderTasks(w, r, s, "", "No task with that id.")
		return
	}
	h.renderTasks(w, r, s, fmt.Sprintf("Task #%d updated.", updated.ID), "")
}

// ApproveTaskRequest signs off a pending sensitive action. Mirrors
// internal/api/workflow.go's ApproveRequest exactly: the same existence and
// workflow.ValidateDecision pre-checks here, then delegating the claim and
// the wallet-credit/refund/terminate execution itself to
// h.approvalExecutor.ExecuteApprovedRequest — the exact method the HTTP
// endpoint calls, so there is one execution path, not two.
func (h *Handler) ApproveTaskRequest(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "tasks")
	if !ok {
		return
	}
	if s.Role != "billing_admin" && s.Role != "isp_owner" {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing admin or owner can decide approval requests.")
		return
	}
	if h.approvals == nil || h.approvalExecutor == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Approvals are not configured.")
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderTasks(w, r, s, "", "Invalid approval id.")
		return
	}

	existing, err := h.approvals.GetApprovalRequest(r.Context(), id)
	if err != nil || existing == nil {
		h.renderTasks(w, r, s, "", "Approval request not found.")
		return
	}
	if err := workflow.ValidateDecision(existing, s.Username); err != nil {
		h.renderTasks(w, r, s, "", "Could not approve: "+err.Error())
		return
	}

	executed, err := h.approvalExecutor.ExecuteApprovedRequest(r.Context(), id, s.Username)
	if err != nil {
		h.renderTasks(w, r, s, "", "Could not approve that request — it may already be decided.")
		return
	}
	if executed.Status == workflow.StatusExecutionFailed {
		h.renderTasks(w, r, s, "", fmt.Sprintf(
			"Approval #%d was recorded, but the action itself failed: %s", id, *executed.ExecutionError))
		return
	}
	h.renderTasks(w, r, s, fmt.Sprintf("Approval #%d approved and executed.", id), "")
}

// RejectTaskRequest declines a pending sensitive action.
func (h *Handler) RejectTaskRequest(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "tasks")
	if !ok {
		return
	}
	if s.Role != "billing_admin" && s.Role != "isp_owner" {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing admin or owner can decide approval requests.")
		return
	}
	if h.approvals == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Approvals are not configured.")
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderTasks(w, r, s, "", "Invalid approval id.")
		return
	}
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		h.renderTasks(w, r, s, "", "A reason is required to reject a request.")
		return
	}

	existing, err := h.approvals.GetApprovalRequest(r.Context(), id)
	if err != nil || existing == nil {
		h.renderTasks(w, r, s, "", "Approval request not found.")
		return
	}
	if err := workflow.ValidateDecision(existing, s.Username); err != nil {
		h.renderTasks(w, r, s, "", "Could not reject: "+err.Error())
		return
	}

	rejected, err := h.approvals.RejectApprovalRequest(r.Context(), id, s.Username, reason)
	if err != nil || rejected == nil {
		h.renderTasks(w, r, s, "", "Could not reject that request — it may already be decided.")
		return
	}
	h.renderTasks(w, r, s, fmt.Sprintf("Approval #%d rejected.", id), "")
}
