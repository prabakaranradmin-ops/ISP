package staffui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
)

// SessionDisconnector ends a live session. Satisfied by *api.Handler, the
// same instance behind SubscriberCreator/BulkActions, so a disconnect from
// the console runs the API's own PoD and audit path rather than a copy.
type SessionDisconnector interface {
	RequestDisconnect(ctx context.Context, sessionID string) (int, error)
}

// maxCompletedRange caps the completed-sessions window. History is
// partitioned by month and a busy ISP writes thousands of rows a day; a month
// is as far back as a screen (rather than a report export) should reach.
const maxCompletedRange = 31 * 24 * time.Hour

// canDisconnect mirrors the API's nocOnly gate on
// POST /api/v1/sessions/{id}/disconnect: dropping a customer's connection is
// a network action, not a front-desk or billing one.
func canDisconnect(role string) bool {
	return role == "noc_engineer" || role == "isp_owner"
}

type sessionsData struct {
	Tab           string // "live" or "completed"
	Query         string
	FUPOnly       bool
	From, To      string // yyyy-mm-dd, as the date inputs hold them
	Live          []LiveSession
	Completed     []CompletedSession
	CanDisconnect bool
	Limit         int
}

const sessionsPageLimit = 500

// Sessions lists who is online now, or which sessions ended in a date range.
func (h *Handler) Sessions(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "sessions")
	if !ok {
		return
	}
	h.renderSessions(w, r, s, "", "")
}

func (h *Handler) renderSessions(w http.ResponseWriter, r *http.Request, s Session, message, errMsg string) {
	d := h.page(s, "Sessions", "sessions")
	d.Message, d.Error = message, errMsg
	if h.console == nil {
		d.Error = "Session monitoring is not configured on this deployment."
		h.render(w, "sessions", d)
		return
	}

	q := r.URL.Query()
	sd := sessionsData{
		Tab:           q.Get("tab"),
		Query:         strings.TrimSpace(q.Get("q")),
		FUPOnly:       q.Get("fup") == "1",
		CanDisconnect: canDisconnect(s.Role),
		Limit:         sessionsPageLimit,
	}
	if sd.Tab != "completed" {
		sd.Tab = "live"
	}

	var err error
	if sd.Tab == "live" {
		sd.Live, err = h.console.ListLiveSessions(r.Context(), SessionFilter{
			Query: sd.Query, FUPOnly: sd.FUPOnly, Limit: sessionsPageLimit,
		})
	} else {
		from, to, rangeErr := parseSessionRange(q.Get("from"), q.Get("to"), time.Now())
		sd.From, sd.To = from.Format("2006-01-02"), to.Add(-24*time.Hour).Format("2006-01-02")
		if rangeErr != "" && d.Error == "" {
			d.Error = rangeErr
		}
		sd.Completed, err = h.console.ListCompletedSessions(r.Context(), SessionFilter{
			Query: sd.Query, From: from, To: to, Limit: sessionsPageLimit,
		})
	}
	if err != nil {
		log.Error().Err(err).Str("tab", sd.Tab).Msg("staffui: list sessions failed")
		if d.Error == "" {
			d.Error = "Could not load sessions."
		}
	}
	d.Data = sd
	h.render(w, "sessions", d)
}

// parseSessionRange turns the two date inputs into a half-open [from, to)
// window covering whole days in the server's local time. Blank inputs mean
// "yesterday and today". A range longer than maxCompletedRange, or reversed,
// is corrected rather than refused, and the returned message says how.
func parseSessionRange(fromStr, toStr string, now time.Time) (time.Time, time.Time, string) {
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	to := today.Add(24 * time.Hour)
	from := today.Add(-24 * time.Hour)
	var msg string

	if toStr != "" {
		if t, err := time.ParseInLocation("2006-01-02", toStr, loc); err == nil {
			to = t.Add(24 * time.Hour)
		} else {
			msg = "Dates must be in the form YYYY-MM-DD; showing the default range."
		}
	}
	if fromStr != "" {
		if t, err := time.ParseInLocation("2006-01-02", fromStr, loc); err == nil {
			from = t
		} else {
			msg = "Dates must be in the form YYYY-MM-DD; showing the default range."
		}
	}
	if !to.After(from) {
		from, to = to.Add(-24*time.Hour), from.Add(24*time.Hour)
		msg = "The start date was after the end date, so they were swapped."
	}
	if to.Sub(from) > maxCompletedRange {
		from = to.Add(-maxCompletedRange)
		msg = "Completed sessions are shown for at most 31 days at a time; the start date was moved."
	}
	return from, to, msg
}

// DisconnectSessionForm handles POST /staff/sessions/{session_id}/disconnect.
func (h *Handler) DisconnectSessionForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "sessions")
	if !ok {
		return
	}
	if !canDisconnect(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Your role cannot disconnect sessions.")
		return
	}
	if h.disconnector == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Disconnecting sessions is not configured on this deployment.")
		return
	}
	sessionID := r.PathValue("session_id")
	username := r.PostFormValue("username")

	// WithSubject so the audit line names this operator, not a service.
	ctx := middleware.WithSubject(r.Context(), s.Username)
	_, err := h.disconnector.RequestDisconnect(ctx, sessionID)
	who := username
	if who == "" {
		who = "session " + sessionID
	}
	switch {
	case errors.Is(err, api.ErrSessionNotFound):
		h.renderSessions(w, r, s, "", "That session has already ended, so there was nothing to disconnect.")
	case errors.Is(err, api.ErrSessionControlUnavailable):
		h.renderSessions(w, r, s, "", "Disconnecting sessions is not configured on this deployment.")
	case err != nil:
		log.Error().Err(err).Str("session_id", sessionID).Msg("staffui: disconnect failed")
		h.renderSessions(w, r, s, "", "Could not send the disconnect. Try again in a moment.")
	default:
		h.renderSessions(w, r, s,
			"Disconnect sent for "+who+". The router drops the session within a few seconds; the list updates once it reports the stop.", "")
	}
}
