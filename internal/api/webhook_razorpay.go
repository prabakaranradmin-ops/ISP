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
		// Recorded even though nothing could be authenticated: a callback
		// arrived and whatever it described was not credited, which is
		// the single most reconciliation-relevant thing that can happen
		// here (FR-PAY-001).
		h.recordGatewayCallback(r.Context(), billing.GatewayEvent{
			Status: billing.GatewayUnconfigured,
			Detail: "a callback arrived while RAZORPAY_WEBHOOK_SECRET was not set, so it could not be authenticated",
		})
		http.Error(w, "webhook not configured", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRazorpayWebhookBody))
	if err != nil {
		h.recordGatewayCallback(r.Context(), billing.GatewayEvent{
			Status: billing.GatewayRejected, Detail: "the request body could not be read",
		})
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}

	if err := billing.ValidateRazorpaySignature(body, r.Header.Get("X-Razorpay-Signature"), h.razorpayWebhookSecret); err != nil {
		log.Warn().Err(err).Msg("api: rejected Razorpay webhook")
		// The signature itself is deliberately not recorded: it is a
		// credential-shaped value and authenticity has already been
		// decided. That it failed is the fact worth keeping.
		h.recordGatewayCallback(r.Context(), billing.GatewayEvent{
			Status: billing.GatewayRejected, Detail: "failed the signature check",
		})
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}

	var payload razorpayWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		h.recordGatewayCallback(r.Context(), billing.GatewayEvent{
			Status: billing.GatewayRejected,
			Detail: "authentic, but the payload could not be decoded",
		})
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	if payload.Event != "payment.captured" {
		// Acknowledged and recorded. "Whether or not it credits a wallet"
		// (FR-PAY-001) includes the events this system deliberately does
		// nothing about: a reconciliation needs to see that the callback
		// arrived and was a no-op, not infer it from silence.
		h.recordGatewayCallback(r.Context(), billing.GatewayEvent{
			Event:            payload.Event,
			GatewayPaymentID: payload.Payload.Payment.Entity.ID,
			Status:           billing.GatewayIgnored,
			Detail:           "this system acts only on payment.captured",
		})
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
		// Money taken and credited to nobody. The row most worth having.
		h.recordGatewayCallback(r.Context(), billing.GatewayEvent{
			Event:            payload.Event,
			GatewayPaymentID: entity.ID,
			Amount:           decimal.NewFromInt(entity.Amount).Div(decimal.NewFromInt(100)),
			Status:           billing.GatewayUnmatched,
			Detail:           "captured, but the subscriber_id note was missing or unreadable",
		})
		w.WriteHeader(http.StatusOK)
		return
	}

	amount := decimal.NewFromInt(entity.Amount).Div(decimal.NewFromInt(100))
	tx, err := h.walletSvc.Recharge(r.Context(), billing.RechargeRequest{
		SubscriberID:     subscriberID,
		Amount:           amount,
		TransactionToken: entity.ID,
		Description:      "recharge via razorpay webhook",
		Payment:          &billing.PaymentDetail{Method: billing.MethodOnline, Reference: entity.ID, RecordedBy: "razorpay"},
	})
	if err != nil {
		log.Error().Err(err).Str("payment_id", entity.ID).Int("subscriber_id", subscriberID).
			Msg("api: razorpay webhook credit failed")
		h.recordGatewayCallback(r.Context(), billing.GatewayEvent{
			Event: payload.Event, GatewayPaymentID: entity.ID,
			SubscriberID: subscriberID, Amount: amount,
			Status: billing.GatewayFailed, Detail: err.Error(),
		})
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

	// Recorded as credited whether this was the first capture or a
	// retried callback.
	//
	// Not distinguished, deliberately: Recharge is idempotent on
	// transaction_token (FR-BIL-005) and returns the original transaction
	// for a repeat, with no error and no signal that it was one — so
	// telling them apart here would mean an extra lookup on the money
	// path, and a racy one. Both outcomes are true: the money is in the
	// wallet. A retried callback shows as a second credited row sharing
	// one payment id, which idx_pgw_payment exists to surface.
	h.recordGatewayCallback(r.Context(), billing.GatewayEvent{
		Event: payload.Event, GatewayPaymentID: entity.ID,
		SubscriberID: subscriberID, Amount: amount,
		Status: billing.GatewayCredited,
	})

	w.WriteHeader(http.StatusOK)
}
