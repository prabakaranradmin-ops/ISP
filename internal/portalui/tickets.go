package portalui

import (
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/rs/zerolog/log"
)

type ticketsListData struct {
	Tickets []portal.TicketEntry
	Error   string
}

type ticketsData struct {
	baseData
	CSRFToken string
	List      ticketsListData
}

// Tickets handles GET /ui/tickets.
func (h *Handler) Tickets(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())

	var tickets []portal.TicketEntry
	if h.tickets != nil {
		var err error
		tickets, err = h.tickets.ListTickets(r.Context(), subID)
		if err != nil {
			http.Error(w, "failed to load tickets", http.StatusInternalServerError)
			return
		}
	}

	renderPage(w, http.StatusOK, "tickets", ticketsData{
		baseData:  baseData{Authenticated: true, Active: "tickets"},
		CSRFToken: h.csrfTokenFromRequest(r),
		List:      ticketsListData{Tickets: tickets},
	})
}

// CreateTicket handles POST /ui/tickets. Guarded by requireCSRF (see
// auth.go and RegisterRoutes). subscriber_id is always taken from the
// session context, never from the form body — mirrors
// portal.Handler.CreateTicket's existing guarantee, so a subscriber can
// never file a ticket against someone else's account by tampering with a
// hidden field. Returns only the refreshed ticket-list fragment.
func (h *Handler) CreateTicket(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())

	category := r.FormValue("category")
	description := r.FormValue("description")

	switch {
	case h.tickets == nil:
		h.renderTicketsList(w, r, subID, "Ticket filing is not available right now.")
	case category == "" || description == "":
		h.renderTicketsList(w, r, subID, "Category and description are required.")
	default:
		_, err := h.tickets.CreateTicket(r.Context(), portal.TicketCreateRequest{
			SubscriberID: subID,
			Category:     category,
			Description:  description,
		})
		if err != nil {
			// The subscriber is told to try again, which for most causes is
			// the right advice. But the common cause is not transient at
			// all: CreateTicket refuses to file a ticket it cannot attach an
			// SLA to, so a missing sla_policies or
			// category_priority_defaults row makes every ticket fail with
			// this message forever. Without this line the only symptom is a
			// subscriber who cannot raise a ticket and a request logged as
			// 200, which is a long way from the configuration gap that
			// caused it.
			log.Error().Err(err).Int("subscriber_id", subID).Str("category", category).
				Msg("portalui: filing a ticket failed")
			h.renderTicketsList(w, r, subID, "Failed to file the ticket. Please try again.")
			return
		}
		h.renderTicketsList(w, r, subID, "")
	}
}

func (h *Handler) renderTicketsList(w http.ResponseWriter, r *http.Request, subID int, errMsg string) {
	var tickets []portal.TicketEntry
	if h.tickets != nil {
		var err error
		tickets, err = h.tickets.ListTickets(r.Context(), subID)
		if err != nil && errMsg == "" {
			errMsg = "Failed to load the ticket list."
		}
	}
	renderFragment(w, "tickets-list", ticketsListData{Tickets: tickets, Error: errMsg})
}
