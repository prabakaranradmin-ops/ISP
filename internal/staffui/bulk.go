package staffui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Bulk subscriber actions — the Subscribers screen's multi-select toolbar.
// Every action here calls straight into internal/api's own *ForMany /
// SendBulkNotification methods (internal/api/subscribers_bulk.go,
// announcements.go): the console is a second caller of that logic, not a
// second implementation of it, the same relationship NewSubscriber already
// has with api.ProvisionSubscriber.

// BulkActionExecutor runs one bulk action across many subscriber ids.
// Satisfied by *api.Handler.
type BulkActionExecutor interface {
	ChangePlanForMany(ctx context.Context, ids []int, newPlanID int) []api.BulkResult
	UpdateStatusForMany(ctx context.Context, ids []int, status string) []api.BulkResult
	RequestCreditForMany(ctx context.Context, requestedBy string, ids []int, amount decimal.Decimal, reason string) []api.BulkCreditResult
	SendBulkNotification(ctx context.Context, requestedBy string, ids []int, title, body string, channels []string, showInPortal bool) (int, error)
}

// BulkAction handles POST /staff/subscribers/bulk.
//
// Restricted to isp_owner/billing_admin — the same tier the underlying API
// bulk endpoints require — checked here too rather than relying solely on
// the template hiding the form from other roles (see this package's own
// doc comment on that point).
func (h *Handler) BulkAction(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if s.Role != "isp_owner" && s.Role != "billing_admin" {
		h.renderError(w, r, s, http.StatusForbidden, "Your role cannot perform bulk actions.")
		return
	}
	if h.bulkActions == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Bulk actions are not configured.")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderSubscribers(w, r, s, "", "", "Could not read the submitted form.")
		return
	}

	var ids []int
	for _, raw := range r.PostForm["ids"] {
		if id, err := strconv.Atoi(raw); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		h.renderSubscribers(w, r, s, "", "", "Select at least one subscriber before applying a bulk action.")
		return
	}

	ctx := r.Context()
	switch r.PostFormValue("action") {
	case "plan_change":
		h.bulkPlanChange(w, r, s, ctx, ids)
	case "status":
		h.bulkStatusChange(w, r, s, ctx, ids)
	case "credit":
		h.bulkCredit(w, r, s, ctx, ids)
	case "notify":
		h.bulkNotify(w, r, s, ctx, ids)
	default:
		h.renderSubscribers(w, r, s, "", "", "Unknown bulk action.")
	}
}

func (h *Handler) bulkPlanChange(w http.ResponseWriter, r *http.Request, s Session, ctx context.Context, ids []int) {
	planID, err := strconv.Atoi(r.PostFormValue("plan_id"))
	if err != nil || planID <= 0 {
		h.renderSubscribers(w, r, s, "", "", "Choose a plan for the bulk plan change.")
		return
	}
	results := h.bulkActions.ChangePlanForMany(ctx, ids, planID)
	ok, failed := splitBulkResults(results)
	h.renderSubscribers(w, r, s, "", summarizeBulk("Plan changed", ok, failed), "")
}

func (h *Handler) bulkStatusChange(w http.ResponseWriter, r *http.Request, s Session, ctx context.Context, ids []int) {
	status := r.PostFormValue("status")
	if !api.BulkAllowedStatuses[status] {
		h.renderSubscribers(w, r, s, "", "", "Choose a valid status for the bulk status change.")
		return
	}
	results := h.bulkActions.UpdateStatusForMany(ctx, ids, status)
	ok, failed := splitBulkResults(results)
	h.renderSubscribers(w, r, s, "", summarizeBulk("Status set to "+status+" for", ok, failed), "")
}

func (h *Handler) bulkCredit(w http.ResponseWriter, r *http.Request, s Session, ctx context.Context, ids []int) {
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if reason == "" {
		h.renderSubscribers(w, r, s, "", "", "A reason is required for a bulk wallet credit.")
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue("amount")))
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		h.renderSubscribers(w, r, s, "", "", "Amount must be a positive decimal, for example 100.00.")
		return
	}

	results := h.bulkActions.RequestCreditForMany(ctx, s.Username, ids, amount, reason)
	okCount := 0
	var failed []string
	for _, res := range results {
		if res.OK {
			okCount++
			continue
		}
		failed = append(failed, fmt.Sprintf("id %d (%s)", res.SubscriberID, res.Error))
	}
	msg := fmt.Sprintf("%d pending wallet-credit approval(s) filed — each still needs a second staff member's sign-off, not applied yet.", okCount)
	if len(failed) > 0 {
		msg += " Failed: " + strings.Join(failed, "; ") + "."
	}
	h.renderSubscribers(w, r, s, "", msg, "")
}

func (h *Handler) bulkNotify(w http.ResponseWriter, r *http.Request, s Session, ctx context.Context, ids []int) {
	title := strings.TrimSpace(r.PostFormValue("title"))
	body := strings.TrimSpace(r.PostFormValue("body"))
	if title == "" || body == "" {
		h.renderSubscribers(w, r, s, "", "", "A title and message are required to send a notification.")
		return
	}
	var channels []string
	if r.PostFormValue("channel_sms") == "on" {
		channels = append(channels, "sms")
	}
	if r.PostFormValue("channel_email") == "on" {
		channels = append(channels, "email")
	}
	if r.PostFormValue("channel_whatsapp") == "on" {
		channels = append(channels, "whatsapp")
	}
	showInPortal := r.PostFormValue("show_in_portal") == "on"
	if len(channels) == 0 && !showInPortal {
		h.renderSubscribers(w, r, s, "", "", "Pick at least one channel or the portal banner for the notification.")
		return
	}

	enqueued, err := h.bulkActions.SendBulkNotification(ctx, s.Username, ids, title, body, channels, showInPortal)
	if err != nil {
		log.Error().Err(err).Msg("staffui: bulk notification failed")
		h.renderSubscribers(w, r, s, "", "", "Could not send the notification.")
		return
	}
	h.renderSubscribers(w, r, s, "",
		fmt.Sprintf("Notification sent to %d of %d selected subscriber(s) (%d message(s) enqueued).",
			len(ids), len(ids), enqueued), "")
}

func splitBulkResults(results []api.BulkResult) (ok int, failed []string) {
	for _, res := range results {
		if res.OK {
			ok++
			continue
		}
		failed = append(failed, fmt.Sprintf("id %d (%s)", res.SubscriberID, res.Error))
	}
	return ok, failed
}

func summarizeBulk(verb string, ok int, failed []string) string {
	msg := fmt.Sprintf("%s for %d subscriber(s).", verb, ok)
	if len(failed) > 0 {
		msg += " Failed: " + strings.Join(failed, "; ") + "."
	}
	return msg
}
