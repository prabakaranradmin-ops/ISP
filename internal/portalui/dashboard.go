package portalui

import (
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
)

type dashboardData struct {
	baseData
	Profile     *portal.SubscriberProfile
	SessionCard sessionCard
}

// sessionCard is what the live-usage card renders: the session, or why there
// is none. Unavailable is a read that failed, which is not the same thing as
// being offline.
type sessionCard struct {
	Session     *portal.ActiveSession
	Unavailable bool
}

// readSessionCard reads the live session for the card.
//
// The error used to be discarded, so a session store that could not be read
// told the customer "No active session — you appear to be offline", on the
// page and again on every 15-second poll. Someone told their line is down
// calls support about a line that may be fine.
func (h *Handler) readSessionCard(r *http.Request, subID int) sessionCard {
	if h.sessions == nil {
		return sessionCard{Unavailable: true}
	}
	session, err := h.sessions.GetActiveSession(r.Context(), subID)
	if err != nil {
		log.Warn().Err(err).Int("subscriber_id", subID).Msg("portalui: live session unavailable")
		return sessionCard{Unavailable: true}
	}
	return sessionCard{Session: session}
}

// Dashboard handles GET /ui/dashboard — the browsable equivalent of the JSON
// GET /portal/dashboard, reading from the same queriers portal.Handler uses.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())
	profile, err := h.subscribers.GetSubscriberByID(r.Context(), subID)
	if err != nil {
		// Not "not found": the customer has just signed in, and telling them
		// their account does not exist because a query failed is alarming
		// and wrong.
		log.Error().Err(err).Int("subscriber_id", subID).Msg("portalui: dashboard profile lookup failed")
		http.Error(w, "We could not load your account just now. Please try again in a moment.",
			http.StatusInternalServerError)
		return
	}
	if profile == nil {
		http.Error(w, "subscriber not found", http.StatusNotFound)
		return
	}

	renderPage(w, http.StatusOK, "dashboard", dashboardData{
		baseData:    baseData{Authenticated: true, Active: "dashboard"},
		Profile:     profile,
		SessionCard: h.readSessionCard(r, subID),
	})
}

// DashboardSessionFragment handles GET /ui/dashboard/session — the HTMX
// polling target (every 15s) for the live-usage card, so it refreshes
// without a full page reload.
func (h *Handler) DashboardSessionFragment(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())
	renderFragment(w, "session-inner", h.readSessionCard(r, subID))
}
