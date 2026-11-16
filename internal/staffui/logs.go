package staffui

import (
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// maxLogRange caps the log window. These tables grow by the message, and a
// screen is for answering "did this customer get the SMS"; a month is ample.
const maxLogRange = 31 * 24 * time.Hour

// canSeeWebhookLog limits partner webhook deliveries to the owner, who is the
// only role that can issue the partner keys those endpoints belong to.
func canSeeWebhookLog(role string) bool { return role == "isp_owner" }

type logsData struct {
	Tab         string // "messages" or "webhooks"
	From, To    string
	Channel     string
	Status      string
	Query       string
	Messages    []MessageLogRow
	Webhooks    []WebhookLogRow
	Channels    []string
	Statuses    []string
	ShowWebhook bool
}

// Logs shows outbound subscriber messages and, for the owner, partner
// webhook deliveries.
func (h *Handler) Logs(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "logs")
	if !ok {
		return
	}
	d := h.page(s, "Message logs", "logs")
	if h.console == nil {
		d.Error = "Logs are not configured on this deployment."
		h.render(w, "logs", d)
		return
	}
	q := r.URL.Query()
	ld := logsData{
		Tab:         q.Get("tab"),
		Channel:     q.Get("channel"),
		Status:      q.Get("status"),
		Query:       strings.TrimSpace(q.Get("q")),
		Channels:    MessageChannels,
		ShowWebhook: canSeeWebhookLog(s.Role),
	}
	if ld.Tab != "webhooks" {
		ld.Tab = "messages"
	}
	if ld.Tab == "webhooks" && !ld.ShowWebhook {
		h.renderError(w, r, s, http.StatusForbidden, "Only the owner can see partner webhook deliveries.")
		return
	}
	ld.Statuses = MessageStatuses
	if ld.Tab == "webhooks" {
		ld.Statuses = WebhookStatuses
	}
	if ld.Channel != "" && !inStrings(MessageChannels, ld.Channel) {
		d.Error, ld.Channel = "Unknown channel "+ld.Channel+"; showing every channel.", ""
	}
	if ld.Status != "" && !inStrings(ld.Statuses, ld.Status) {
		d.Error, ld.Status = "Unknown status "+ld.Status+"; showing every status.", ""
	}

	from, to, msg := parseSessionRange(q.Get("from"), q.Get("to"), time.Now())
	if msg != "" && d.Error == "" {
		d.Error = strings.Replace(msg, "Completed sessions are", "Logs are", 1)
	}
	if to.Sub(from) > maxLogRange {
		from = to.Add(-maxLogRange)
	}
	ld.From, ld.To = from.Format("2006-01-02"), to.Add(-24*time.Hour).Format("2006-01-02")
	f := LogFilter{From: from, To: to, Channel: ld.Channel, Status: ld.Status, Query: ld.Query, Limit: 500}

	var err error
	if ld.Tab == "messages" {
		ld.Messages, err = h.console.ListMessageLog(r.Context(), f)
	} else {
		ld.Webhooks, err = h.console.ListWebhookDeliveries(r.Context(), f)
	}
	if err != nil {
		log.Error().Err(err).Str("tab", ld.Tab).Msg("staffui: list logs failed")
		d.Error = "Could not load the log."
	}
	d.Data = ld
	h.render(w, "logs", d)
}
