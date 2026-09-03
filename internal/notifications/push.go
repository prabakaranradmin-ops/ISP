package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Push channel via OneSignal — FR-NOTIF-013 | MDS §4.17.
//
// OneSignal rather than FCM/APNs directly (the FR allows either): a single
// REST endpoint covers both platforms, where talking to Apple and Google
// separately would mean two credential formats, two payload shapes and
// APNs' certificate rotation — none of which buys anything at this scale.

// ErrPushNotConfigured is returned when push credentials are absent.
var ErrPushNotConfigured = errors.New("notifications: push provider is not configured")

// PushSender is the interface the dispatcher depends on.
type PushSender interface {
	SendPush(ctx context.Context, tokens []string, title, body string) error
}

// OneSignalClient implements PushSender.
type OneSignalClient struct {
	appID   string
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewOneSignalClient constructs a OneSignalClient. Empty credentials leave
// it unconfigured, which SendPush reports rather than failing at startup.
func NewOneSignalClient(appID, apiKey string) *OneSignalClient {
	return &OneSignalClient{
		appID: appID, apiKey: apiKey,
		baseURL: "https://onesignal.com/api/v1/notifications",
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// SetBaseURL overrides the OneSignal endpoint. Same purpose as
// WhatsAppClient.SetBaseURL — point it at cmd/mockgateway in development so
// the real client code still runs. Production leaves the default.
func (c *OneSignalClient) SetBaseURL(u string) { c.baseURL = u }

// Configured reports whether this client can actually send.
func (c *OneSignalClient) Configured() bool {
	return c != nil && c.appID != "" && c.apiKey != ""
}

type oneSignalRequest struct {
	AppID            string            `json:"app_id"`
	IncludePlayerIDs []string          `json:"include_player_ids"`
	Headings         map[string]string `json:"headings"`
	Contents         map[string]string `json:"contents"`
}

// SendPush delivers one notification to every registered device token for a
// subscriber.
//
// All of a subscriber's tokens go in a single request rather than one call
// per device: OneSignal treats that as one notification to N recipients,
// which is both fewer round trips and — more importantly — one delivery
// outcome to log, matching the one notification_log row the caller writes.
func (c *OneSignalClient) SendPush(ctx context.Context, tokens []string, title, body string) error {
	if !c.Configured() {
		return ErrPushNotConfigured
	}
	if len(tokens) == 0 {
		// Caller-side bug: the dispatcher is expected to skip a subscriber
		// with no tokens rather than call this with an empty slice, since
		// OneSignal would reject it as a malformed request.
		return errors.New("notifications: push requires at least one device token")
	}

	payload, err := json.Marshal(oneSignalRequest{
		AppID:            c.appID,
		IncludePlayerIDs: tokens,
		Headings:         map[string]string{"en": title},
		Contents:         map[string]string{"en": body},
	})
	if err != nil {
		return fmt.Errorf("notifications: marshal push payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("notifications: build push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("notifications: push request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 300 {
		// Body is bounded: a provider error page should not be able to pull
		// an unbounded read into a log line.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512)) //nolint:errcheck
		return fmt.Errorf("notifications: push provider returned %d: %s", resp.StatusCode, string(snippet))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) //nolint:errcheck // drain for connection reuse
	return nil
}
