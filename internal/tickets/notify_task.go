// Package tickets carries the background task that tells a subscriber their
// support ticket's status changed (FR-NOTIF-007).
//
// TMPL-008 (ticket_update) was seeded and fully wired for delivery, but
// nothing ever enqueued it: db.TicketStore.UpdateTicketAdmin is a bare
// UPDATE, called from both the JSON API and the operations console, and
// neither told the subscriber. Harmless while only an API call could change
// a status; real once a CSR resolves a ticket from the console and the
// customer is never told.
//
// The ticket domain itself lives split across internal/db, internal/api and
// internal/staffui, so this package holds only the notification side of a
// status change — mirroring internal/billing/dunning_task.go and
// internal/fup/warning_task.go, which do the same for their own domains.
//
// FR: FR-NOTIF-007 | DDS §5.8
package tickets

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

const (
	// TaskTypeTicketUpdate carries a ticket status-change notification.
	TaskTypeTicketUpdate = "notif:ticket_update"

	// TemplateTicketUpdate is TMPL-008 in the seed data.
	TemplateTicketUpdate = "TMPL-008" //nolint:gosec // template id, not a credential
)

// UpdatePayload is the task payload for a ticket status-change notification.
type UpdatePayload struct {
	SubscriberID int    `json:"subscriber_id"`
	Username     string `json:"username"`
	TicketID     int    `json:"ticket_id"`
	Status       string `json:"status"`
}

// Notifier is the notification surface the handler depends on. It is
// satisfied by notifications.Dispatcher, which is not imported here for the
// same reason internal/billing and internal/fup don't: this package is a
// dependency of the notification wiring, not the other way round.
type Notifier interface {
	Notify(ctx context.Context, subscriberID int, templateID, triggerEvent string, vars []string) error
}

// UpdateHandler dispatches the ticket status-change notification.
type UpdateHandler struct {
	notifier Notifier
}

// NewUpdateHandler constructs an UpdateHandler.
func NewUpdateHandler(n Notifier) *UpdateHandler {
	return &UpdateHandler{notifier: n}
}

// ProcessTask implements jobqueue.Handler for TaskTypeTicketUpdate.
func (h *UpdateHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p UpdatePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// A malformed payload will never become valid on retry.
		return fmt.Errorf("ticket update: unmarshal payload: %w: %w", err, jobqueue.SkipRetry)
	}
	if h.notifier == nil {
		return fmt.Errorf("ticket update: notifier not configured")
	}

	vars := []string{p.Username, fmt.Sprintf("%d", p.TicketID), p.Status}
	if err := h.notifier.Notify(ctx, p.SubscriberID, TemplateTicketUpdate, "ticket_update", vars); err != nil {
		return fmt.Errorf("ticket update: dispatch to sub %d: %w", p.SubscriberID, err)
	}
	return nil
}
