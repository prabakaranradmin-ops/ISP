package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// Scheduled plan changes — FR-BIL-023.
//
// PlanChangeManager is satisfied by *api.Handler, so the console validates,
// audits and counts exactly as the API does.
type PlanChangeManager interface {
	SchedulePlanChange(ctx context.Context, req billing.SchedulePlanChangeRequest) (*billing.ScheduledPlanChange, error)
	CancelPlanChange(ctx context.Context, id int, reason string) error
	ListPlanChanges(ctx context.Context, f billing.PlanChangeFilter) ([]billing.ScheduledPlanChange, error)
}

// planChangeMessages words each outcome carried back in the URL.
var planChangeMessages = map[string]string{
	"scheduled": "Plan change scheduled. The renewal on or after that date moves them and bills the new plan — nothing changes before then.",
	"cancelled": "Scheduled plan change cancelled. They stay on their current plan.",

	"exists": "Not scheduled: this subscriber already has a plan change waiting. Cancel that one first.",
	"same":   "Not scheduled: they are already on that plan.",
	"closed": "That plan change has already been applied or cancelled.",
	"reason": "Not cancelled: give a reason.",
	"failed": "That did not go through. Nothing was changed; try again.",
}

// Which outcomes are successes is not a lookup: a success redirects with the
// message in the URL and a failure re-renders the page with it, so the two
// are already distinguished by which branch runs. A planChangeSuccess map
// used to sit here and was never read.

// SchedulePlanChangeForm handles
// POST /staff/subscribers/{id}/scheduled-plan-change.
func (h *Handler) SchedulePlanChangeForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	// The same reach as the immediate plan change: this decides what the
	// next renewal charges.
	if !canManageCredentials(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only billing staff and the owner can schedule a plan change.")
		return
	}
	if h.planChanges == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Scheduled plan changes are not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}

	planID, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("new_plan_id")))
	if err != nil {
		h.renderSubscriberDetail(w, r, s, id, "Plan change not scheduled: choose the plan to move them to.")
		return
	}
	// Parsed in the operator's own zone, not UTC.
	//
	// effective_on is a plain calendar date: the column is DATE, the due-change
	// scanner compares it as a date, and Normalise compares it against "today".
	// Parsing it as UTC midnight made it an instant in a different zone from
	// the today it is compared with, and from the session the DATE cast uses.
	//
	// East of UTC that is merely inconsistent. West of it, 00:00 UTC is the
	// previous local day, so a change scheduled for today was both refused as
	// "already passed" and, if stored, written a day early. Three tests failed
	// on this the first time the suite ran between midnight and 05:30 IST,
	// which is the only window where UTC and IST disagree about the date.
	on, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(r.PostFormValue("effective_on")), time.Local)
	if err != nil {
		h.renderSubscriberDetail(w, r, s, id,
			"Plan change not scheduled: give the date it should take effect from.")
		return
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	_, err = h.planChanges.SchedulePlanChange(ctx, billing.SchedulePlanChangeRequest{
		SubscriberID: id, NewPlanID: planID, EffectiveOn: on, Reason: r.PostFormValue("reason"),
	})
	switch {
	case err == nil:
		http.Redirect(w, r, fmt.Sprintf("/staff/subscribers/%d?msg=%s", id,
			url.QueryEscape(planChangeMessages["scheduled"])), http.StatusSeeOther)
	case errors.Is(err, billing.ErrPlanChangeAlreadyScheduled):
		h.renderSubscriberDetail(w, r, s, id, planChangeMessages["exists"])
	case errors.Is(err, billing.ErrPlanChangeSamePlan):
		h.renderSubscriberDetail(w, r, s, id, planChangeMessages["same"])
	case billing.PlanChangeInvalidReason(err) != "":
		h.renderSubscriberDetail(w, r, s, id,
			"Plan change not scheduled: "+billing.PlanChangeInvalidReason(err)+".")
	default:
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: schedule plan change failed")
		h.renderSubscriberDetail(w, r, s, id, "Plan change not scheduled. Nothing was saved; try again.")
	}
}

// CancelPlanChangeForm handles
// POST /staff/scheduled-plan-changes/{id}/cancel.
func (h *Handler) CancelPlanChangeForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canManageCredentials(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden,
			"Only billing staff and the owner can cancel a scheduled plan change.")
		return
	}
	if h.planChanges == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"Scheduled plan changes are not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid plan change id.")
		return
	}
	subscriberID, _ := strconv.Atoi(r.PostFormValue("subscriber_id"))

	ctx := middleware.WithSubject(r.Context(), s.Username)
	err = h.planChanges.CancelPlanChange(ctx, id, r.PostFormValue("reason"))

	if subscriberID <= 0 {
		// Nothing to go back to; the subscriber directory is the least
		// surprising landing place.
		http.Redirect(w, r, "/staff/subscribers", http.StatusSeeOther)
		return
	}
	switch {
	case err == nil:
		http.Redirect(w, r, fmt.Sprintf("/staff/subscribers/%d?msg=%s", subscriberID,
			url.QueryEscape(planChangeMessages["cancelled"])), http.StatusSeeOther)
	case errors.Is(err, billing.ErrPlanChangeNotPending):
		h.renderSubscriberDetail(w, r, s, subscriberID, planChangeMessages["closed"])
	case billing.PlanChangeInvalidReason(err) != "":
		h.renderSubscriberDetail(w, r, s, subscriberID,
			"Not cancelled: "+billing.PlanChangeInvalidReason(err)+".")
	case errors.Is(err, billing.ErrPlanChangeNotFound):
		h.renderSubscriberDetail(w, r, s, subscriberID, "Not cancelled: no such plan change.")
	default:
		log.Error().Err(err).Int("change_id", id).Msg("staffui: cancel plan change failed")
		h.renderSubscriberDetail(w, r, s, subscriberID, "Not cancelled. Nothing was changed; try again.")
	}
}
