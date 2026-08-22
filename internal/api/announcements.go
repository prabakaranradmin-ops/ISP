package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// Announcement endpoints — FR-ANN-001..002 | MDS §4.17.

// AnnouncementQuerier is the persistence surface broadcasts need.
// Satisfied by *db.AnnouncementStore.
type AnnouncementQuerier interface {
	CreateAnnouncement(ctx context.Context, a notifications.Announcement) (*notifications.Announcement, error)
	GetAnnouncement(ctx context.Context, id int) (*notifications.Announcement, error)
	ListAnnouncements(ctx context.Context, status *string) ([]notifications.Announcement, error)
	ListPortalAnnouncements(ctx context.Context, subscriberID int) ([]notifications.Announcement, error)
	// ClaimAnnouncementForSending must be an atomic conditional update over a
	// draft row, returning (nil, nil) when the claim did not land — that is
	// what stops a double-click broadcasting twice.
	ClaimAnnouncementForSending(ctx context.Context, id int) (*notifications.Announcement, error)
	FinishAnnouncement(ctx context.Context, id int, status string, recipientCount int) error
	// ListSegmentSubscriberIDs resolves an announcement's recipients: its
	// explicit list (announcement_recipients) if one was given at creation,
	// otherwise the franchise/plan/status segment filters.
	ListSegmentSubscriberIDs(ctx context.Context, announcementID int, franchiseID, planID *int, status *string) ([]int, error)
}

type createAnnouncementRequest struct {
	Title              string   `json:"title"`
	Body               string   `json:"body"`
	Channels           []string `json:"channels"`
	Class              string   `json:"class"`
	SegmentFranchiseID *int     `json:"segment_franchise_id"`
	SegmentPlanID      *int     `json:"segment_plan_id"`
	SegmentStatus      *string  `json:"segment_status"`
	ShowInPortal       bool     `json:"show_in_portal"`
	// SubscriberIDs targets exactly these subscribers — the console's
	// multi-select bulk notification — instead of the segment filters
	// above. Mutually exclusive with them: mixing "these specific people"
	// with "everyone matching a filter" in one request is exactly the kind
	// of ambiguity that sends a broadcast to the wrong reach.
	SubscriberIDs []int `json:"subscriber_ids"`
}

// CreateAnnouncement handles POST /api/v1/announcements — composing a draft.
//
// Creating never sends. A broadcast that went out the moment it was typed
// would have no review step at all, and the segment filters are exactly the
// kind of thing worth re-reading before addressing tens of thousands of
// people.
func (h *Handler) CreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	if h.announcements == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "announcement store not configured")
		return
	}

	var req createAnnouncementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Title == "" || req.Body == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "title and body are required")
		return
	}
	for _, c := range req.Channels {
		if !notifications.ValidChannel(c) {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
				"channels must be drawn from whatsapp, sms, email, push (a portal banner is show_in_portal)")
			return
		}
	}
	// An announcement addressed to nothing would report success having
	// reached nobody. Checked here as well as by
	// chk_announcement_has_destination so the caller gets a readable reason.
	if len(req.Channels) == 0 && !req.ShowInPortal {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
			"an announcement needs at least one channel or show_in_portal")
		return
	}
	if len(req.SubscriberIDs) > 0 {
		if req.SegmentFranchiseID != nil || req.SegmentPlanID != nil || req.SegmentStatus != nil {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
				"subscriber_ids cannot be combined with segment_franchise_id/segment_plan_id/segment_status")
			return
		}
		if len(req.SubscriberIDs) > maxBulkSubscribers {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION",
				fmt.Sprintf("subscriber_ids must not exceed %d per announcement", maxBulkSubscribers))
			return
		}
	}
	class := req.Class
	if class == "" {
		class = "marketing" // so DND opt-out is honoured by default
	}
	if !notifications.ValidClass(class) {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "class must be marketing or transactional")
		return
	}

	created, err := h.announcements.CreateAnnouncement(r.Context(), notifications.Announcement{
		Title: req.Title, Body: req.Body, Channels: req.Channels, Class: class,
		SegmentFranchiseID: req.SegmentFranchiseID, SegmentPlanID: req.SegmentPlanID,
		SegmentStatus: req.SegmentStatus, ShowInPortal: req.ShowInPortal,
		SubscriberIDs: req.SubscriberIDs,
		CreatedBy:     middleware.SubjectFromContext(r.Context()),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "create announcement failed")
		return
	}

	middleware.Audit(r.Context(), "announcement.create", strconv.Itoa(created.ID), map[string]any{
		"channels": created.Channels, "class": created.Class,
	})
	writeJSON(w, http.StatusCreated, created)
}

