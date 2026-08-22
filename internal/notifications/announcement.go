package notifications

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Announcements — FR-ANN-001..002 | MDS §4.17.
//
// An announcement is not a new delivery mechanism. Its only jobs are
// deciding *who* and enqueuing one ordinary notification per recipient per
// channel; suppression, sending, logging and delivery callbacks are the
// path that already exists and is already tested (FR-ANN-002).

// Announcement lifecycle. 'sending' is the atomic claim that stops a
// double-click broadcasting twice to the same segment.
const (
	AnnouncementDraft   = "draft"
	AnnouncementSending = "sending"
	AnnouncementSent    = "sent"
	AnnouncementFailed  = "failed"
)

// ErrAnnouncementNotDraft is returned when a send is attempted on an
// announcement that is not in draft — already sending, or already sent.
var ErrAnnouncementNotDraft = errors.New("notifications: only a draft announcement can be sent")

var (
	AnnouncementsSentTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "announcements_sent_total",
		Help: "Announcements broadcast",
	})
	AnnouncementRecipientsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "announcement_recipients_total",
		Help: "Notification tasks enqueued by announcement fan-out",
	})
)

// Announcement is a staff-composed broadcast.
type Announcement struct {
	ID       int      `json:"id"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Channels []string `json:"channels"`
	Class    string   `json:"class"`

	SegmentFranchiseID *int    `json:"segment_franchise_id,omitempty"`
	SegmentPlanID      *int    `json:"segment_plan_id,omitempty"`
	SegmentStatus      *string `json:"segment_status,omitempty"`

	// SubscriberIDs, when non-empty, targets exactly these subscribers
	// instead of the segment filters above — the console's multi-select
	// "send to these" bulk action. Transient: read at CreateAnnouncement
	// time to populate announcement_recipients, never scanned back off the
	// announcements row (which has no column for it), so a later
	// GetAnnouncement/ListAnnouncements leaves this empty.
	SubscriberIDs []int `json:"subscriber_ids,omitempty"`

	ShowInPortal   bool       `json:"show_in_portal"`
	Status         string     `json:"status"`
	RecipientCount int        `json:"recipient_count"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	SentAt         *time.Time `json:"sent_at,omitempty"`
}

// ValidChannel reports whether c is a dispatched channel an announcement may
// target. "portal" is deliberately absent — a banner is displayed, not
// transmitted, and is the ShowInPortal flag instead (MDS §4.17).
func ValidChannel(c string) bool {
	switch c {
	case "whatsapp", "sms", "email", "push":
		return true
	default:
		return false
	}
}

// ValidClass reports whether c is a legal announcement class.
func ValidClass(c string) bool { return c == "marketing" || c == "transactional" }
