package notifications

import (
	"context"
	"fmt"

	"github.com/rs/zerolog/log"
)

// Dispatcher routes notification tasks to the correct channel client
// after applying the DND suppression check (FR-NOTIF-008).
//
// FR: FR-NOTIF-001..011 | DDS §5.8
type Dispatcher struct {
	db        NotifQuerier
	whatsapp  *WhatsAppClient
	smsClient SMSSender
	email     EmailSender
	push      PushSender
}

// SMSSender is the interface for SMS gateway implementations.
type SMSSender interface {
	SendSMS(ctx context.Context, toPhone, message string) error
}

// NewDispatcher constructs a Dispatcher with the two channels that have
// always existed. Email and push are attached separately so existing call
// sites — and deployments that have bought neither — need no change.
func NewDispatcher(db NotifQuerier, wa *WhatsAppClient, sms SMSSender) *Dispatcher {
	return &Dispatcher{db: db, whatsapp: wa, smsClient: sms}
}

// SetEmailSender enables the email channel (FR-NOTIF-012).
func (d *Dispatcher) SetEmailSender(s EmailSender) { d.email = s }

// SetPushSender enables the push channel (FR-NOTIF-013).
func (d *Dispatcher) SetPushSender(s PushSender) { d.push = s }

// Dispatch applies DND suppression, then routes to the correct channel.
func (d *Dispatcher) Dispatch(ctx context.Context, task NotificationTask) error {
	sub, err := d.db.GetSubscriber(ctx, task.SubscriberID)
	if err != nil {
		return fmt.Errorf("notifications: get subscriber %d: %w", task.SubscriberID, err)
	}

	// DND suppression: only suppress marketing-class notifications (FR-NOTIF-008)
	if sub.DndOptOut && task.Class == "marketing" {
		log.Info().
			Int("subscriber_id", task.SubscriberID).
			Str("event", task.TriggerEvent).
			Msg("notification suppressed: DND opt-out")

		_ = d.db.CreateNotificationLog(ctx, NotificationLog{
			SubscriberID:     task.SubscriberID,
			Channel:          task.Channel,
			TemplateID:       task.TemplateID,
			TriggeredByEvent: task.TriggerEvent,
			DeliveryStatus:   "suppressed_dnd",
		})
		return nil // intentional suppression, not an error
	}

	switch task.Channel {
	case "whatsapp":
		if d.whatsapp == nil {
			return fmt.Errorf("notifications: WhatsApp client not configured")
		}
		toPhone := task.ToPhone
		if toPhone == "" {
			toPhone = sub.MobileNumber
		}
		return d.whatsapp.SendTemplate(ctx, TemplateMessage{
			SubscriberID: task.SubscriberID,
			ToPhoneE164:  toPhone,
			TemplateName: TemplateNameFor(task.TemplateID),
			TemplateID:   task.TemplateID,
			TriggerEvent: task.TriggerEvent,
			Variables:    task.Variables,
		})
	case "sms":
		if d.smsClient == nil {
			return fmt.Errorf("notifications: SMS client not configured")
		}
		toPhone := task.ToPhone
		if toPhone == "" {
			toPhone = sub.MobileNumber
		}
		if err := d.smsClient.SendSMS(ctx, toPhone, firstVariable(task)); err != nil {
			return err
		}
		// SMS previously returned here without logging, so FR-NOTIF-009's
		// "every outbound notification creates a log record" was quietly
		// untrue for this channel — WhatsApp logs its own (it has a provider
		// message id for the delivery callback), which is why the gap went
		// unnoticed. Closed while completing the dispatcher.
		return d.recordSent(ctx, task)

	case "email":
		if d.email == nil {
			return fmt.Errorf("notifications: email client not configured")
		}
		if sub.Email == "" {
			return d.recordUnreachable(ctx, task, "subscriber has no email address")
		}
		if err := d.email.SendEmail(ctx, sub.Email, task.Subject, firstVariable(task)); err != nil {
			return err
		}
		return d.recordSent(ctx, task)

	case "push":
		if d.push == nil {
			return fmt.Errorf("notifications: push client not configured")
		}
		tokens, err := d.db.ListPushTokens(ctx, task.SubscriberID)
		if err != nil {
			return fmt.Errorf("notifications: list push tokens for %d: %w", task.SubscriberID, err)
		}
		if len(tokens) == 0 {
			return d.recordUnreachable(ctx, task, "subscriber has no registered push tokens")
		}
		if err := d.push.SendPush(ctx, tokens, task.Subject, firstVariable(task)); err != nil {
			return err
		}
		return d.recordSent(ctx, task)

	default:
		return fmt.Errorf("notifications: unsupported channel %q", task.Channel)
	}
}

// recordUnreachable logs a subscriber the channel simply cannot reach — no
// email address, no registered device — and returns nil.
//
// Deliberately not an error: most subscribers will never install the app or
// supply an address, so returning one would push an ordinary state into
// the queue's retry-and-dead-letter path for a condition no retry can fix. This
// is the same judgment PoDHandler makes with SkipRetry when a subscriber has
// no live session.
func (d *Dispatcher) recordUnreachable(ctx context.Context, task NotificationTask, reason string) error {
	log.Info().
		Int("subscriber_id", task.SubscriberID).
		Str("channel", task.Channel).
		Str("reason", reason).
		Msg("notification not deliverable on this channel")

	MissingDestinationTotal.WithLabelValues(task.Channel).Inc()

	return d.db.CreateNotificationLog(ctx, NotificationLog{
		SubscriberID:     task.SubscriberID,
		Channel:          task.Channel,
		TemplateID:       task.TemplateID,
		TriggeredByEvent: task.TriggerEvent,
		DeliveryStatus:   "failed",
		FailureReason:    reason,
	})
}

// recordSent writes the FR-NOTIF-009 log record for a delivered message.
//
// WhatsApp writes its own (it has a provider message id to record for the
// delivery callback); email and SMTP-style channels have no such id, so the
// row is written here.
func (d *Dispatcher) recordSent(ctx context.Context, task NotificationTask) error {
	return d.db.CreateNotificationLog(ctx, NotificationLog{
		SubscriberID:     task.SubscriberID,
		Channel:          task.Channel,
		TemplateID:       task.TemplateID,
		TriggeredByEvent: task.TriggerEvent,
		DeliveryStatus:   "sent",
	})
}

// firstVariable returns the message body a plain-text channel sends.
//
// Guarded because SMS previously indexed Variables[0] directly, which panics
// on a task carrying no variables — reachable from any future caller that
// forgets, and a panic in a notification worker takes the whole queue
// consumer down.
func firstVariable(task NotificationTask) string {
	if len(task.Variables) == 0 {
		return task.Subject
	}
	return task.Variables[0]
}

// Notify sends a transactional WhatsApp template to a subscriber, resolving the
// destination number from the subscriber record.
//
// It is the entry point used by task handlers, which know a subscriber ID
// and a template but not a phone number.
func (d *Dispatcher) Notify(ctx context.Context, subscriberID int, templateID, triggerEvent string, vars []string) error {
	return d.Dispatch(ctx, NotificationTask{
		SubscriberID: subscriberID,
		Channel:      "whatsapp",
		TemplateID:   templateID,
		TriggerEvent: triggerEvent,
		Class:        "transactional",
		Variables:    vars,
	})
}
