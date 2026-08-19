package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// sessionTaskRetention bounds how long a completed session-control task's
// result is kept in the queue, matching the retention the FUP scanner already uses
// for CoA tasks.
const sessionTaskRetention = 24 * time.Hour

// SessionReader serves the NOC-facing live session view.
// Satisfied by *cache.SessionStore.
type SessionReader interface {
	GetActiveSession(ctx context.Context, subscriberID int) (*health.SessionSummary, error)
}

// SessionController resolves a NAS-issued session_id to the subscriber and NAS
// address that own it, and flips FUP throttle state. Satisfied by
// *db.FUPStore.
type SessionController interface {
	ResolveSessionSubscriber(ctx context.Context, sessionID string) (subscriberID int, nasIP string, err error)
	SetFUPActive(ctx context.Context, subscriberID int, active bool) error
}

// TaskEnqueuer is the subset of *jobqueue.Client the API needs to trigger
// session-control tasks that the radiusd worker pool executes.
type TaskEnqueuer interface {
	Enqueue(task *jobqueue.Task, opts ...jobqueue.Option) (*jobqueue.TaskInfo, error)
}

// GetActiveSession handles GET /api/v1/sessions/{subscriber_id}/active.
func (h *Handler) GetActiveSession(w http.ResponseWriter, r *http.Request) {
	subscriberID, err := pathInt(r, "subscriber_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid subscriber_id")
		return
	}
	if h.sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "session store not configured")
		return
	}

	sess, err := h.sessions.GetActiveSession(r.Context(), subscriberID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "session lookup failed")
		return
	}
	if sess == nil {
		writeError(w, http.StatusNotFound, "ERR_NO_ACTIVE_SESSION", "no active session found")
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// DisconnectSession handles POST /api/v1/sessions/{session_id}/disconnect.
// Enqueues a PoD (Disconnect-Request) task; the radiusd worker pool sends the
// packet and retries with backoff, so this returns 202 rather than waiting for
// the NAS round trip.
//
// API §7 | DDS §5.3
func (h *Handler) DisconnectSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "session_id is required")
		return
	}
	if h.sessionCtl == nil || h.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "session control not configured")
		return
	}

	subscriberID, nasIP, err := h.sessionCtl.ResolveSessionSubscriber(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "ERR_SESSION_NOT_FOUND", "no active session with that id")
		return
	}

	payload, _ := json.Marshal(fup.PoDPayload{SubscriberID: subscriberID}) //nolint:errcheck // static struct, cannot fail
	task := jobqueue.NewTask(fup.TaskTypePoD, payload,
		jobqueue.Queue(fup.QueueNetCommands), jobqueue.MaxRetry(5), jobqueue.Retention(sessionTaskRetention))
	if _, err := h.tasks.Enqueue(task); err != nil {
		log.Error().Err(err).Str("session_id", sessionID).Msg("api: enqueue PoD task failed")
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not enqueue disconnect")
		return
	}

	middleware.Audit(r.Context(), "session.disconnect", sessionID, map[string]any{
		"subscriber_id": subscriberID, "nas_ip": nasIP,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "pod_enqueued"})
}

// fupOverrideRequest is the POST /api/v1/sessions/{session_id}/fup-override body.
type fupOverrideRequest struct {
	Action string `json:"action"` // "apply" | "remove"
}

// FUPOverride handles POST /api/v1/sessions/{session_id}/fup-override.
// Sets fup_active directly, then enqueues the same CoA task the scanner would:
// GetSubscriberNASSession picks the throttled or full rate based on that flag,
// so one code path drives both automatic and manual throttling.
//
// API §7 | DDS §5.3
func (h *Handler) FUPOverride(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "session_id is required")
		return
	}
	if h.sessionCtl == nil || h.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "session control not configured")
		return
	}

	var req fupOverrideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Action != "apply" && req.Action != "remove" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "action must be \"apply\" or \"remove\"")
		return
	}

	subscriberID, nasIP, err := h.sessionCtl.ResolveSessionSubscriber(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "ERR_SESSION_NOT_FOUND", "no active session with that id")
		return
	}

	active := req.Action == "apply"
	if err := h.sessionCtl.SetFUPActive(r.Context(), subscriberID, active); err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not update FUP state")
		return
	}

	payload, _ := json.Marshal(fup.CoAPayload{SubscriberID: subscriberID, NasIP: nasIP}) //nolint:errcheck
	task := jobqueue.NewTask(fup.TaskTypeCoA, payload,
		jobqueue.Queue(fup.QueueNetCommands), jobqueue.MaxRetry(5), jobqueue.Retention(sessionTaskRetention))
	if _, err := h.tasks.Enqueue(task); err != nil {
		log.Error().Err(err).Str("session_id", sessionID).Msg("api: enqueue CoA task failed")
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not enqueue CoA")
		return
	}

	middleware.Audit(r.Context(), "session.fup_override", sessionID, map[string]any{
		"subscriber_id": subscriberID, "action": req.Action,
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "coa_enqueued"})
}