// SendBulkNotification creates and immediately sends a notification to
// exactly the given subscriber ids — the console's multi-select "Notify
// these" bulk action.
//
// Unlike CreateAnnouncement/SendAnnouncement's own two-step review flow
// (compose now, broadcast later, often to tens of thousands of people via a
// segment filter), a bulk console notification already names its small,
// hand-picked audience: there is nothing left to review before sending, so
// this collapses create+claim+fan-out+finish into one call for a direct,
// non-HTTP caller (internal/staffui) the same way ProvisionSubscriber does
// for subscriber creation.
func (h *Handler) SendBulkNotification(ctx context.Context, requestedBy string, ids []int, title, body string, channels []string, showInPortal bool) (enqueued int, err error) {
	created, err := h.announcements.CreateAnnouncement(ctx, notifications.Announcement{
		Title: title, Body: body, Channels: channels,
		// Transactional: a hand-picked, staff-initiated notice (an outage
		// window, an account-specific heads-up) is a service message, not
		// marketing, so DND opt-out should not silently swallow it.
		Class:         "transactional",
		ShowInPortal:  showInPortal,
		SubscriberIDs: ids,
		CreatedBy:     requestedBy,
	})
	if err != nil {
		return 0, fmt.Errorf("create bulk announcement: %w", err)
	}

	claimed, err := h.announcements.ClaimAnnouncementForSending(ctx, created.ID)
	if err != nil {
		return 0, fmt.Errorf("claim bulk announcement %d: %w", created.ID, err)
	}
	if claimed == nil {
		return 0, fmt.Errorf("bulk announcement %d was not left in draft state", created.ID)
	}

	recipients, err := h.announcements.ListSegmentSubscriberIDs(ctx, claimed.ID,
		claimed.SegmentFranchiseID, claimed.SegmentPlanID, claimed.SegmentStatus)
	if err != nil {
		h.finishAnnouncement(ctx, created.ID, notifications.AnnouncementFailed, 0)
		return 0, fmt.Errorf("resolve bulk announcement %d recipients: %w", created.ID, err)
	}

	enqueued = h.fanOutAnnouncement(claimed, recipients)
	h.finishAnnouncement(ctx, created.ID, notifications.AnnouncementSent, enqueued)

	notifications.AnnouncementsSentTotal.Inc()
	middleware.Audit(ctx, "announcement.send", strconv.Itoa(created.ID), map[string]any{
		"recipients": len(recipients), "tasks_enqueued": enqueued, "bulk": true,
	})
	return enqueued, nil
}

// ListAnnouncements handles GET /api/v1/announcements?status=.
func (h *Handler) ListAnnouncements(w http.ResponseWriter, r *http.Request) {
	if h.announcements == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "announcement store not configured")
		return
	}
	var status *string
	if v := r.URL.Query().Get("status"); v != "" {
		status = &v
	}

	list, err := h.announcements.ListAnnouncements(r.Context(), status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list announcements failed")
		return
	}
	if list == nil {
		list = []notifications.Announcement{}
	}
	writeJSON(w, http.StatusOK, list)
}

// SendAnnouncement handles POST /api/v1/announcements/{id}/send — the fan-out.
//
// FR: FR-ANN-001..002 | MDS §4.17
func (h *Handler) SendAnnouncement(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.announcements == nil || h.tasks == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "announcement dispatch not configured")
		return
	}

	existing, err := h.announcements.GetAnnouncement(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "announcement lookup failed")
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "announcement not found")
		return
	}

	// Atomic claim: only one caller can move a draft to 'sending', so a
	// double-click cannot broadcast the same message twice to the same
	// segment (MDS §4.17).
	claimed, err := h.announcements.ClaimAnnouncementForSending(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not claim announcement")
		return
	}
	if claimed == nil {
		writeError(w, http.StatusConflict, "ERR_NOT_DRAFT", notifications.ErrAnnouncementNotDraft.Error())
		return
	}

	recipients, err := h.announcements.ListSegmentSubscriberIDs(r.Context(), id,
		claimed.SegmentFranchiseID, claimed.SegmentPlanID, claimed.SegmentStatus)
	if err != nil {
		// The claim already moved it out of draft; mark it failed so it is
		// visibly stuck rather than silently sitting in 'sending' forever.
		h.finishAnnouncement(r.Context(), id, notifications.AnnouncementFailed, 0)
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not resolve the announcement segment")
		return
	}

	enqueued := h.fanOutAnnouncement(claimed, recipients)

	// A portal-only announcement legitimately enqueues nothing, so 'sent'
	// with zero recipients is a correct outcome, not a failure.
	h.finishAnnouncement(r.Context(), id, notifications.AnnouncementSent, enqueued)

	notifications.AnnouncementsSentTotal.Inc()
	middleware.Audit(r.Context(), "announcement.send", strconv.Itoa(id), map[string]any{
		"recipients": len(recipients), "tasks_enqueued": enqueued,
	})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": notifications.AnnouncementSent, "recipients": len(recipients), "tasks_enqueued": enqueued,
	})
}

