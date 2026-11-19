package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Scheduled plan changes — FR-BIL-023 | migration 059. See
// internal/billing/plan_change.go for the domain rules.
//
// Distinct from POST /subscribers/{id}/plan-change (FR-LC-001), which moves
// the subscriber now and prorates. This one waits for the renewal, and the
// renewal buys the next cycle on the new plan.

// PlanChangeStore schedules, cancels and reads scheduled plan changes.
// Satisfied by *db.BillingStore.
type PlanChangeStore interface {
	SchedulePlanChange(ctx context.Context, req billing.SchedulePlanChangeRequest, by string) (*billing.ScheduledPlanChange, error)
	CancelPlanChange(ctx context.Context, id int, reason, by string) error
	GetPlanChange(ctx context.Context, id int) (*billing.ScheduledPlanChange, error)
	ListPlanChanges(ctx context.Context, f billing.PlanChangeFilter) ([]billing.ScheduledPlanChange, error)
}

// ErrPlanChangesUnwired is returned when no store is configured.
var ErrPlanChangesUnwired = errors.New("api: scheduled plan changes not configured")

// SchedulePlanChange records a plan change for a future renewal.
func (h *Handler) SchedulePlanChange(ctx context.Context, req billing.SchedulePlanChangeRequest) (*billing.ScheduledPlanChange, error) {
	if h.planChanges == nil {
		return nil, ErrPlanChangesUnwired
	}
	c, err := h.planChanges.SchedulePlanChange(ctx, req, middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "subscriber.plan_change_scheduled", strconv.Itoa(c.SubscriberID), map[string]any{
		"change_id": c.ID, "from_plan_id": c.FromPlanID, "new_plan_id": c.NewPlanID,
		"effective_on": c.EffectiveOn.Format("2006-01-02"), "reason": c.Reason,
	})
	billing.LifecycleActionsTotal.WithLabelValues("plan_change_scheduled").Inc()
	return c, nil
}

