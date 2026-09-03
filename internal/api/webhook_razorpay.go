package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/partner"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// maxRazorpayWebhookBody caps the request body read so a malicious caller
// cannot force unbounded allocation before the signature has been checked.
const maxRazorpayWebhookBody = 1 << 20 // 1 MiB

// razorpayWebhookPayload is the subset of Razorpay's payment.captured payload
// this handler needs. subscriber_id travels in the order's notes map, which
// the order-creation flow (portal one-tap renewal) sets when the order is
// created — see portal.RazorpayOrderCreator.
type razorpayWebhookPayload struct {
	Event   string `json:"event"`
	Payload struct {
		Payment struct {
			Entity struct {
				ID     string `json:"id"`
				Amount int64  `json:"amount"` // paise
				Notes  struct {
					SubscriberID string `json:"subscriber_id"`
				} `json:"notes"`
			} `json:"entity"`
		} `json:"payment"`
	} `json:"payload"`
}

// RazorpayWebhook handles POST /webhooks/razorpay.
//
// Only payment.captured moves money. Every other event Razorpay sends
// (order.paid, payment.failed, refund.*, ...) is acknowledged with 200 so
// Razorpay stops retrying it, without touching the wallet.
//
// FR: FR-SEC-004, FR-BIL-005 | DDS §5.6
func (h *Handler) RazorpayWebhook(w http.ResponseWriter, r *http.Request) {
	if h.razorpayWebhookSecret == "" {
		// An empty secret would make ValidateRazorpaySignature check against
		// HMAC-SHA256 with a "" key, which anyone can compute — that is not a
		// missing feature degrading gracefully, it is an open money-crediting
		// endpoint. Refuse outright instead.
		log.Error().Msg("api: razorpay webhook called but RAZORPAY_WEBHOOK_SECRET is not configured")
		http.Error(w, "webhook not configured", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRazorpayWebhookBody))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	if err := billing.ValidateRazorpaySignature(body, r.Header.Get("X-Razorpay-Signature"), h.razorpayWebhookSecret); err != nil {
		log.Warn().Err(err).Msg("api: rejected Razorpay webhook")
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var payload razorpayWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if payload.Event != "payment.captured" {
		w.WriteHeader(http.StatusOK)
		return
	}

	entity := payload.Payload.Payment.Entity
	subscriberID, err := strconv.Atoi(entity.Notes.SubscriberID)
	if err != nil || subscriberID <= 0 {
		// A captured payment with no subscriber_id note is a data problem on
		// our side (order creation should always set it), not a transient
		// one. Acknowledge so Razorpay does not retry something a retry
		// cannot fix, but log loudly since a payment is now unmatched.
		log.Error().Str("payment_id", entity.ID).Msg("api: razorpay webhook missing subscriber_id note")
		w.WriteHeader(http.StatusOK)
		return
	}

	amount := decimal.NewFromInt(entity.Amount).Div(decimal.NewFromInt(100))
	tx, err := h.walletSvc.Recharge(r.Context(), billing.RechargeRequest{
		SubscriberID:     subscriberID,
		Amount:           amount,
		TransactionToken: entity.ID,
		Description:      "recharge via razorpay webhook",
	})
	if err != nil {
		log.Error().Err(err).Str("payment_id", entity.ID).Int("subscriber_id", subscriberID).
			Msg("api: razorpay webhook credit failed")
		// Non-2xx tells Razorpay to retry — appropriate here since the
		// signature already proved authenticity and the failure is likely our
		// database being unavailable, not bad input.
		http.Error(w, "recharge failed", http.StatusInternalServerError)
		return
	}
	if h.franchises != nil {
		revenue.SettleCommissionForRecharge(r.Context(), h.franchises, subscriberID, amount, entity.ID)
	}

	// The receipt FR-NOTIF-004 asks for ("on successful wallet recharge").
	//
	// This was missing here while the manual staff-recharge path in
	// routes.go had it, which is backwards: a subscriber whose payment a
	// CSR keyed in got a confirmation, and one who paid through the gateway
	// — the path every real customer uses — got silence. It also suppressed
	// FR-NOTIF-006's "you are back online" message for exactly the people
	// most likely to need it, since paying to clear a suspension is what
	// the gateway is for.
	h.enqueuePaymentReceipt(r.Context(), subscriberID, amount.StringFixed(2), tx.BalanceAfter.String())

	// Emitted only after the wallet credit committed. Publishing before it
	// would tell a partner money arrived that a rollback then unwound.
	h.emit(r.Context(), partner.EventPaymentReceived, subscriberID)

	w.WriteHeader(http.StatusOK)
}