// fanOutAnnouncement enqueues one task per recipient per channel, returning
// how many actually landed.
//
// One task per message rather than one bulk task: each gets the queue's retry
// and dead-lettering, and a single unreachable subscriber cannot fail the
// broadcast for everyone else.
func (h *Handler) fanOutAnnouncement(a *notifications.Announcement, recipients []int) int {
	enqueued := 0
	for _, subscriberID := range recipients {
		for _, channel := range a.Channels {
			payload, err := json.Marshal(notifications.AnnouncementPayload{
				AnnouncementID: a.ID, SubscriberID: subscriberID, Channel: channel,
				Title: a.Title, Body: a.Body, Class: a.Class,
			})
			if err != nil {
				continue // a static struct cannot realistically fail to marshal
			}
			task := jobqueue.NewTask(notifications.TaskTypeAnnouncement, payload,
				jobqueue.Queue(notifications.QueueAnnouncements),
				// One delivery per announcement/subscriber/channel, so a
				// retried send cannot message the same person twice.
				jobqueue.TaskID(notifications.AnnouncementTaskID(a.ID, subscriberID, channel)),
				jobqueue.MaxRetry(3),
				jobqueue.Retention(announcementRetention))

			if _, err := h.tasks.Enqueue(task); err != nil {
				if errors.Is(err, jobqueue.ErrTaskIDConflict) {
					continue // already enqueued by an earlier attempt
				}
				log.Error().Err(err).
					Int("announcement_id", a.ID).Int("subscriber_id", subscriberID).
					Str("channel", channel).Msg("api: announcement fan-out enqueue failed")
				continue
			}
			enqueued++
			notifications.AnnouncementRecipientsTotal.Inc()
		}
	}
	return enqueued
}

func (h *Handler) finishAnnouncement(ctx context.Context, id int, status string, count int) {
	if err := h.announcements.FinishAnnouncement(ctx, id, status, count); err != nil {
		log.Error().Err(err).Int("announcement_id", id).
			Msg("api: could not record the announcement outcome")
	}
}

// announcementRetention bounds how long a completed fan-out task's result is
// kept, matching the retention the session-control tasks use.
const announcementRetention = 24 * time.Hour

// GetPortalAnnouncements handles GET /api/v1/announcements/portal — the
// banner feed for the authenticated subscriber.
//
// Segment-scoped: a banner aimed at one franchise's customers must not
// appear on everyone's dashboard.
func (h *Handler) GetPortalAnnouncements(w http.ResponseWriter, r *http.Request) {
	if h.announcements == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "announcement store not configured")
		return
	}
	subscriberID := middleware.SubscriberIDFromContext(r.Context())
	if subscriberID == 0 {
		writeError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "missing subscriber context")
		return
	}

	list, err := h.announcements.ListPortalAnnouncements(r.Context(), subscriberID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "list portal announcements failed")
		return
	}
	if list == nil {
		list = []notifications.Announcement{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── FR-NOTIF-013: push token registration ───────────────────────────────────

// PushTokenQuerier registers a subscriber's device for push.
// Satisfied by *db.NotificationStore.
type PushTokenQuerier interface {
	RegisterPushToken(ctx context.Context, subscriberID int, token, platform string) error
}

type registerPushTokenRequest struct {
	Token    string `json:"token"`
	Platform string `json:"platform"`
}

// RegisterPushToken handles POST /api/v1/push-tokens, called by the mobile
// app for the authenticated subscriber.
//
// FR: FR-NOTIF-013 (and the storage FR-MOB-001 will need)
func (h *Handler) RegisterPushToken(w http.ResponseWriter, r *http.Request) {
	if h.pushTokens == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "push token store not configured")
		return
	}
	// The subscriber comes from the token, never the body: letting a caller
	// name the subscriber would let anyone point somebody else's device at
	// their own account, or vice versa.
	subscriberID := middleware.SubscriberIDFromContext(r.Context())
	if subscriberID == 0 {
		writeError(w, http.StatusUnauthorized, "ERR_UNAUTHORIZED", "missing subscriber context")
		return
	}

	var req registerPushTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "token is required")
		return
	}
	switch req.Platform {
	case "ios", "android", "web":
	default:
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "platform must be ios, android or web")
		return
	}

	if err := h.pushTokens.RegisterPushToken(r.Context(), subscriberID, req.Token, req.Platform); err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "register push token failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered"})
}
