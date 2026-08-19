package notifications

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// Announcement fan-out task — FR-ANN-001..002 | MDS §4.17.

const (
	// TaskTypeAnnouncement carries one announcement to one subscriber on one
	// channel. Fan-out enqueues N×M of these rather than one bulk task: a
	// per-recipient task gets the queue's existing retry and dead-lettering, and
	// one unreachable subscriber cannot fail the whole broadcast.
	TaskTypeAnnouncement = "notif:announcement"

	// QueueAnnouncements is deliberately separate from the transactional
	// notifications queue. A 50,000-recipient marketing blast must not sit
	// in front of a payment receipt or a suspension notice — those are
	// time-critical and these are not.
	QueueAnnouncements = "announcements"
)

// AnnouncementPayload is the task payload for one fanned-out message.
type AnnouncementPayload struct {
	AnnouncementID int    `json:"announcement_id"`
	SubscriberID   int    `json:"subscriber_id"`
	Channel        string `json:"channel"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	Class          string `json:"class"`
}

// AnnouncementHandler delivers one fanned-out announcement message.
type AnnouncementHandler struct {
	dispatcher *Dispatcher
}

// NewAnnouncementHandler constructs an AnnouncementHandler.
func NewAnnouncementHandler(d *Dispatcher) *AnnouncementHandler {
	return &AnnouncementHandler{dispatcher: d}
}

// ProcessTask implements jobqueue.Handler for TaskTypeAnnouncement.
//
// Routes through the ordinary Dispatcher, which is the whole point of
// FR-ANN-002: DND suppression, channel selection and notification_log all
// come from the path that already exists rather than a parallel one.
func (h *AnnouncementHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p AnnouncementPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("announcement: unmarshal payload: %w: %w", err, jobqueue.SkipRetry)
	}
	if h.dispatcher == nil {
		return fmt.Errorf("announcement: dispatcher not configured")
	}

	err := h.dispatcher.Dispatch(ctx, NotificationTask{
		SubscriberID: p.SubscriberID,
		Channel:      p.Channel,
		TriggerEvent: fmt.Sprintf("announcement_%d", p.AnnouncementID),
		Class:        p.Class,
		Subject:      p.Title,
		Variables:    []string{p.Body},
	})
	if err != nil {
		return fmt.Errorf("announcement %d to sub %d on %s: %w",
			p.AnnouncementID, p.SubscriberID, p.Channel, err)
	}
	return nil
}

// AnnouncementTaskID is the idempotency key for one fanned-out message: one
// per announcement, per subscriber, per channel.
//
// Without it, a retried or duplicated send would deliver the same broadcast
// to the same person twice — the announcement-level claim stops a double
// *send*, and this stops a double *delivery* within one send.
func AnnouncementTaskID(announcementID, subscriberID int, channel string) string {
	return fmt.Sprintf("ann-%d-%d-%s", announcementID, subscriberID, channel)
}
