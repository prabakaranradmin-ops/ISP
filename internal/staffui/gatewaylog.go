package staffui

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
//
// GatewayLogReader reads the log. Satisfied by *db.GatewayLogStore.
// Read-only: the table has UPDATE and DELETE revoked from the role the
// services run as, because its rows are evidence in a payment dispute.
type GatewayLogReader interface {
	ListGatewayEvents(ctx context.Context, f billing.GatewayLogFilter) ([]billing.GatewayLogEntry, error)
	SummariseGatewayEvents(ctx context.Context, f billing.GatewayLogFilter) ([]billing.GatewayStatusCount, error)
}

type gatewayRow struct {
	billing.GatewayLogEntry
	Advice string
}

type gatewaySummaryRow struct {
	billing.GatewayStatusCount
	Label          string
	Advice         string
	NeedsAttention bool
}

type gatewayLogData struct {
	Rows    []gatewayRow
	Summary []gatewaySummaryRow
	// Filter values as given, so the form comes back filled in.
	Status       string
	Kind         string
	PaymentID    string
	ProblemsOnly bool
	From         string
	To           string
	Statuses     []gatewayStatusOption
	SummaryHours int
	// Problems is how many rows in the window need attention, so the
	// screen can lead with the number rather than make it be counted.
	Problems int
}

type gatewayStatusOption struct{ Key, Label string }

// gatewayStatusOptions is the filter dropdown, with the outcomes an
// operator is looking for first rather than alphabetically.
var gatewayStatusOptions = []gatewayStatusOption{
	{billing.GatewayUnmatched, "Paid, but matched no subscriber"},
	{billing.GatewayFailed, "Could not be completed"},
	{billing.GatewayRejected, "Rejected — failed authentication"},
	{billing.GatewayUnconfigured, "No webhook secret configured"},
	{billing.GatewayCredited, "Credited to the wallet"},
	{billing.GatewayCreated, "Payment link created"},
	{billing.GatewayIgnored, "Acknowledged, no action needed"},
}

// gatewaySummaryWindow is how far back the summary looks.
const gatewaySummaryWindow = 7 * 24 * time.Hour

// GatewayLog handles GET /staff/gateway-log.
func (h *Handler) GatewayLog(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "gateway-log")
	if !ok {
		return
	}
	d := h.page(s, "Gateway log", "gateway-log")

	if h.gatewayLog == nil {
		d.Error = "The payment-gateway log is not configured on this deployment."
		h.render(w, "gateway_log", d)
		return
	}

	q := r.URL.Query()
	gd := gatewayLogData{
		Status:       strings.TrimSpace(q.Get("status")),
		Kind:         strings.TrimSpace(q.Get("kind")),
		PaymentID:    strings.TrimSpace(q.Get("payment_id")),
		ProblemsOnly: q.Get("problems") == "true",
		From:         strings.TrimSpace(q.Get("from")),
		To:           strings.TrimSpace(q.Get("to")),
		Statuses:     gatewayStatusOptions,
		SummaryHours: int(gatewaySummaryWindow / time.Hour),
	}
	switch gd.Kind {
	case "", billing.GatewayKindOrder, billing.GatewayKindCallback:
	default:
		gd.Kind = ""
	}

	f := billing.GatewayLogFilter{
		Status: gd.Status, Kind: gd.Kind, PaymentID: gd.PaymentID,
		ProblemsOnly: gd.ProblemsOnly, Limit: 200,
	}
	if v := q.Get("subscriber_id"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.SubscriberID = n
		}
	}
	if gd.From != "" {
		if t, err := time.ParseInLocation("2006-01-02", gd.From, time.UTC); err == nil {
			f.From = &t
		} else {
			d.Error = "The 'from' date must be a date, for example 2026-04-01."
		}
	}
	if gd.To != "" {
		if t, err := time.ParseInLocation("2006-01-02", gd.To, time.UTC); err == nil {
			end := t.AddDate(0, 0, 1) // inclusive of the chosen day
			f.To = &end
		} else {
			d.Error = "The 'to' date must be a date, for example 2026-04-30."
		}
	}

	events, err := h.gatewayLog.ListGatewayEvents(r.Context(), f)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list gateway events failed")
		d.Error = "Could not load the gateway log."
		h.render(w, "gateway_log", d)
		return
	}
	for _, e := range events {
		gd.Rows = append(gd.Rows, gatewayRow{
			GatewayLogEntry: e, Advice: billing.GatewayStatusAdvice[e.Status],
		})
	}

	// The summary leads, because the question is "is money going missing",
	// not "what happened at 14:03".
	since := time.Now().Add(-gatewaySummaryWindow)
	if summary, err := h.gatewayLog.SummariseGatewayEvents(r.Context(),
		billing.GatewayLogFilter{From: &since}); err == nil {
		for _, c := range summary {
			probe := billing.GatewayLogEntry{Status: c.Status}
			row := gatewaySummaryRow{
				GatewayStatusCount: c,
				Label:              probe.StatusLabel(),
				Advice:             billing.GatewayStatusAdvice[c.Status],
				NeedsAttention:     probe.NeedsAttention(),
			}
			if row.NeedsAttention {
				gd.Problems += c.Count
			}
			gd.Summary = append(gd.Summary, row)
		}
	} else {
		log.Warn().Err(err).Msg("staffui: gateway event summary unavailable")
	}

	d.Data = gd
	h.render(w, "gateway_log", d)
}
