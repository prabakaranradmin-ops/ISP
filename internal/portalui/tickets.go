package portalui

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
	"github.com/rs/zerolog/log"
)

type ticketsListData struct {
	Tickets []portal.TicketEntry
	Error   string
	// CSRFToken is carried on the fragment, not only on the page: the rating
	// form is inside the list and the list replaces itself after every post,
	// so a token held only by the outer page would be gone from the markup
	// the subscriber is looking at by the time they rate a second ticket.
	CSRFToken string
	// Message is a success note, kept apart from Error so the two cannot be
	// styled or read as the same thing.
	Message string
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
		List:      ticketsListData{Tickets: tickets, CSRFToken: h.csrfTokenFromRequest(r)},
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
	h.renderTicketsListWith(w, r, subID, errMsg, "")
}

// renderTicketsListWith is renderTicketsList plus a success note, for the
// rating form — "thanks" belongs beside the list that now shows the rating,
// not on a page the subscriber has to navigate back from.
func (h *Handler) renderTicketsListWith(w http.ResponseWriter, r *http.Request, subID int, errMsg, message string) {
	var entries []portal.TicketEntry
	if h.tickets != nil {
		var err error
		entries, err = h.tickets.ListTickets(r.Context(), subID)
		if err != nil && errMsg == "" {
			errMsg = "Failed to load the ticket list."
		}
	}
	renderFragment(w, "tickets-list", ticketsListData{
		Tickets:   entries,
		Error:     errMsg,
		Message:   message,
		CSRFToken: h.csrfTokenFromRequest(r),
	})
}

// RateTicket handles POST /ui/tickets/{id}/rating — FR-SUP-004.
//
// Guarded by requireCSRF like every other write here, and the subscriber id
// comes from the session rather than the form, so a rating cannot be filed
// against somebody else's ticket by editing a hidden field — the same
// guarantee CreateTicket makes, for the same reason.
//
// Renders the refreshed list fragment rather than redirecting, matching how
// filing a ticket behaves on this screen.
func (h *Handler) RateTicket(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())

	if h.tickets == nil {
		h.renderTicketsList(w, r, subID, "Rating is not available right now.")
		return
	}

	ticketID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || ticketID <= 0 {
		h.renderTicketsList(w, r, subID, "That is not a valid ticket.")
		return
	}
	score, err := strconv.Atoi(r.FormValue("rating"))
	if err != nil {
		h.renderTicketsList(w, r, subID, "Choose a rating from 1 to 5.")
		return
	}

	err = h.tickets.RateTicket(r.Context(), ticketID, subID, tickets.Rating{
		Score:   score,
		Comment: r.FormValue("comment"),
	})
	switch {
	case err == nil:
		h.renderTicketsListWith(w, r, subID, "", "Thank you — your rating has been recorded.")
	case errors.Is(err, tickets.ErrRatingOutOfRange):
		h.renderTicketsList(w, r, subID, "Choose a rating from 1 to 5.")
	case errors.Is(err, tickets.ErrCommentTooLong):
		// The number, so the subscriber knows how much to cut rather than
		// guessing. Their text is still in the form.
		h.renderTicketsList(w, r, subID,
			fmt.Sprintf("Please keep the comment under %d characters.", tickets.MaxRatingComment))
	case errors.Is(err, tickets.ErrTicketNotResolved):
		h.renderTicketsList(w, r, subID,
			"That ticket is still open. You can rate it once it has been resolved.")
	case errors.Is(err, tickets.ErrTicketNotFound), errors.Is(err, tickets.ErrNotYourTicket):
		// One answer for both, as the API does: telling them apart would let
		// anybody discover which ticket ids exist.
		h.renderTicketsList(w, r, subID, "That ticket could not be found.")
	default:
		log.Error().Err(err).Int("subscriber_id", subID).Int("ticket_id", ticketID).
			Msg("portalui: rating a ticket failed")
		h.renderTicketsList(w, r, subID, "Could not save your rating. Please try again.")
	}
}
