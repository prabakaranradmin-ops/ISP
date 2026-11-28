package staffui

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
	"github.com/rs/zerolog/log"
)

// RADIUS authentication attempts — FR-NET-004 | migration 063.
//
// AuthLogReader reads the attempt log. Satisfied by *db.AuthLogStore.
type AuthLogReader interface {
	ListAuthAttempts(ctx context.Context, f radius.AuthLogFilter) ([]radius.AuthLogEntry, error)
	SummariseAuthRejects(ctx context.Context, since time.Time) ([]radius.AuthRejectSummary, error)
}

// rejectReasonLabels turn the stored vocabulary into something an
// operator can act on. The stored value stays short and groupable; this
// is only for display.
var rejectReasonLabels = map[string]string{
	radius.RejectUnknownUser:     "No such username",
	radius.RejectBadPassword:     "Wrong password",
	radius.RejectNotAuthorised:   "Account not in a state that grants service",
	radius.RejectLockedOut:       "Locked out after repeated failures",
	radius.RejectNoMABDevice:     "MAC not registered, or MAB off for this router",
	radius.RejectEAPFailed:       "EAP exchange failed",
	radius.RejectInternalError:   "Internal error",
	radius.RejectConnectionLimit: "Over their connection limit",
}

// rejectReasonAdvice says what each failure usually means, because the
// reason alone tells an operator what happened and not what to do.
var rejectReasonAdvice = map[string]string{
	radius.RejectUnknownUser:   "A typo, or the subscriber was never provisioned. Check the username on their account.",
	radius.RejectBadPassword:   "A typo, or a router configured with an old password after a reset.",
	radius.RejectNotAuthorised: "Suspended, terminated, or awaiting their first payment — not a network fault. Check their billing.",
	radius.RejectLockedOut:     "Too many failures in a short window. It clears by itself; a burst of these from one username is worth looking at.",
	radius.RejectNoMABDevice:   "The MAC is not registered to this router, or the router has MAB turned off.",
	radius.RejectEAPFailed:     "The supplicant and the server could not agree. Usually a client configured for the wrong EAP method.",
	radius.RejectInternalError: "Something on this side failed rather than the subscriber. Check the daemon's log around this time.",
	radius.RejectConnectionLimit: "The password was right, but they already have as many connections online as they pay for. " +
		"Sell them an extra connection, or end one of their sessions on the Connections screen.",
}

type authLogRow struct {
	radius.AuthLogEntry
	ReasonLabel string
}

type rejectSummaryRow struct {
	radius.AuthRejectSummary
	Label  string
	Advice string
}

type authLogData struct {
	Rows    []authLogRow
	Summary []rejectSummaryRow
	// Filter values as given, so the form comes back filled in.
	Username string
	Result   string
	Reason   string
	From     string
	To       string
	Reasons  []authReasonOption
	// SummaryHours is the window the reject summary covers.
	SummaryHours int
	Retention    int
}

type authReasonOption struct{ Key, Label string }

// authReasonOptions is the filter dropdown, in the order an operator is
// likely to want them rather than alphabetically.
var authReasonOptions = []authReasonOption{
	{radius.RejectBadPassword, rejectReasonLabels[radius.RejectBadPassword]},
	{radius.RejectUnknownUser, rejectReasonLabels[radius.RejectUnknownUser]},
	{radius.RejectNotAuthorised, rejectReasonLabels[radius.RejectNotAuthorised]},
	{radius.RejectLockedOut, rejectReasonLabels[radius.RejectLockedOut]},
	{radius.RejectNoMABDevice, rejectReasonLabels[radius.RejectNoMABDevice]},
	{radius.RejectEAPFailed, rejectReasonLabels[radius.RejectEAPFailed]},
	{radius.RejectConnectionLimit, rejectReasonLabels[radius.RejectConnectionLimit]},
	{radius.RejectInternalError, rejectReasonLabels[radius.RejectInternalError]},
}

// authSummaryWindow is how far back the reject summary looks.
const authSummaryWindow = 24 * time.Hour

// AuthLog handles GET /staff/auth-log.
func (h *Handler) AuthLog(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "auth-log")
	if !ok {
		return
	}
	d := h.page(s, "Authentication log", "auth-log")

	if h.authLog == nil {
		d.Error = "The authentication log is not configured on this deployment."
		h.render(w, "auth_log", d)
		return
	}

	q := r.URL.Query()
	ad := authLogData{
		Username:     strings.TrimSpace(q.Get("username")),
		Result:       strings.TrimSpace(q.Get("result")),
		Reason:       strings.TrimSpace(q.Get("reason")),
		From:         strings.TrimSpace(q.Get("from")),
		To:           strings.TrimSpace(q.Get("to")),
		Reasons:      authReasonOptions,
		SummaryHours: int(authSummaryWindow / time.Hour),
		Retention:    int(radius.AuthLogRetention / (24 * time.Hour)),
	}
	switch ad.Result {
	case "", radius.ResultAccept, radius.ResultReject:
	default:
		ad.Result = ""
	}

	f := radius.AuthLogFilter{
		Username: ad.Username, Result: ad.Result, Reason: ad.Reason, Limit: 200,
	}
	if ad.From != "" {
		if t, err := time.ParseInLocation("2006-01-02", ad.From, time.Local); err == nil {
			f.From = &t
		} else {
			d.Error = "The 'from' date must be a date, for example 2026-04-01."
		}
	}
	if ad.To != "" {
		if t, err := time.ParseInLocation("2006-01-02", ad.To, time.Local); err == nil {
			// Inclusive of the chosen day, as every other date filter here.
			end := t.AddDate(0, 0, 1)
			f.To = &end
		} else {
			d.Error = "The 'to' date must be a date, for example 2026-04-30."
		}
	}

	entries, err := h.authLog.ListAuthAttempts(r.Context(), f)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list authentication attempts failed")
		d.Error = "Could not load the authentication log."
		h.render(w, "auth_log", d)
		return
	}
	for _, e := range entries {
		ad.Rows = append(ad.Rows, authLogRow{
			AuthLogEntry: e, ReasonLabel: rejectReasonLabels[e.RejectReason],
		})
	}

	// The summary first, because "which failure is happening" is the
	// question; the list of attempts is how it gets narrowed down.
	if summary, err := h.authLog.SummariseAuthRejects(r.Context(), time.Now().Add(-authSummaryWindow)); err == nil {
		for _, s := range summary {
			label := rejectReasonLabels[s.Reason]
			if label == "" {
				label = s.Reason
			}
			ad.Summary = append(ad.Summary, rejectSummaryRow{
				AuthRejectSummary: s, Label: label, Advice: rejectReasonAdvice[s.Reason],
			})
		}
	} else {
		log.Warn().Err(err).Msg("staffui: authentication reject summary unavailable")
	}

	d.Data = ad
	h.render(w, "auth_log", d)
}
