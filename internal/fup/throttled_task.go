package fup

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// TemplateFUPThrottled is the WhatsApp template used once a FUP throttle has
// actually been applied. It is a template identifier, not a credential.
//
// TMPL-002 ("fup_throttled") has been registered in
// internal/notifications' template map since that map was written; nothing
// ever sent it, which is the gap this file closes.
const TemplateFUPThrottled = "TMPL-002" //nolint:gosec // G101 false positive: template ID

// ThrottledPayload is the task payload for the throttle-applied notification.
type ThrottledPayload struct {
	SubscriberID int    `json:"subscriber_id"`
	Username     string `json:"username"`
	// ThrottleSpeed is the rate limit now in force ("10M/10M"), empty if
	// the plan named none.
	ThrottleSpeed string `json:"throttle_speed"`
}

// ThrottledHandler dispatches the notification telling a subscriber their
// speed has been reduced.
//
// FR-NOTIF-003 requires this on throttle application, and FR-FUP-005 adds
// that it must carry the reason and how to restore full speed. Until now
// only the 80% warning existed: a subscriber was told they were approaching
// their quota and then, on crossing it, silently slowed down with no
// message at all - which is exactly the call the support desk then
// receives.
//
// FR: FR-NOTIF-003, FR-FUP-005 | DDS §5.3, §5.8 | MDS §4.7
type ThrottledHandler struct {
	notifier WarningNotifier
}

// NewThrottledHandler constructs a ThrottledHandler.
func NewThrottledHandler(n WarningNotifier) *ThrottledHandler {
	return &ThrottledHandler{notifier: n}
}

// ProcessTask implements jobqueue.Handler for TaskTypeFUPThrottled.
func (h *ThrottledHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p ThrottledPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// A malformed payload will never become valid on retry.
		return fmt.Errorf("fup throttled: unmarshal payload: %w: %w", err, jobqueue.SkipRetry)
	}
	if h.notifier == nil {
		return fmt.Errorf("fup throttled: notifier not configured")
	}

	// The speed is stated rather than left implicit: "your speed has been
	// reduced" without saying to what is the message that generates a
	// support call instead of preventing one. A plan with no throttle
	// string still notifies - the subscriber was still slowed - but says so
	// in general terms rather than printing an empty speed.
	speed := p.ThrottleSpeed
	if speed == "" {
		speed = "a reduced speed"
	}
	vars := []string{p.Username, speed}

	if err := h.notifier.Notify(ctx, p.SubscriberID, TemplateFUPThrottled, "fup_throttle_applied", vars); err != nil {
		return fmt.Errorf("fup throttled: dispatch to sub %d: %w", p.SubscriberID, err)
	}
	return nil
}

// ThrottledTaskID returns the task ID that makes the throttle notification
// idempotent for a given subscriber and quota cycle.
//
// Keyed the same way the 80% warning is: one message per subscriber per
// quota threshold. Without it the 10-second scan loop would re-notify on
// every pass for as long as SetFUPActive had not yet committed.
func ThrottledTaskID(subscriberID int, threshold int64) string {
	return fmt.Sprintf("fupthrottled-%d-%d", subscriberID, threshold)
}
