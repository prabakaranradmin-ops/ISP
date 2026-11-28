package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// Announcement operations free of HTTP — FR-ANN-001..002 | MDS §4.17.
//
// Shared by the API routes in announcements.go and the console's
// Announcements screen: one set of validation, one atomic draft→sending
// claim (so a double-click cannot broadcast twice), one fan-out, one audit
// line.

// Errors the announcement operations report. ErrAnnouncementInvalid wraps an
// operator-facing sentence.
var (
	ErrAnnouncementsUnavailable = errors.New("api: announcement store not configured")
	ErrAnnouncementDispatchDown = errors.New("api: announcement dispatch not configured")
	ErrAnnouncementInvalid      = errors.New("api: invalid announcement")
	ErrAnnouncementNotFound     = errors.New("api: announcement not found")
	errAnnouncementSegment      = errors.New("api: could not resolve the announcement segment")
	// ErrAnnouncementQueueDown is a broadcast that had messages to send and
	// could queue none of them. It is recorded failed rather than sent:
	// until 2026-11-28 it was recorded 'sent' and reported as success, with
	// nobody having received anything and no way to send it again.
	ErrAnnouncementQueueDown = errors.New("api: no announcement message could be queued")
)

func announcementInvalid(msg string) error { return fmt.Errorf("%w: %s", ErrAnnouncementInvalid, msg) }

// AnnouncementInvalidReason returns the sentence inside an
// ErrAnnouncementInvalid, or "".
func AnnouncementInvalidReason(err error) string {
	if !errors.Is(err, ErrAnnouncementInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrAnnouncementInvalid.Error()+": ")
}

// AnnouncementInput is a draft to compose.
type AnnouncementInput struct {
	Title, Body        string
	Channels           []string
	Class              string // "" defaults to marketing
	SegmentFranchiseID *int
	SegmentPlanID      *int
	SegmentStatus      *string
	// SegmentGroupID addresses a named subscriber group (FR-GRP-001).
	SegmentGroupID *int
	ShowInPortal   bool
	SubscriberIDs  []int
}

