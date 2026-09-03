// Package billing — Razorpay Payment Links client for portal one-tap renewal.
//
// Payment Links, not the Orders API + Checkout.js, are used deliberately:
// Razorpay copies the notes set at link-creation time onto the resulting
// Payment entity when the customer pays. RazorpayWebhook (webhook_razorpay.go)
// depends on payment.entity.notes.subscriber_id to know whose wallet to
// credit — the Orders API does not propagate order notes onto payments the
// same way, so it cannot feed that handler without an extra order-lookup
// round trip.
//
// FR: POR-003 | DDS §5.6
package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/shopspring/decimal"
)

const razorpayPaymentLinksURL = "https://api.razorpay.com/v1/payment_links"

// RazorpayClient creates Razorpay Payment Links. It implements
// portal.RazorpayOrderCreator.
type RazorpayClient struct {
	keyID      string
	keySecret  string
	baseURL    string
	httpClient *http.Client
}

// NewRazorpayClient constructs a RazorpayClient authenticating with the given
// API key ID/secret pair (Razorpay dashboard → Settings → API Keys).
func NewRazorpayClient(keyID, keySecret string) *RazorpayClient {
	return &RazorpayClient{
		keyID:      keyID,
		keySecret:  keySecret,
		baseURL:    razorpayPaymentLinksURL,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// SetBaseURL overrides the Payment Links endpoint. Same purpose as
// notifications.WhatsAppClient.SetBaseURL: point it at cmd/mockgateway
// during development so the request this client builds, and its handling of
// the response, are both still exercised. A stub that replaced the client
// outright would test neither. Production leaves the default.
func (c *RazorpayClient) SetBaseURL(u string) { c.baseURL = u }

type razorpayPaymentLinkRequest struct {
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	Description    string            `json:"description"`
	Notes          map[string]string `json:"notes"`
	Notify         razorpayNotify    `json:"notify"`
	ReminderEnable bool              `json:"reminder_enable"`
}

type razorpayNotify struct {
	SMS   bool `json:"sms"`
	Email bool `json:"email"`
}

type razorpayPaymentLinkResponse struct {
	ID       string `json:"id"`
	ShortURL string `json:"short_url"`
	Error    *struct {
		Description string `json:"description"`
	} `json:"error"`
}

// CreateOrder creates a Razorpay Payment Link for a subscriber renewal and
// returns (link ID, short deeplink URL, error).
func (c *RazorpayClient) CreateOrder(ctx context.Context, subscriberID int, amount decimal.Decimal) (string, string, error) {
	paise := amount.Round(2).Mul(decimal.NewFromInt(100)).IntPart()
	if paise <= 0 {
		return "", "", fmt.Errorf("billing: razorpay payment link amount must be positive, got %s", amount)
	}

	reqBody := razorpayPaymentLinkRequest{
		Amount:      paise,
		Currency:    "INR",
		Description: "Plan renewal",
		Notes:       map[string]string{"subscriber_id": strconv.Itoa(subscriberID)},
		Notify:      razorpayNotify{SMS: false, Email: false},
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("billing: marshal razorpay request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(buf))
	if err != nil {
		return "", "", fmt.Errorf("billing: create razorpay request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(c.keyID, c.keySecret)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("billing: razorpay request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", "", fmt.Errorf("billing: read razorpay response: %w", err)
	}

	var out razorpayPaymentLinkResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("billing: decode razorpay response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode != http.StatusOK {
		if out.Error != nil {
			return "", "", fmt.Errorf("billing: razorpay error: %s", out.Error.Description)
		}
		return "", "", fmt.Errorf("billing: razorpay returned %d: %s", resp.StatusCode, body)
	}

	if out.ID == "" || out.ShortURL == "" {
		return "", "", fmt.Errorf("billing: razorpay response missing id/short_url")
	}

	return out.ID, out.ShortURL, nil
}
