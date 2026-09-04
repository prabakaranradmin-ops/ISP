package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// NotificationStore persists notification dispatch and delivery state.
// Satisfies notifications.NotifQuerier.
type NotificationStore struct{ pool dbPool }

var _ notifications.NotifQuerier = (*NotificationStore)(nil)

// GetSubscriber loads the DND flag and destination number the dispatcher needs.
func (s *NotificationStore) GetSubscriber(ctx context.Context, subscriberID int) (*notifications.Subscriber, error) {
	const q = `SELECT id, mobile_number, COALESCE(email, ''), dnd_opt_out FROM subscribers WHERE id = $1`

	var sub notifications.Subscriber
	err := s.pool.QueryRow(ctx, q, subscriberID).Scan(&sub.ID, &sub.MobileNumber, &sub.Email, &sub.DndOptOut)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get subscriber %d for notification: %w", subscriberID, err)
	}
	return &sub, nil
}

// CreateNotificationLog records a dispatch attempt, including a DND suppression.
//
// template_id is nullable and carries an FK to notification_templates. An
// empty id — a system event with no template behind it — still logs NULL. An
// id that is set but unknown now fails the insert.
//
// It previously resolved through (SELECT id FROM notification_templates
// WHERE id = $3), which turns "no such template" into NULL and inserts
// happily. notification_templates was empty, so that silently applied to
// every notification this system had ever sent: the audit log could not say
// which template any of them used, and the column's own comment ("NULLABLE:
// system events may have none") made the result look deliberate.
//
// That subquery was also the only thing standing between the ids the code
// sends and the templates the spec defines, and it dissolved every
// disagreement. The code had drifted to sending TMPL-005 for a payment
// receipt, which the spec assigns to hard suspension — a mismatch this
// insert would now reject on the first attempt.
//
// The old reasoning was that losing the audit row is worse than losing the
// attribution. It reads well and is wrong in this direction: an audit row
// that cannot say what was sent is not much of an audit row, and the
// failure it was protecting against is a bug in the caller, which is exactly
// what should be surfaced rather than absorbed.
func (s *NotificationStore) CreateNotificationLog(ctx context.Context, entry notifications.NotificationLog) error {
	const q = `
		INSERT INTO notification_log (
			subscriber_id, channel, template_id, triggered_by_event,
			sent_at, delivery_status, provider_message_id, failure_reason
		) VALUES (
			$1, $2, NULLIF($3,''),
			$4, COALESCE($5, NOW()), $6, NULLIF($7,''), NULLIF($8,'')
		)`

	status := entry.DeliveryStatus
	if status == "" {
		status = "sent"
	}
	var sentAt any
	if !entry.SentAt.IsZero() {
		sentAt = entry.SentAt
	}

	if _, err := s.pool.Exec(ctx, q,
		entry.SubscriberID, entry.Channel, entry.TemplateID, entry.TriggeredByEvent,
		sentAt, status, entry.ProviderMessageID, entry.FailureReason,
	); err != nil {
		return fmt.Errorf("db: create notification log for subscriber %d: %w", entry.SubscriberID, err)
	}
	return nil
}

// ListPushTokens returns every device token registered for a subscriber.
//
// An empty result is a normal state — most subscribers never install the app
// — so it is returned as an empty slice rather than ErrNotFound.
func (s *NotificationStore) ListPushTokens(ctx context.Context, subscriberID int) ([]string, error) {
	const q = `SELECT token FROM subscriber_push_tokens WHERE subscriber_id = $1 ORDER BY last_seen_at DESC`

	rows, err := s.pool.Query(ctx, q, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: list push tokens for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, fmt.Errorf("db: scan push token: %w", err)
		}
		out = append(out, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate push tokens: %w", err)
	}
	return out, nil
}

// RegisterPushToken records or refreshes a device's push token.
//
// Upsert on the unique token: the same physical device re-registering after
// a reinstall must update its owner and last-seen time rather than
// accumulate rows that would each receive a copy of every notification.
func (s *NotificationStore) RegisterPushToken(ctx context.Context, subscriberID int, token, platform string) error {
	const q = `
		INSERT INTO subscriber_push_tokens (subscriber_id, token, platform)
		VALUES ($1, $2, $3)
		ON CONFLICT (token) DO UPDATE
		SET subscriber_id = EXCLUDED.subscriber_id,
		    platform      = EXCLUDED.platform,
		    last_seen_at  = NOW()`

	if _, err := s.pool.Exec(ctx, q, subscriberID, token, platform); err != nil {
		return fmt.Errorf("db: register push token for subscriber %d: %w", subscriberID, err)
	}
	return nil
}

// UpdateDeliveryStatus advances a logged notification to the status reported by
// the provider's delivery callback.
//
// Meta can deliver callbacks out of order, so a status is only allowed to move
// forward: a late 'sent' must not overwrite a 'read' that already arrived.
func (s *NotificationStore) UpdateDeliveryStatus(ctx context.Context, providerMessageID, status string) error {
	if providerMessageID == "" {
		return fmt.Errorf("db: update delivery status: empty provider_message_id")
	}
	const q = `
		UPDATE notification_log
		SET delivery_status = $2
		WHERE provider_message_id = $1
		  AND CASE $2
		        WHEN 'sent'      THEN 0
		        WHEN 'delivered' THEN 1
		        WHEN 'read'      THEN 2
		        WHEN 'failed'    THEN 3
		        ELSE 0
		      END
		      >
		      CASE delivery_status
		        WHEN 'sent'      THEN 0
		        WHEN 'delivered' THEN 1
		        WHEN 'read'      THEN 2
		        WHEN 'failed'    THEN 3
		        ELSE 0
		      END`

	if _, err := s.pool.Exec(ctx, q, providerMessageID, status); err != nil {
		return fmt.Errorf("db: update delivery status for %s: %w", providerMessageID, err)
	}
	// A zero-row result means the callback was a duplicate or arrived out of
	// order. Both are normal for Meta's at-least-once delivery, so neither is
	// an error the webhook should retry on.
	return nil
}
