// Package notifications implements DND-aware multi-channel dispatching and
// the WhatsApp Business API template sender.
//
// FR: FR-NOTIF-001..011 | DDS §5.8
package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/pkg/validate"
)

var (
	notificationDispatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "notification_dispatch_total",
		Help: "Notifications dispatched by channel, template, and status",
	}, []string{"channel", "template_id", "status"})

	// MissingDestinationTotal counts subscribers a channel could not reach
	// because they have no address or token for it. This is the number that
	// says whether a channel is worth paying for — a push provider reaching
	// 4% of the base is a different decision from one reaching 60%.
	MissingDestinationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "notifications_missing_destination_total",
		Help: "Notifications not sent because the subscriber has no destination on that channel",
	}, []string{"channel"})
)

// NotificationLog is the minimal representation for persistence.
type NotificationLog struct {
	SubscriberID      int
	Channel           string
	TemplateID        string
	TriggeredByEvent  string
	ProviderMessageID string
	DeliveryStatus    string
	// FailureReason records why a delivery did not happen — an unreachable
	// subscriber (no email address, no device token) as much as a provider
	// rejection, so "we never sent it" and "we sent it and it bounced" stay
	// distinguishable in the log.
	FailureReason string
	SentAt        time.Time
}

// NotificationTask is the input for the Dispatcher.
type NotificationTask struct {
	SubscriberID int
	Channel      string // whatsapp | sms | email | push
	TemplateID   string
	TriggerEvent string
	Class        string // marketing | transactional
	Variables    []string
	ToPhone      string // E.164
	// Subject is the email subject line and the push notification heading.
	// WhatsApp and SMS have no equivalent and ignore it.
	Subject string
}

// Subscriber holds the fields needed for dispatch decisions.
type Subscriber struct {
	ID           int
	MobileNumber string // E.164
	Email        string // empty = unreachable by email
	DndOptOut    bool
}

// NotifQuerier is the DB interface required by the dispatcher.
type NotifQuerier interface {
	GetSubscriber(ctx context.Context, subscriberID int) (*Subscriber, error)
	CreateNotificationLog(ctx context.Context, entry NotificationLog) error
	// UpdateDeliveryStatus advances a logged notification to the status reported
	// by the provider's delivery callback.
	UpdateDeliveryStatus(ctx context.Context, providerMessageID, status string) error
	// ListPushTokens returns every device token registered for a subscriber.
	// An empty slice is a normal state, not an error: most subscribers never
	// install the app.
	ListPushTokens(ctx context.Context, subscriberID int) ([]string, error)
}

// templateNames maps internal template IDs to the template names registered
// with Meta. Meta's API addresses templates by name, not by our internal ID.
var templateNames = map[string]string{
	"TMPL-001": "fup_warning_80pct",
	"TMPL-002": "fup_throttled",
	"TMPL-003": "payment_reminder",
	"TMPL-004": "service_suspended",
	"TMPL-005": "payment_received",
	"TMPL-006": "plan_expiring",
	"TMPL-007": "promotional_offer",
	"TMPL-008": "ticket_update",
}

// TemplateNameFor resolves the Meta template name for an internal template ID.
// Unknown IDs fall through unchanged so a newly registered template still sends.
func TemplateNameFor(templateID string) string {
	if name, ok := templateNames[templateID]; ok {
		return name
	}
	return templateID
}

// TemplateMessage is the WhatsApp template send request.
type TemplateMessage struct {
	SubscriberID int
	ToPhoneE164  string
	TemplateName string
	TemplateID   string
	TriggerEvent string
	Variables    []string
}

// WhatsAppClient sends templates via the Meta Cloud API.
//
// FR: FR-NOTIF-001..007 | DDS §5.8
type WhatsAppClient struct {
	phoneNumberID string
	accessToken   string
	baseURL       string
	http          *http.Client
	db            NotifQuerier
}

// NewWhatsAppClient constructs a WhatsAppClient.
func NewWhatsAppClient(phoneNumberID, accessToken string, db NotifQuerier) *WhatsAppClient {
	return &WhatsAppClient{
		phoneNumberID: phoneNumberID,
		accessToken:   accessToken,
		baseURL:       "https://graph.facebook.com/v17.0",
		http:          &http.Client{Timeout: 10 * time.Second},
		db:            db,
	}
}

// SetBaseURL overrides the Meta Graph API endpoint. Used to point the client at
// a stub server in tests; production callers should leave the default in place.
func (c *WhatsAppClient) SetBaseURL(u string) {
	c.baseURL = u
}

// SendTemplate dispatches a WhatsApp template message.
func (c *WhatsAppClient) SendTemplate(ctx context.Context, req TemplateMessage) error {
	if !validate.E164(req.ToPhoneE164) {
		return fmt.Errorf("notifications: %q is not a valid E.164 phone number", req.ToPhoneE164)
	}

	templateName := req.TemplateName
	if templateName == "" {
		templateName = TemplateNameFor(req.TemplateID)
	}
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                req.ToPhoneE164,
		"type":              "template",
		"template": map[string]any{
			"name":       templateName,
			"language":   map[string]string{"code": "en"},
			"components": buildComponents(req.Variables),
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notifications: marshal payload: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/%s/messages", c.baseURL, c.phoneNumberID),
		bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notifications: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("notifications: whatsapp send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= http.StatusBadRequest {
		notificationDispatchTotal.WithLabelValues("whatsapp", req.TemplateID, "failed").Inc()

		// Log the attempt, not just the successes. FR-NOTIF-009 asks for a row
		// per dispatch attempt, and until this was added a provider rejection
		// left no trace at all: an operator reading notification_log could not
		// tell "we never tried to warn this subscriber" from "we tried and Meta
		// refused", which are very different answers when someone was suspended
		// without warning.
		if err := c.db.CreateNotificationLog(ctx, NotificationLog{
			SubscriberID:     req.SubscriberID,
			Channel:          "whatsapp",
			TemplateID:       req.TemplateID,
			TriggeredByEvent: req.TriggerEvent,
			DeliveryStatus:   "failed",
			SentAt:           time.Now(),
		}); err != nil {
			log.Warn().Err(err).Msg("notifications: failed to persist failed-dispatch notification_log")
		}
		return fmt.Errorf("notifications: whatsapp API returned HTTP %d", resp.StatusCode)
	}

	var result struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("notifications: decode response: %w", err)
	}

	providerMsgID := ""
	if len(result.Messages) > 0 {
		providerMsgID = result.Messages[0].ID
	}

	// Persist to notification_log (FR-NOTIF-009)
	if err := c.db.CreateNotificationLog(ctx, NotificationLog{
		SubscriberID:      req.SubscriberID,
		Channel:           "whatsapp",
		TemplateID:        req.TemplateID,
		TriggeredByEvent:  req.TriggerEvent,
		ProviderMessageID: providerMsgID,
		DeliveryStatus:    "sent",
		SentAt:            time.Now(),
	}); err != nil {
		log.Warn().Err(err).Msg("notifications: failed to persist notification_log")
	}

	notificationDispatchTotal.WithLabelValues("whatsapp", req.TemplateID, "sent").Inc()
	return nil
}

// buildComponents converts a flat variable slice into WhatsApp component parameters.
func buildComponents(vars []string) []map[string]any {
	if len(vars) == 0 {
		return nil
	}
	params := make([]map[string]any, len(vars))
	for i, v := range vars {
		params[i] = map[string]any{"type": "text", "text": v}
	}
	return []map[string]any{{"type": "body", "parameters": params}}
}
