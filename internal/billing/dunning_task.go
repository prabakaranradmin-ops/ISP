package billing

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
)

// DunningNotifier is the notification surface the handler depends on. It is
// satisfied by notifications.Dispatcher, which is not imported here: billing
// is a dependency of the notification wiring, not the other way round, and an
// interface keeps that direction intact.
type DunningNotifier interface {
	Notify(ctx context.Context, subscriberID int, templateID, triggerEvent string, vars []string) error
}

// DunningNoticeHandler sends the message for a dunning stage.
//
// FR: FR-NOTIF-001 (reminders at T-7d/T-3d/T-1d), FR-NOTIF-005 (suspension)
type DunningNoticeHandler struct {
	notifier DunningNotifier
}

// NewDunningNoticeHandler constructs a DunningNoticeHandler.
func NewDunningNoticeHandler(n DunningNotifier) *DunningNoticeHandler {
	return &DunningNoticeHandler{notifier: n}
}

// ProcessTask implements jobqueue.Handler for TaskTypeDunningNotice.
func (h *DunningNoticeHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p DunningNoticePayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		// A malformed payload will never become valid on retry.
		return fmt.Errorf("dunning notice: unmarshal payload: %w: %w", err, jobqueue.SkipRetry)
	}
	if h.notifier == nil {
		return fmt.Errorf("dunning notice: notifier not configured")
	}

	// The trigger event is the stage itself, so notification_log records which
	// rung of the ladder produced the message rather than a generic "dunning".
	triggerEvent := "dunning_" + string(p.State)
	vars := []string{p.Username, fmt.Sprintf("%d", p.DaysOverdue)}

	if err := h.notifier.Notify(ctx, p.SubscriberID, p.TemplateID, triggerEvent, vars); err != nil {
		return fmt.Errorf("dunning notice: dispatch to sub %d: %w", p.SubscriberID, err)
	}
	return nil
}

// PaymentReceiptHandler acknowledges a payment, and where that payment
// restored a suspended account, tells the subscriber they are back online.
//
// FR: FR-NOTIF-004 (receipt), FR-NOTIF-006 (restoration)
type PaymentReceiptHandler struct {
	notifier DunningNotifier
}

// NewPaymentReceiptHandler constructs a PaymentReceiptHandler.
func NewPaymentReceiptHandler(n DunningNotifier) *PaymentReceiptHandler {
	return &PaymentReceiptHandler{notifier: n}
}

// PaymentReceiptPayload is the task payload for a payment acknowledgement.
type PaymentReceiptPayload struct {
	SubscriberID int    `json:"subscriber_id"`
	Username     string `json:"username"`
	Amount       string `json:"amount"`
	NewBalance   string `json:"new_balance"`
	// WasSuspended records that the subscriber was cut off when the money
	// arrived. It does not change the message — the receipt says only that
	// the payment landed — and exists so the notification log distinguishes
	// a routine top-up from one that is about to trigger a restoration.
	//
	// It was previously named Restored and set the trigger event to
	// "service_restored", which claimed something that had not happened:
	// the webhook only credits a wallet, and it is the renewal scanner,
	// minutes later, that actually puts the subscriber back on. A suspended
	// subscriber was being told they were restored while still cut off, and
	// then told nothing when service genuinely returned. The restoration
	// message is now sent from where restoration occurs.
	WasSuspended bool `json:"was_suspended"`
}

// ProcessTask implements jobqueue.Handler for TaskTypePaymentReceipt.
func (h *PaymentReceiptHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p PaymentReceiptPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("payment receipt: unmarshal payload: %w: %w", err, jobqueue.SkipRetry)
	}
	if h.notifier == nil {
		return fmt.Errorf("payment receipt: notifier not configured")
	}

	// One trigger event, because there is one thing to report: money arrived.
	// Whether it also restores service is decided later by the renewal
	// scanner, which sends its own message when it does.
	triggerEvent := "payment_received"
	if p.WasSuspended {
		triggerEvent = "payment_received_while_suspended"
	}
	vars := []string{p.Username, p.Amount, p.NewBalance}

	if err := h.notifier.Notify(ctx, p.SubscriberID, TemplatePaymentReceived, triggerEvent, vars); err != nil {
		return fmt.Errorf("payment receipt: dispatch to sub %d: %w", p.SubscriberID, err)
	}
	return nil
}

// TaskTypePaymentReceipt carries a payment acknowledgement.
const TaskTypePaymentReceipt = "notif:payment_receipt"

// ServiceRestoredHandler tells a subscriber their service is back on
// (FR-NOTIF-006).
//
// Separate from PaymentReceiptHandler because the two answer different
// questions at different moments. The receipt fires when money arrives; this
// fires when the renewal scanner has actually charged the cycle and put the
// subscriber back on the network, which can be a quarter of an hour later and
// may not happen at all if the payment did not cover the cycle.
type ServiceRestoredHandler struct {
	notifier DunningNotifier
}

// NewServiceRestoredHandler constructs a ServiceRestoredHandler.
func NewServiceRestoredHandler(n DunningNotifier) *ServiceRestoredHandler {
	return &ServiceRestoredHandler{notifier: n}
}

// ProcessTask implements jobqueue.Handler for TaskTypeServiceRestored.
func (h *ServiceRestoredHandler) ProcessTask(ctx context.Context, t *jobqueue.Task) error {
	var p ServiceRestoredPayload
	if err := json.Unmarshal(t.Payload(), &p); err != nil {
		return fmt.Errorf("service restored notice: unmarshal payload: %w: %w", err, jobqueue.SkipRetry)
	}
	if h.notifier == nil {
		return fmt.Errorf("service restored notice: notifier not configured")
	}
	vars := []string{p.Username, p.PlanName, p.ValidUntil}
	if err := h.notifier.Notify(ctx, p.SubscriberID, TemplateServiceRestored, "service_restored", vars); err != nil {
		return fmt.Errorf("service restored notice: dispatch to sub %d: %w", p.SubscriberID, err)
	}
	return nil
}
