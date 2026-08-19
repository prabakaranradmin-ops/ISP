package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
	tickettask "github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// ticketCategories and ticketStatuses mirror the CHECK constraints on the
// tickets table. Validating here turns a bad enum value into a clean 422
// instead of a raw constraint-violation error surfacing as a 500.
var (
	ticketCategories = map[string]bool{"connectivity": true, "billing": true, "plan_change": true, "other": true}
	ticketStatuses   = map[string]bool{"open": true, "in_progress": true, "resolved": true, "closed": true}
	// ticketPriorities mirrors chk_tickets_priority (migration 023).
	ticketPriorities = map[string]bool{"low": true, "medium": true, "high": true, "critical": true}
)

// TicketRecord is the API representation of a support ticket.
type TicketRecord struct {
	ID           int       `json:"id"`
	SubscriberID int       `json:"subscriber_id"`
	Category     string    `json:"category"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	AssignedTo   *int      `json:"assigned_to,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// SLA fields (FR-SUP-001, FR-SUP-003 | MDS §4.13). Pointers because
	// tickets created before migration 023 have none — they are genuinely
	// absent rather than zero, and a zero time.Time rendered as
	// "0001-01-01T00:00:00Z" would read as a deadline in the distant past
	// and light up the breach scanner for every historical row.
	Priority           string     `json:"priority,omitempty"`
	SLAResponseDueAt   *time.Time `json:"sla_response_due_at,omitempty"`
	SLAResolutionDueAt *time.Time `json:"sla_resolution_due_at,omitempty"`
	RoutedRole         *string    `json:"routed_role,omitempty"`
}

// TicketAdminQuerier creates and updates tickets on behalf of any subscriber.
// Distinct from the portal's ticket queries, which are scoped to the calling
// subscriber. Satisfied by *db.TicketStore.
type TicketAdminQuerier interface {
	CreateTicketAdmin(ctx context.Context, subscriberID int, category, description string, priority *string) (*TicketRecord, error)
	UpdateTicketAdmin(ctx context.Context, ticketID int, status *string, assignedTo *int, priority *string) (*TicketRecord, error)
}

// CreateTicket handles POST /api/v1/tickets.
func (h *Handler) CreateTicket(w http.ResponseWriter, r *http.Request) {
	if h.tickets == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "ticket store not configured")
		return
	}

	var req struct {
		SubscriberID int     `json:"subscriber_id"`
		Category     string  `json:"category"`
		Description  string  `json:"description"`
		Priority     *string `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.SubscriberID == 0 {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_id is required")
		return
	}
	if !ticketCategories[req.Category] {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "category must be one of connectivity, billing, plan_change, other")
		return
	}
	if req.Description == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "description is required")
		return
	}
	if req.Priority != nil && !ticketPriorities[*req.Priority] {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "priority must be one of low, medium, high, critical")
		return
	}

	ticket, err := h.tickets.CreateTicketAdmin(r.Context(), req.SubscriberID, req.Category, req.Description, req.Priority)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create ticket failed")
		return
	}

	middleware.Audit(r.Context(), "ticket.create", strconv.Itoa(ticket.ID), map[string]any{
		"subscriber_id": req.SubscriberID, "category": req.Category,
	})
	h.emit(r.Context(), partner.EventTicketCreated, ticket.ID)
	writeJSON(w, http.StatusCreated, ticket)
}

// UpdateTicket handles PATCH /api/v1/tickets/{ticket_id}.
func (h *Handler) UpdateTicket(w http.ResponseWriter, r *http.Request) {
	ticketID, err := pathInt(r, "ticket_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid ticket_id")
		return
	}
	if h.tickets == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "ticket store not configured")
		return
	}

	var req struct {
		Status     *string `json:"status"`
		AssignedTo *int    `json:"assigned_to"`
		Priority   *string `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Status != nil && !ticketStatuses[*req.Status] {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "status must be one of open, in_progress, resolved, closed")
		return
	}
	if req.Priority != nil && !ticketPriorities[*req.Priority] {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "priority must be one of low, medium, high, critical")
		return
	}

	ticket, err := h.tickets.UpdateTicketAdmin(r.Context(), ticketID, req.Status, req.AssignedTo, req.Priority)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "update ticket failed")
		return
	}
	if ticket == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "ticket not found")
		return
	}

	// FR-NOTIF-007: tell the subscriber, but only when the status actually
	// moved — an assignee-only patch is an internal routing change the
	// subscriber has no stake in.
	if req.Status != nil {
		h.enqueueTicketUpdate(r.Context(), *ticket)
	}

	middleware.Audit(r.Context(), "ticket.update", strconv.Itoa(ticketID), map[string]any{
		"status": req.Status, "assigned_to": req.AssignedTo,
	})
	// Only resolution is published. Partners integrating on support outcomes
	// care that a ticket closed, not that it moved to in_progress — and a
	// webhook per intermediate step would be noise they have to filter.
	if req.Status != nil && *req.Status == "resolved" {
		h.emit(r.Context(), partner.EventTicketResolved, ticketID)
	}
	writeJSON(w, http.StatusOK, ticket)
}

// enqueueTicketUpdate tells the subscriber their ticket's status changed
// (FR-NOTIF-007, TMPL-008). Failures are logged, never returned: the ticket
// is already updated and the caller already has their 200 by this point.
func (h *Handler) enqueueTicketUpdate(ctx context.Context, ticket TicketRecord) {
	if h.tasks == nil || h.db == nil {
		return
	}

	username := ""
	if sub, err := h.db.GetSubscriberByID(ctx, ticket.SubscriberID); err == nil && sub != nil {
		username = sub.Username
	}

	payload, err := json.Marshal(tickettask.UpdatePayload{
		SubscriberID: ticket.SubscriberID,
		Username:     username,
		TicketID:     ticket.ID,
		Status:       ticket.Status,
	})
	if err != nil {
		return
	}

	task := jobqueue.NewTask(tickettask.TaskTypeTicketUpdate, payload,
		jobqueue.Queue("notifications"),
		jobqueue.MaxRetry(3),
		jobqueue.Retention(24*time.Hour))
	if _, err := h.tasks.Enqueue(task); err != nil {
		log.Warn().Err(err).Int("ticket_id", ticket.ID).
			Msg("api: ticket update enqueue failed")
	}
}
