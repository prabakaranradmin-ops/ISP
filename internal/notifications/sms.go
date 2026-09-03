package notifications

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/maaransoft/isp-bss-oss/pkg/validate"
)

// MSG91Client implements SMSSender for the MSG91 SMS gateway.
//
// FR: FR-NOTIF-009 (SMS channel) | DDS §5.8
type MSG91Client struct {
	apiKey   string
	senderID string
	baseURL  string
	http     *http.Client
}

// msg91SendURL is MSG91's transactional send endpoint.
const msg91SendURL = "https://api.msg91.com/api/sendhttp.php"

// NewMSG91Client constructs an MSG91Client.
func NewMSG91Client(apiKey, senderID string) *MSG91Client {
	return &MSG91Client{apiKey: apiKey, senderID: senderID, baseURL: msg91SendURL, http: &http.Client{}}
}

// SetBaseURL overrides the MSG91 endpoint, matching WhatsAppClient's own
// setter. Used to point the client at cmd/mockgateway during development so
// the real request-building and response-handling code still runs — a stub
// that replaced this client entirely would not catch a malformed request.
// Production callers leave the default in place.
func (c *MSG91Client) SetBaseURL(u string) { c.baseURL = u }

// SendSMS sends a plain-text SMS via MSG91.
func (c *MSG91Client) SendSMS(ctx context.Context, toPhone, message string) error {
	if !validate.E164(toPhone) {
		return fmt.Errorf("sms: %q is not a valid E.164 phone number", toPhone)
	}

	// Sanitise: strip leading '+' for MSG91 (expects 91XXXXXXXXXX format)
	mobile := strings.TrimPrefix(toPhone, "+")

	params := url.Values{}
	params.Set("authkey", c.apiKey)
	params.Set("mobiles", mobile)
	params.Set("message", message)
	params.Set("sender", c.senderID)
	params.Set("route", "4") // transactional route

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("sms: build request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sms: msg91 send: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		return fmt.Errorf("sms: msg91 returned HTTP %d", resp.StatusCode)
	}
	return nil
}