// ComposeAnnouncement validates and stores a draft. It never sends: a
// broadcast that went out the moment it was typed would have no review step,
// and segment filters are exactly the thing worth re-reading before
// addressing tens of thousands of people.
func (h *Handler) ComposeAnnouncement(ctx context.Context, in AnnouncementInput) (*notifications.Announcement, error) {
	if h.announcements == nil {
		return nil, ErrAnnouncementsUnavailable
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
		return nil, announcementInvalid("title and body are required")
	}
	for _, c := range in.Channels {
		if !notifications.ValidChannel(c) {
			return nil, announcementInvalid("channels must be drawn from whatsapp, sms, email, push (a portal banner is show_in_portal)")
		}
	}
	// An announcement addressed to nothing would report success having
	// reached nobody. Checked here as well as by
	// chk_announcement_has_destination so the caller gets a readable reason.
	if len(in.Channels) == 0 && !in.ShowInPortal {
		return nil, announcementInvalid("an announcement needs at least one channel or show_in_portal")
	}
	if len(in.SubscriberIDs) > 0 {
		if in.SegmentFranchiseID != nil || in.SegmentPlanID != nil || in.SegmentStatus != nil ||
			in.SegmentGroupID != nil {
			return nil, announcementInvalid("subscriber_ids cannot be combined with segment_franchise_id/segment_plan_id/segment_status/segment_group_id")
		}
		if len(in.SubscriberIDs) > maxBulkSubscribers {
			return nil, announcementInvalid(fmt.Sprintf("subscriber_ids must not exceed %d per announcement", maxBulkSubscribers))
		}
	}
	class := in.Class
	if class == "" {
		class = "marketing" // so DND opt-out is honoured by default
	}
	if !notifications.ValidClass(class) {
		return nil, announcementInvalid("class must be marketing or transactional")
	}

	created, err := h.announcements.CreateAnnouncement(ctx, notifications.Announcement{
		Title: in.Title, Body: in.Body, Channels: in.Channels, Class: class,
		SegmentFranchiseID: in.SegmentFranchiseID, SegmentPlanID: in.SegmentPlanID,
		SegmentStatus: in.SegmentStatus, SegmentGroupID: in.SegmentGroupID,
		ShowInPortal:  in.ShowInPortal,
		SubscriberIDs: in.SubscriberIDs,
		CreatedBy:     middleware.SubjectFromContext(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("api: create announcement: %w", err)
	}
	middleware.Audit(ctx, "announcement.create", strconv.Itoa(created.ID), map[string]any{
		"channels": created.Channels, "class": created.Class,
	})
	return created, nil
}

// SendResult is the outcome of broadcasting a draft.
type SendResult struct {
	Recipients    int
	TasksEnqueued int
	// TasksFailed counts messages that could not be queued and so will not
	// be delivered. Non-zero only on a partial failure; a broadcast where
	// every message failed is ErrAnnouncementQueueDown instead.
	TasksFailed int
}

// BroadcastAnnouncement claims a draft and fans it out. Errors:
// notifications.ErrAnnouncementNotDraft when it was already sent (including
// a concurrent second click), ErrAnnouncementNotFound,
// ErrAnnouncementDispatchDown, or a wrapped store failure.
func (h *Handler) BroadcastAnnouncement(ctx context.Context, id int) (*SendResult, error) {
	if h.announcements == nil || h.tasks == nil {
		return nil, ErrAnnouncementDispatchDown
	}
	existing, err := h.announcements.GetAnnouncement(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("api: announcement lookup: %w", err)
	}
	if existing == nil {
		return nil, ErrAnnouncementNotFound
	}
	// Atomic claim: only one caller can move a draft to 'sending', so a
	// double-click cannot broadcast the same message twice (MDS §4.17).
	claimed, err := h.announcements.ClaimAnnouncementForSending(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("api: claim announcement: %w", err)
	}
	if claimed == nil {
		return nil, notifications.ErrAnnouncementNotDraft
	}
	recipients, err := h.announcements.ListSegmentSubscriberIDs(ctx, id,
		claimed.SegmentFranchiseID, claimed.SegmentPlanID, claimed.SegmentStatus, claimed.SegmentGroupID)
	if err != nil {
		// The claim already moved it out of draft; mark it failed so it is
		// visibly stuck rather than silently sitting in 'sending' forever.
		h.finishAnnouncement(ctx, id, notifications.AnnouncementFailed, 0)
		return nil, fmt.Errorf("%w: %v", errAnnouncementSegment, err)
	}
	enqueued, failed := h.fanOutAnnouncement(claimed, recipients)
	// A portal-only announcement legitimately enqueues nothing, so 'sent'
	// with zero recipients is a correct outcome, not a failure. What is a
	// failure is having messages to queue and queuing none of them.
	if failed > 0 && enqueued == 0 {
		h.finishAnnouncement(ctx, id, notifications.AnnouncementFailed, 0)
		return nil, fmt.Errorf("%w: %d message(s)", ErrAnnouncementQueueDown, failed)
	}
	h.finishAnnouncement(ctx, id, notifications.AnnouncementSent, enqueued)

	notifications.AnnouncementsSentTotal.Inc()
	middleware.Audit(ctx, "announcement.send", strconv.Itoa(id), map[string]any{
		"recipients": len(recipients), "tasks_enqueued": enqueued, "tasks_failed": failed,
	})
	return &SendResult{Recipients: len(recipients), TasksEnqueued: enqueued, TasksFailed: failed}, nil
}

// AnnouncementList lists announcements, optionally by status.
func (h *Handler) AnnouncementList(ctx context.Context, status *string) ([]notifications.Announcement, error) {
	if h.announcements == nil {
		return nil, ErrAnnouncementsUnavailable
	}
	return h.announcements.ListAnnouncements(ctx, status)
}
