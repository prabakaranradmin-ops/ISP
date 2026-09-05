package fup

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// TemplateFUPWarning is the WhatsApp template used for the 80% quota warning.
// It is a template identifier, not a credential.
const TemplateFUPWarning = "TMPL-001" //nolint:gosec // G101 false positive: template ID

// WarningPayload is the task payload for 80% FUP warnings.
type WarningPayload struct {
	SubscriberID int    `json:"subscriber_id"`
	Username     string `json:"username"`
	PctUsed      int    `json:"pct_used"`
}

// WarningNotifier is the notification surface the warning handler depends on.
// It is satisfied by notifications.Dispatcher.
type WarningNotifier interface {
	Notify(ctx context.Context, subscriberID int, templateID, triggerEvent string, vars []string, channels ...string) error
}

// ContactLookup resolves the phone number a warning should be delivered to.
type ContactLookup interface {
	GetSubscriberPhone(ctx context.Context, subscriberID int) (string, error)
}

// WarningHandler dispatches the FUP 80% warning notification.
//
// FR: FR-FUP-004, FR-NOTIF-005 | DDS §5.3, §5.8
type WarningHandler struct {
	notifier WarningNotifier
}

// NewWarningHandler constructs a WarningHandler.
func NewWarningHandler(n WarningNotifier) *WarningHandler {
	return &WarningHandler{notifier: n}
}

// ProcessTask implements jobqueue.Handler for TaskTypeFUPWarning.
func (h *WarningHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p WarningPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// A malformed payload will never become valid on retry.
		return fmt.Errorf("fup warning: unmarshal payload: %w: %w", err, jobqueue.SkipRetry)
	}
	if h.notifier == nil {
		return fmt.Errorf("fup warning: notifier not configured")
	}

	vars := []string{p.Username, fmt.Sprintf("%d%%", p.PctUsed)}
	if err := h.notifier.Notify(ctx, p.SubscriberID, TemplateFUPWarning, "fup_warning_80pct", vars); err != nil {
		return fmt.Errorf("fup warning: dispatch to sub %d: %w", p.SubscriberID, err)
	}
	return nil
}
