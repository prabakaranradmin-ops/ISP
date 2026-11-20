package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/rs/zerolog/log"
)

// Payment-gateway order and callback log — FR-PAY-001 | migration 064.
// See internal/billing/gatewaylog.go for the vocabulary.

// GatewayLogStore records and reads gateway events. Satisfied by
// *db.GatewayLogStore.
type GatewayLogStore interface {
	billing.GatewayLogger
	ListGatewayEvents(ctx context.Context, f billing.GatewayLogFilter) ([]billing.GatewayLogEntry, error)
	SummariseGatewayEvents(ctx context.Context, f billing.GatewayLogFilter) ([]billing.GatewayStatusCount, error)
}

// recordGatewayCallback writes one callback outcome.
//
// Every failure is logged and swallowed, and the context is detached:
// this is called from exit paths that have already decided the response,
// including ones where the caller's context is about to go away, and the
// event being described has already happened. Refusing a credit because
// its log row could not be written would turn a bookkeeping problem into
// a lost payment.
func (h *Handler) recordGatewayCallback(_ context.Context, e billing.GatewayEvent) {
	if h.gatewayLog == nil {
		return
	}
	e.Kind = billing.GatewayKindCallback
	if e.Gateway == "" {
		e.Gateway = billing.DefaultGateway
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := h.gatewayLog.RecordGatewayEvent(ctx, e); err != nil {
		log.Error().Err(err).Str("status", e.Status).Str("payment_id", e.GatewayPaymentID).
			Msg("api: gateway callback could not be recorded; the log line above is the only record")
	}
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

type gatewayEventJSON struct {
	ID               int64  `json:"id"`
	Gateway          string `json:"gateway"`
	Kind             string `json:"kind"`
	Event            string `json:"event,omitempty"`
	GatewayOrderID   string `json:"gateway_order_id,omitempty"`
	GatewayPaymentID string `json:"gateway_payment_id,omitempty"`
	SubscriberID     *int   `json:"subscriber_id,omitempty"`
	Username         string `json:"username,omitempty"`
	Amount           string `json:"amount,omitempty"`
	Status           string `json:"status"`
	Detail           string `json:"detail,omitempty"`
	NeedsAttention   bool   `json:"needs_attention"`
	CreatedAt        string `json:"created_at"`
}

func toGatewayEventJSON(e billing.GatewayLogEntry) gatewayEventJSON {
	out := gatewayEventJSON{
		ID: e.ID, Gateway: e.Gateway, Kind: e.Kind, Event: e.Event,
		GatewayOrderID: e.GatewayOrderID, GatewayPaymentID: e.GatewayPaymentID,
		SubscriberID: e.SubscriberID, Username: e.Username,
		Status: e.Status, Detail: e.Detail,
		NeedsAttention: e.NeedsAttention(),
		CreatedAt:      e.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if e.Amount != nil {
		out.Amount = e.Amount.StringFixed(2)
	}
	return out
}

// ListGatewayEventsHTTP handles GET /api/v1/gateway-events.
func (h *Handler) ListGatewayEventsHTTP(w http.ResponseWriter, r *http.Request) {
	if h.gatewayLog == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "the gateway log is not configured")
		return
	}
	q := r.URL.Query()
	f := billing.GatewayLogFilter{
		Status:       strings.TrimSpace(q.Get("status")),
		Kind:         strings.TrimSpace(q.Get("kind")),
		PaymentID:    strings.TrimSpace(q.Get("payment_id")),
		ProblemsOnly: q.Get("problems_only") == "true",
		Limit:        parseLimit(q, 200, 500),
	}
	if v := q.Get("subscriber_id"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_id must be a positive number")
			return
		}
		f.SubscriberID = n
	}

	list, err := h.gatewayLog.ListGatewayEvents(r.Context(), f)
	if err != nil {
		log.Error().Err(err).Msg("api: list gateway events failed")
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not read the gateway log")
		return
	}
	out := make([]gatewayEventJSON, 0, len(list))
	for _, e := range list {
		out = append(out, toGatewayEventJSON(e))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}
