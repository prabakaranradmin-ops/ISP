// Package alerting delivers operational alerts to an external on-call
// system.
//
// It exists because radiusd previously logged, on every start, that
// "PagerDuty delivery is not implemented — alerts go to logs only" while
// PAGERDUTY_ROUTING_KEY sat in the config as though it did something. An
// operator who set that key would reasonably believe someone gets woken
// when the dead-letter queue fills. Nobody did.
package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// eventsAPIURL is PagerDuty's Events API v2 enqueue endpoint.
const eventsAPIURL = "https://events.pagerduty.com/v2/enqueue"

// PagerDuty delivers alerts through the Events API v2.
//
// Deliberately not a wrapper around a vendor SDK: this sends one small JSON
// document to one endpoint, and a dependency for that would be more code to
// audit and pin than the request it replaces.
type PagerDuty struct {
	routingKey string
	source     string
	client     *http.Client
	url        string // overridable so tests can point at a local server
}

// NewPagerDuty constructs a client. source identifies this deployment in the
// alert — PagerDuty shows it on the incident, and "which box is this?" is
// the first question anyone asks at 3am.
func NewPagerDuty(routingKey, source string) *PagerDuty {
	return &PagerDuty{
		routingKey: routingKey,
		source:     source,
		// A short timeout on purpose. This is called from a monitor loop,
		// and an alerting system that is itself slow or down must not stall
		// the thing trying to report a problem.
		client: &http.Client{Timeout: 10 * time.Second},
		url:    eventsAPIURL,
	}
}

// SetURL overrides the endpoint. For tests.
func (p *PagerDuty) SetURL(u string) { p.url = u }

// event is the Events API v2 payload shape.
type event struct {
	RoutingKey  string       `json:"routing_key"`
	EventAction string       `json:"event_action"`
	DedupKey    string       `json:"dedup_key,omitempty"`
	Payload     eventPayload `json:"payload"`
}

type eventPayload struct {
	Summary   string `json:"summary"`
	Source    string `json:"source"`
	Severity  string `json:"severity"`
	Component string `json:"component,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	// CustomDetails carries the alert's own detail verbatim, so the incident
	// shows the count or error that triggered it rather than only a summary
	// line someone then has to go and correlate against logs.
	CustomDetails any `json:"custom_details,omitempty"`
}

// Trigger sends one alert, satisfying fup.Alerter.
//
// The signature has no error return because that is the interface the
// monitors were built against, and widening it would push a decision onto
// every caller that none of them can act on: a monitor that cannot reach
// PagerDuty has no better fallback than the log line Trigger already
// writes. Failures are surfaced through the delivery counter and the error
// log instead.
func (p *PagerDuty) Trigger(eventName string, detail any) {
	if p == nil || p.routingKey == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.send(ctx, eventName, detail); err != nil {
		alertDeliveryFailures.Inc()
		logError(err, eventName)
		return
	}
	alertsDelivered.Inc()
}

func (p *PagerDuty) send(ctx context.Context, eventName string, detail any) error {
	ev := event{
		RoutingKey:  p.routingKey,
		EventAction: "trigger",
		// One dedup key per event type. PagerDuty then folds repeats into
		// the open incident instead of opening a new one each time, which
		// is the same problem the dead-letter monitor's own rate limiting
		// solves one layer up — belt and braces, because the cost of
		// getting it wrong is an on-call rota that stops reading alerts.
		DedupKey: fmt.Sprintf("isp-bss/%s/%s", p.source, eventName),
		Payload: eventPayload{
			Summary:       fmt.Sprintf("%s: %v", eventName, detail),
			Source:        p.source,
			Severity:      "error",
			Component:     "isp-bss",
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			CustomDetails: detail,
		},
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("alerting: marshal event: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("alerting: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("alerting: post to PagerDuty: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// Events API v2 answers 202 on success. Anything else is reported with
	// its status rather than swallowed: a routing key that has been revoked
	// returns 4xx forever, and an alerting path that fails silently is
	// indistinguishable from one that is working.
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("alerting: PagerDuty returned %s", resp.Status)
	}
	return nil
}