// CancelPlanChange withdraws a pending change.
func (h *Handler) CancelPlanChange(ctx context.Context, id int, reason string) error {
	if h.planChanges == nil {
		return ErrPlanChangesUnwired
	}
	if err := h.planChanges.CancelPlanChange(ctx, id, reason, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	middleware.Audit(ctx, "subscriber.plan_change_cancelled", strconv.Itoa(id), map[string]any{
		"reason": strings.TrimSpace(reason),
	})
	billing.LifecycleActionsTotal.WithLabelValues("plan_change_cancelled").Inc()
	return nil
}

// GetPlanChange loads one scheduled change.
func (h *Handler) GetPlanChange(ctx context.Context, id int) (*billing.ScheduledPlanChange, error) {
	if h.planChanges == nil {
		return nil, ErrPlanChangesUnwired
	}
	return h.planChanges.GetPlanChange(ctx, id)
}

// ListPlanChanges lists scheduled changes, pending first.
func (h *Handler) ListPlanChanges(ctx context.Context, f billing.PlanChangeFilter) ([]billing.ScheduledPlanChange, error) {
	if h.planChanges == nil {
		return nil, ErrPlanChangesUnwired
	}
	return h.planChanges.ListPlanChanges(ctx, f)
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

type planChangeJSON struct {
	ID               int    `json:"id"`
	SubscriberID     int    `json:"subscriber_id"`
	Username         string `json:"username,omitempty"`
	FromPlanID       int    `json:"from_plan_id"`
	FromPlanName     string `json:"from_plan_name"`
	NewPlanID        int    `json:"new_plan_id"`
	NewPlanName      string `json:"new_plan_name"`
	NewPlanPrice     string `json:"new_plan_price"`
	EffectiveOn      string `json:"effective_on"`
	Status           string `json:"status"`
	Reason           string `json:"reason,omitempty"`
	ScheduledBy      string `json:"scheduled_by"`
	ScheduledAt      string `json:"scheduled_at"`
	AppliedAt        string `json:"applied_at,omitempty"`
	AppliedInvoiceID *int   `json:"applied_invoice_id,omitempty"`
	CancelReason     string `json:"cancel_reason,omitempty"`
}

func toPlanChangeJSON(c billing.ScheduledPlanChange) planChangeJSON {
	out := planChangeJSON{
		ID: c.ID, SubscriberID: c.SubscriberID, Username: c.Username,
		FromPlanID: c.FromPlanID, FromPlanName: c.FromPlanName,
		NewPlanID: c.NewPlanID, NewPlanName: c.NewPlanName,
		NewPlanPrice: c.NewPlanPrice.StringFixed(2),
		EffectiveOn:  c.EffectiveOn.Format("2006-01-02"),
		Status:       c.Status, Reason: c.Reason,
		ScheduledBy:      c.ScheduledBy,
		ScheduledAt:      c.ScheduledAt.Format("2006-01-02T15:04:05Z07:00"),
		AppliedInvoiceID: c.AppliedInvoiceID,
		CancelReason:     c.CancelReason,
	}
	if c.AppliedAt != nil {
		out.AppliedAt = c.AppliedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

// writePlanChangeError maps a failure to a status and code.
func writePlanChangeError(w http.ResponseWriter, err error) {
	switch {
	case billing.PlanChangeInvalidReason(err) != "":
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", billing.PlanChangeInvalidReason(err))
	case errors.Is(err, billing.ErrPlanChangeAlreadyScheduled):
		writeError(w, http.StatusConflict, "ERR_PLAN_CHANGE_EXISTS",
			"this subscriber already has a plan change scheduled; cancel it before scheduling another")
	case errors.Is(err, billing.ErrPlanChangeSamePlan):
		writeError(w, http.StatusConflict, "ERR_SAME_PLAN", "the subscriber is already on that plan")
	case errors.Is(err, billing.ErrPlanChangeNotPending):
		writeError(w, http.StatusConflict, "ERR_PLAN_CHANGE_CLOSED",
			"that plan change has already been applied or cancelled")
	case errors.Is(err, billing.ErrPlanChangeNotFound), isNotFound(err):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "not found")
	case errors.Is(err, ErrPlanChangesUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "scheduled plan changes not configured")
	default:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "plan change operation failed")
	}
}

type schedulePlanChangeBody struct {
	NewPlanID   int    `json:"new_plan_id"`
	EffectiveOn string `json:"effective_on"` // YYYY-MM-DD
	Reason      string `json:"reason"`
}

// SchedulePlanChangeHTTP handles
// POST /api/v1/subscribers/{id}/scheduled-plan-change.
func (h *Handler) SchedulePlanChangeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b schedulePlanChangeBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	// Parsed in UTC, as a bare date: effective_on is a day, and attaching
	// the server's zone to it would make a change scheduled for the 1st
	// land on the 31st for half the world.
	on, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(b.EffectiveOn), time.UTC)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"effective_on must be a date, for example \"2026-04-01\"")
		return
	}
	c, err := h.SchedulePlanChange(r.Context(), billing.SchedulePlanChangeRequest{
		SubscriberID: id, NewPlanID: b.NewPlanID, EffectiveOn: on, Reason: b.Reason,
	})
	if err != nil {
		writePlanChangeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toPlanChangeJSON(*c))
}

// ListPlanChangesHTTP handles
// GET /api/v1/scheduled-plan-changes?status=&subscriber_id=.
func (h *Handler) ListPlanChangesHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := billing.PlanChangeFilter{Status: q.Get("status"), Limit: parseLimit(q, 100, 500)}
	switch f.Status {
	case "", billing.PlanChangeScheduled, billing.PlanChangeApplied, billing.PlanChangeCancelled:
	default:
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"status must be scheduled, applied or cancelled")
		return
	}
	if v := q.Get("subscriber_id"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_id must be a positive number")
			return
		}
		f.SubscriberID = n
	}
	list, err := h.ListPlanChanges(r.Context(), f)
	if err != nil {
		writePlanChangeError(w, err)
		return
	}
	out := make([]planChangeJSON, 0, len(list))
	for _, c := range list {
		out = append(out, toPlanChangeJSON(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"plan_changes": out})
}

// GetPlanChangeHTTP handles GET /api/v1/scheduled-plan-changes/{id}.
func (h *Handler) GetPlanChangeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	c, err := h.GetPlanChange(r.Context(), id)
	if err != nil {
		writePlanChangeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPlanChangeJSON(*c))
}

type cancelPlanChangeBody struct {
	Reason string `json:"reason"`
}

// CancelPlanChangeHTTP handles
// POST /api/v1/scheduled-plan-changes/{id}/cancel.
func (h *Handler) CancelPlanChangeHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b cancelPlanChangeBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := h.CancelPlanChange(r.Context(), id, b.Reason); err != nil {
		writePlanChangeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
