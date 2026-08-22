package staffui

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/rs/zerolog/log"
)

// Speed override — a temporary, CoA-pushed rate change an owner can apply
// from a subscriber's own page, distinct from a plan change: the billed
// plan is untouched and the override reverts on its own (internal/fup's
// scanner sweeps for expiries). Modeled directly on FUPOverride
// (internal/api/sessions.go), which already does this shape of thing for
// the automatic FUP throttle.

// SpeedOverrideController sets or clears the override. Satisfied by
// *db.FUPStore — the same store already wired for LEA lookups.
type SpeedOverrideController interface {
	SetSpeedOverride(ctx context.Context, subscriberID int, rateLimit string, expiresAt *time.Time) error
	ClearSpeedOverride(ctx context.Context, subscriberID int) error
}

const speedOverrideTaskRetention = 24 * time.Hour

// requireOwnerSubscriber parses the subscriber id from the path and checks
// both that speed override is wired up and that the caller is the owner —
// the console hides the form from other roles, but the handler must not
// rely on that alone (see staffui's own package doc on this point).
func (h *Handler) requireOwnerSubscriber(w http.ResponseWriter, r *http.Request, s Session) (int, bool) {
	if s.Role != "isp_owner" {
		h.renderError(w, r, s, http.StatusForbidden, "Only the owner can change a subscriber's speed.")
		return 0, false
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return 0, false
	}
	if h.speedOverride == nil || h.tasks == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Speed override is not configured.")
		return 0, false
	}
	return id, true
}

// ApplySpeedOverride handles POST /staff/subscribers/{id}/speed-override.
func (h *Handler) ApplySpeedOverride(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	id, ok := h.requireOwnerSubscriber(w, r, s)
	if !ok {
		return
	}

	rateLimit := strings.TrimSpace(r.PostFormValue("rate_limit_string"))
	if !strings.Contains(rateLimit, "/") {
		h.renderSubscriberDetail(w, r, s, id, "Speed must be in MikroTik rate-limit form, for example 100M/100M.")
		return
	}

	var expiresAt *time.Time
	if raw := strings.TrimSpace(r.PostFormValue("duration_minutes")); raw != "" {
		minutes, err := strconv.Atoi(raw)
		if err != nil || minutes <= 0 {
			h.renderSubscriberDetail(w, r, s, id, "Duration must be a positive number of minutes, or blank for until cleared.")
			return
		}
		t := time.Now().Add(time.Duration(minutes) * time.Minute)
		expiresAt = &t
	}

	if err := h.speedOverride.SetSpeedOverride(r.Context(), id, rateLimit, expiresAt); err != nil {
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: set speed override failed")
		h.renderSubscriberDetail(w, r, s, id, "Could not apply the speed override.")
		return
	}
	h.enqueueSpeedOverrideCoA(r.Context(), id)
	http.Redirect(w, r, "/staff/subscribers/"+strconv.Itoa(id), http.StatusSeeOther)
}

// ClearSpeedOverride handles POST /staff/subscribers/{id}/speed-override/clear.
func (h *Handler) ClearSpeedOverride(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	id, ok := h.requireOwnerSubscriber(w, r, s)
	if !ok {
		return
	}

	if err := h.speedOverride.ClearSpeedOverride(r.Context(), id); err != nil {
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: clear speed override failed")
		h.renderSubscriberDetail(w, r, s, id, "Could not clear the speed override.")
		return
	}
	h.enqueueSpeedOverrideCoA(r.Context(), id)
	http.Redirect(w, r, "/staff/subscribers/"+strconv.Itoa(id), http.StatusSeeOther)
}

// enqueueSpeedOverrideCoA pushes the same CoA task the FUP scanner and the
// JSON API's own speed-override endpoint use. NasIP is left blank
// deliberately — fup.CoAHandler.ProcessTask resolves the live NAS session
// fresh at execution time rather than trusting a snapshot.
func (h *Handler) enqueueSpeedOverrideCoA(ctx context.Context, subscriberID int) {
	payload, err := json.Marshal(fup.CoAPayload{SubscriberID: subscriberID})
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", subscriberID).Msg("staffui: marshal speed-override CoA payload failed")
		return
	}
	task := jobqueue.NewTask(fup.TaskTypeCoA, payload,
		jobqueue.Queue(fup.QueueNetCommands), jobqueue.MaxRetry(5), jobqueue.Retention(speedOverrideTaskRetention))
	if _, err := h.tasks.Enqueue(task); err != nil {
		log.Error().Err(err).Int("subscriber_id", subscriberID).Msg("staffui: enqueue speed-override CoA task failed")
	}
}
