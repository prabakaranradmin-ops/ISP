package staffui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
	"github.com/shopspring/decimal"
)

// Home sends an operator to the first section their role can use, rather than
// to a fixed landing page some roles would be bounced off.
func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	s, ok := SessionFrom(r.Context())
	if !ok {
		http.Redirect(w, r, "/staff/login", http.StatusSeeOther)
		return
	}
	allowed := AllowedSections(s.Role, s.LeaAccess)
	if len(allowed) == 0 {
		h.renderError(w, r, s, http.StatusForbidden,
			"Your role has no console sections assigned. Contact an administrator.")
		return
	}
	http.Redirect(w, r, allowed[0].Path, http.StatusSeeOther)
}

// ── Subscribers (all staff roles) ───────────────────────────────────────────

type subscribersData struct {
	Query   string
	Results []revenue.SubscriberRow
	Total   int
	// Directory is set when the console store is configured: the richer
	// list with status, plan and expiry columns, filters, and search across
	// phone, email, CAF and IP. Results/Total above are the fallback when it
	// is not, and are left empty when it is.
	Directory *DirectoryPage
	// LoadFailed suppresses the table and counts when the directory query
	// itself failed, leaving only the error banner and the search form.
	LoadFailed bool
	Filter     DirectoryFilter
	ViewLabel  string
	Statuses   []string
	Views      []string
	// ShowBulkActions gates the checkbox column and bulk-action card, same
	// tier as the API's own "admin" middleware (billing_admin, isp_owner)
	// that BulkChangeSubscriberPlan/BulkUpdateStatus/BulkWalletCredit sit
	// behind — the console must never offer a bulk action the API itself
	// would refuse.
	ShowBulkActions bool
	Plans           []Plan
}

// Subscribers lists subscribers with an optional search and filters.
//
// The search box takes whatever the operator has in front of them: a
// username or id from a ticket, or a phone number, email, CAF number or IP
// address read out by a caller. The status and view filters are what the
// dashboard's tiles link to.
func (h *Handler) Subscribers(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	q := r.URL.Query()
	h.renderSubscribers(w, r, s, DirectoryFilter{
		Query:  strings.TrimSpace(q.Get("q")),
		Status: q.Get("status"),
		View:   q.Get("view"),
	}, "", "")
}

// renderSubscribers is shared by Subscribers itself and by BulkAction, so a
// bulk action's per-subscriber results come back on the same list rather
// than a dead-end confirmation page.
func (h *Handler) renderSubscribers(w http.ResponseWriter, r *http.Request, s Session, f DirectoryFilter, message, errMsg string) {
	d := h.page(s, "Subscribers", "subscribers")
	d.Message, d.Error = message, errMsg
	showBulk := s.Role == "isp_owner" || s.Role == "billing_admin"

	if h.console != nil {
		h.renderDirectory(w, r, d, f, showBulk)
		return
	}
	query := f.Query

	if h.revenue == nil {
		d.Error = "The subscriber directory is not configured on this deployment."
		h.render(w, "subscribers", d)
		return
	}

	rows, err := h.revenue.ListSubscribers(r.Context(), nil)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list subscribers failed")
		if d.Error == "" {
			d.Error = "Could not load subscribers."
		}
		h.render(w, "subscribers", d)
		return
	}

	filtered := rows
	if query != "" {
		filtered = nil
		lower := strings.ToLower(query)
		for _, row := range rows {
			if strings.Contains(strings.ToLower(row.Username), lower) ||
				strconv.Itoa(row.ID) == query {
				filtered = append(filtered, row)
			}
		}
	}

	sd := subscribersData{
		Query:           query,
		Results:         filtered,
		Total:           len(rows),
		ShowBulkActions: showBulk,
	}
	h.attachBulkPlans(r, &sd)
	d.Data = sd
	h.render(w, "subscribers", d)
}

// renderDirectory is the console-store path of renderSubscribers.
//
// An unknown status or view in the URL is reported, and the list is shown
// unfiltered beneath the message, rather than silently treated as "any": a
// tile whose link is wrong should look wrong.
func (h *Handler) renderDirectory(w http.ResponseWriter, r *http.Request, d pageData, f DirectoryFilter, showBulk bool) {
	if f.Status != "" && !inStrings(SubscriberStatuses, f.Status) {
		d.Error = "Unknown status filter " + f.Status + "; showing everyone."
		f.Status = ""
	}
	if f.View != "" && !inStrings(DirectoryViews, f.View) {
		d.Error = "Unknown list " + f.View + "; showing everyone."
		f.View = ""
	}

	sd := subscribersData{
		Query:           f.Query,
		Filter:          f,
		ViewLabel:       directoryViewLabels[f.View],
		Statuses:        SubscriberStatuses,
		Views:           DirectoryViews,
		ShowBulkActions: showBulk,
	}
	page, err := h.console.ListDirectory(r.Context(), f)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list directory failed")
		if d.Error == "" {
			d.Error = "Could not load subscribers."
		}
		// No table and no "0 shown of 0" line: an empty list under an error
		// reads as "nobody matched", which is a different claim.
		sd.LoadFailed = true
		d.Data = sd
		h.render(w, "subscribers", d)
		return
	}
	sd.Directory = page
	h.attachBulkPlans(r, &sd)
	d.Data = sd
	h.render(w, "subscribers", d)
}

func (h *Handler) attachBulkPlans(r *http.Request, sd *subscribersData) {
	if !sd.ShowBulkActions || h.catalogue == nil {
		return
	}
	if plans, err := h.catalogue.ListPlans(r.Context()); err == nil {
		sd.Plans = plans
	}
}

func inStrings(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

type subscriberDetailData struct {
	Subscriber *api.SubscriberRecord
	Health     *health.SubscriberRecord
	Session    *portal.ActiveSession
	Balance    decimal.Decimal
	Ledger     []api.LedgerEntry
	Tickets    []portal.TicketEntry
	// ShowBilling and ShowTickets gate the panels by role, so a NOC engineer
	// does not see a wallet ledger the API would refuse them. ShowSpeedOverride
	// is narrower still — owner only, since it changes what a customer is
	// being charged to receive, not just who can see it.
	ShowBilling bool
	// ShowRecordPayment: billing admin and owner, the API's admin gate on
	// POST /wallets/recharge (FR-BIL-013).
	ShowRecordPayment bool
	PaymentMethods    []string
	// ShowGrace: the same roles, when grace extensions are wired
	// (FR-BIL-016).
	ShowGrace bool
	Grace     graceView
	// ShowProforma: the same roles, when proformas are wired (FR-BIL-018).
	ShowProforma         bool
	ProformaDefaultValid int
	// Charges are the catalogue products attached to this subscriber
	// (FR-BIL-022), including ended and already-billed ones so the history
	// of what they were charged is on the page. ShowCharges is read-only
	// reach (owner, billing, CSR); ShowManageCharges gates the attach and
	// end forms to owner and billing.
	ShowCharges       bool
	ShowManageCharges bool
	Charges           []billing.SubscriberProduct
	// AvailableProducts fills the attach dropdown: the offered catalogue
	// only, since a retired product cannot be attached to anyone new.
	AvailableProducts []billing.Product
	// ShowPlanChange gates the schedule/cancel forms (FR-BIL-023) to the
	// roles that may decide what the next renewal charges — the same set
	// as the immediate plan change. PlanChanges is this subscriber's
	// scheduled-change history, pending first.
	ShowPlanChange bool
	PlanChanges    []billing.ScheduledPlanChange
	// PendingPlanChange is the one waiting to fire, if any, so the plan
	// card can say so without the template searching the slice.
	PendingPlanChange *billing.ScheduledPlanChange
	// PlanChangeDefaultDate pre-fills the date field with the subscriber's
	// current expiry, which is what an operator almost always wants: the
	// change then lands on the next renewal. Formatted for <input
	// type="date">, and empty for a subscriber who has never had a cycle.
	PlanChangeDefaultDate string
	ShowTickets           bool
	ShowSpeedOverride     bool

	// Credentials card (view/reset password) and plan change.
	ShowCredentials   bool // can see the card and reveal the password
	ShowResetPassword bool // can set a new password
	ShowChangePlan    bool // can move the subscriber to another plan
	Plans             []Plan
	// Set only on the render that follows a reveal action, never on a plain
	// page load — the password is shown once, not baked into every view.
	PasswordRevealed bool
	PasswordCaptured bool
	RevealedPassword string
}

// SubscriberDetail is the 360 view: who they are, whether they are online, and
// — for roles entitled to see it — their money and their tickets.
func (h *Handler) SubscriberDetail(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}
	h.renderSubscriberDetail(w, r, s, id, "")
}

// renderSubscriberDetail is shared by SubscriberDetail itself and by
// ApplySpeedOverride/ClearSpeedOverride's validation-failure paths, so a
// rejected speed-override form comes back on the same page with everything
// else about the subscriber still visible, matching how every other
// create/edit form in this console reports its own errors.
func (h *Handler) renderSubscriberDetail(w http.ResponseWriter, r *http.Request, s Session, id int, errMsg string) {
	d := h.page(s, "Subscriber", "subscribers")
	d.Error = errMsg
	// Success messages from the reset/plan-change redirects land back here as a
	// query parameter, so the action confirms on the page it returns to rather
	// than a dead-end screen.
	if msg := r.URL.Query().Get("msg"); msg != "" {
		d.Message = msg
	}
	// A recorded payment confirms by amount, parsed as a number so the URL
	// can only ever carry a figure, not text of its own.
	if paid, err := decimal.NewFromString(r.URL.Query().Get("paid")); err == nil && paid.IsPositive() {
		d.Message = "Payment of ₹" + paid.StringFixed(2) + " recorded and credited to the wallet."
	}
	if m, ok := graceMessages[r.URL.Query().Get("grace")]; ok {
		d.Message = m
	}
	detail := subscriberDetailData{
		ShowBilling:          canAccess("billing", s.Role, s.LeaAccess),
		ShowRecordPayment:    canRecordPayment(s.Role) && h.payments != nil,
		ShowGrace:            canRecordPayment(s.Role) && h.dunning != nil,
		ShowProforma:         canRecordPayment(s.Role) && h.proformas != nil,
		ProformaDefaultValid: billing.ProformaDefaultValid,
		ShowCharges:          canAccess("products", s.Role, s.LeaAccess) && h.products != nil,
		ShowManageCharges:    canManageProducts(s.Role) && h.products != nil,
		ShowPlanChange:       canManageCredentials(s.Role) && h.planChanges != nil,
		PaymentMethods:       billing.PaymentMethods,
		ShowTickets:          canAccess("tickets", s.Role, s.LeaAccess),
		ShowSpeedOverride:    s.Role == "isp_owner",
		ShowCredentials:      canRevealPassword(s.Role),
		ShowResetPassword:    canManageCredentials(s.Role),
		ShowChangePlan:       canManageCredentials(s.Role),
	}
	// The plan dropdown for the change-plan form.
	if detail.ShowChangePlan && h.catalogue != nil {
		if plans, err := h.catalogue.ListPlans(r.Context()); err == nil {
			detail.Plans = plans
		}
	}
	// A revealed password is passed in through the request context by
	// RevealPassword, so it is shown on exactly the one render that follows the
	// reveal action and never persists into an ordinary page load.
	if rs, ok := r.Context().Value(revealedPasswordKey).(revealState); ok {
		detail.PasswordRevealed = true
		detail.PasswordCaptured = rs.Stored
		detail.RevealedPassword = rs.Password
	}

	if h.subscribers != nil {
		sub, err := h.subscribers.GetSubscriberByID(r.Context(), id)
		if err != nil {
			log.Error().Err(err).Int("id", id).Msg("staffui: subscriber lookup failed")
		}
		if sub == nil {
			h.renderError(w, r, s, http.StatusNotFound, "No subscriber with that id.")
			return
		}
		detail.Subscriber = sub
		detail.Grace = newGraceView(sub.GraceUntil, sub.GraceReason, sub.GraceSetBy, sub.PlanExpiry, sub.Status, time.Now())
		// The next renewal's date, as the default for a scheduled plan
		// change (FR-BIL-023). An expiry already in the past is no use as
		// a default — Normalise would refuse it — so today is offered
		// instead, which applies the change at the next pass.
		if sub.PlanExpiry != nil {
			when := billing.DayOf(*sub.PlanExpiry)
			if today := billing.DayOf(time.Now()); when.Before(today) {
				when = today
			}
			detail.PlanChangeDefaultDate = when.Format("2006-01-02")
		}
	}

	// Each panel degrades on its own. A failing diagnostic must not take the
	// whole page down when the operator only needed the plan and status.
	if h.health != nil {
		if hr, err := h.health.GetSubscriberWithMeta(r.Context(), id); err == nil {
			detail.Health = hr
		} else {
			log.Warn().Err(err).Int("id", id).Msg("staffui: health panel unavailable")
		}
	}
	// Live session comes from Redis, separately from the database record: a
	// subscriber can be active on paper and offline in fact, and the difference
	// is the whole question when someone calls to say the internet is down.
	if h.sessions != nil {
		if sess, err := h.sessions.GetActiveSession(r.Context(), id); err == nil {
			detail.Session = sess
		} else {
			log.Warn().Err(err).Int("id", id).Msg("staffui: live session unavailable")
		}
	}
	if detail.ShowBilling && h.billing != nil {
		if bal, err := h.billing.GetSubscriberBalance(r.Context(), id); err == nil {
			detail.Balance = bal
		}
		if entries, err := h.billing.ListLedgerEntries(r.Context(), id, nil, nil, 10); err == nil {
			detail.Ledger = entries
		}
	}
	if detail.ShowTickets && h.tickets != nil {
		if ts, err := h.tickets.ListTickets(r.Context(), id); err == nil {
			detail.Tickets = ts
		}
	}
	// Scheduled plan changes (FR-BIL-023). Read whenever the plan card is
	// shown, not only when the operator may change it: "they are moving to
	// Fibre on the 1st" is the answer to a support call, and a CSR who
	// cannot schedule one still needs to see that one exists.
	if h.planChanges != nil && detail.ShowBilling {
		list, err := h.planChanges.ListPlanChanges(r.Context(),
			billing.PlanChangeFilter{SubscriberID: id, Limit: 20})
		if err != nil {
			log.Warn().Err(err).Int("id", id).Msg("staffui: scheduled plan changes unavailable")
		} else {
			detail.PlanChanges = list
			for i := range list {
				if list[i].Pending() {
					detail.PendingPlanChange = &list[i]
					break
				}
			}
		}
	}

	// Catalogue charges (FR-BIL-022). History included, so a billing
	// question about last month's invoice can be answered from this page.
	// Degrades on its own like every other panel here.
	if detail.ShowCharges {
		if charges, err := h.products.ListSubscriberProducts(r.Context(), id, true); err == nil {
			detail.Charges = charges
		} else {
			log.Warn().Err(err).Int("id", id).Msg("staffui: charges panel unavailable")
		}
		if detail.ShowManageCharges {
			if list, err := h.products.ListProducts(r.Context(),
				billing.ProductFilter{ActiveOnly: true, Limit: 200}); err == nil {
				detail.AvailableProducts = list
			} else {
				log.Warn().Err(err).Msg("staffui: product list for the attach form unavailable")
			}
		}
	}

	d.Data = detail
	h.render(w, "subscriber_detail", d)
}

// ── Billing (billing_admin, csr, isp_owner) ─────────────────────────────────

type billingData struct {
	SubscriberID int
	Balance      decimal.Decimal
	Ledger       []api.LedgerEntry
	Looked       bool
}

// Billing shows a subscriber's wallet ledger.
func (h *Handler) Billing(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "billing")
	if !ok {
		return
	}
	d := h.page(s, "Billing", "billing")

	idParam := strings.TrimSpace(r.URL.Query().Get("subscriber_id"))
	if idParam == "" {
		d.Data = billingData{}
		h.render(w, "billing", d)
		return
	}
	id, err := strconv.Atoi(idParam)
	if err != nil {
		d.Error = "Enter a numeric subscriber id."
		d.Data = billingData{}
		h.render(w, "billing", d)
		return
	}
	if h.billing == nil {
		d.Error = "Billing is not configured on this deployment."
		d.Data = billingData{}
		h.render(w, "billing", d)
		return
	}

	bd := billingData{SubscriberID: id, Looked: true}
	if bal, err := h.billing.GetSubscriberBalance(r.Context(), id); err == nil {
		bd.Balance = bal
	} else {
		log.Warn().Err(err).Int("id", id).Msg("staffui: balance unavailable")
	}
	if entries, err := h.billing.ListLedgerEntries(r.Context(), id, nil, nil, 50); err == nil {
		bd.Ledger = entries
	} else {
		d.Error = "Could not load the ledger for that subscriber."
	}

	d.Data = bd
	h.render(w, "billing", d)
}

// ── Support (csr, technician, isp_owner) ────────────────────────────────────

type ticketsData struct {
	SubscriberID int
	Tickets      []portal.TicketEntry
	Looked       bool
	Statuses     []string

	// Queue mode (FR-CON-005), used whenever the console store is wired:
	// every subscriber's tickets in one list, with the helpdesk figures.
	QueueMode  bool
	Queue      []QueueTicket
	Summary    *HelpdeskSummary
	Filter     TicketFilter
	Views      []string
	Priorities []string
	Assignees  []Assignee
	// FilterQuery re-encodes the active filter so each row's update form
	// returns the operator to the same view.
	FilterQuery string
}

var ticketStatuses = []string{"open", "in_progress", "resolved", "closed"}

// Tickets is the support queue: every subscriber's tickets by default,
// narrowed by the view chips, priority, a search, or one subscriber.
func (h *Handler) Tickets(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "tickets")
	if !ok {
		return
	}
	if h.console != nil {
		h.renderTicketQueue(w, r, s)
		return
	}
	d := h.page(s, "Support", "tickets")
	td := ticketsData{Statuses: ticketStatuses}

	idParam := strings.TrimSpace(r.URL.Query().Get("subscriber_id"))
	if idParam != "" {
		id, err := strconv.Atoi(idParam)
		if err != nil {
			d.Error = "Enter a numeric subscriber id."
		} else if h.tickets == nil {
			d.Error = "Ticketing is not configured on this deployment."
		} else {
			td.SubscriberID = id
			td.Looked = true
			if ts, err := h.tickets.ListTickets(r.Context(), id); err == nil {
				td.Tickets = ts
			} else {
				d.Error = "Could not load tickets for that subscriber."
			}
		}
	}
	if msg := r.URL.Query().Get("updated"); msg != "" {
		d.Message = "Ticket updated."
	}

	d.Data = td
	h.render(w, "tickets", d)
}

// UpdateTicketStatus moves a ticket through the support workflow.
func (h *Handler) UpdateTicketStatus(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "tickets")
	if !ok {
		return
	}
	ticketID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid ticket id.")
		return
	}
	if h.tickets == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Ticketing is not configured.")
		return
	}

	status := r.PostFormValue("status")
	currentStatus := r.PostFormValue("current_status")
	subscriberID := r.PostFormValue("subscriber_id")

	// Validated against the allowed set rather than passed through: the column
	// has no CHECK constraint, so an arbitrary string would persist and then
	// render as an unknown badge everywhere it appears.
	if !inStrings(ticketStatuses, status) {
		h.renderError(w, r, s, http.StatusUnprocessableEntity, "That is not a valid ticket status.")
		return
	}

	// Assignment (queue mode). Blank means "leave as is". Any other value must
	// be one of the staff the queue offered — tickets.assigned_to's FK would
	// accept any staff id, including a billing admin who cannot then open
	// the ticket.
	var assignee *int
	if raw := strings.TrimSpace(r.PostFormValue("assigned_to")); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || !h.isTicketAssignee(r.Context(), id) {
			h.renderError(w, r, s, http.StatusUnprocessableEntity, "That person cannot be assigned tickets.")
			return
		}
		assignee = &id
	}

	// Only a real change of status is applied and announced. An assignment
	// from the queue re-submits the row's unchanged status, and telling the
	// subscriber "your ticket was updated" for an internal reassignment is
	// noise at best.
	statusChanged := currentStatus == "" || currentStatus != status
	var statusPtr *string
	if statusChanged {
		statusPtr = &status
	}
	if statusPtr == nil && assignee == nil {
		http.Redirect(w, r, "/staff/tickets?"+ticketReturnQuery(r, subscriberID).Encode(), http.StatusSeeOther) //nolint:gosec
		return
	}

	updated, err := h.tickets.UpdateTicketAdmin(r.Context(), ticketID, statusPtr, assignee, nil)
	if err != nil {
		log.Error().Err(err).Int("ticket_id", ticketID).Msg("staffui: ticket update failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not update that ticket.")
		return
	}

	// FR-NOTIF-007: tell the subscriber. A CSR resolving a ticket from here is
	// the exact "embarrassed in front of a subscriber" gap this closes — the
	// customer would otherwise never learn their ticket moved.
	if updated != nil && statusChanged {
		h.enqueueTicketUpdate(r.Context(), *updated)
	}

	// gosec G710 (open redirect): the target can never leave /staff/tickets —
	// it is a fixed literal, and every parameter only fills a query value,
	// built with url.Values.Encode rather than concatenated, so a value
	// containing & or # cannot inject parameters or corrupt the target.
	q := ticketReturnQuery(r, subscriberID)
	q.Set("updated", "1")
	http.Redirect(w, r, "/staff/tickets?"+q.Encode(), http.StatusSeeOther) //nolint:gosec
}

// ticketReturnQuery rebuilds the queue filter the update form came from,
// from known parameter names only, so the operator lands back on the same
// view rather than the default one.
func ticketReturnQuery(r *http.Request, subscriberID string) url.Values {
	q := url.Values{}
	for _, k := range []string{"view", "priority", "q"} {
		if v := strings.TrimSpace(r.PostFormValue("return_" + k)); v != "" {
			q.Set(k, v)
		}
	}
	if subscriberID != "" {
		q.Set("subscriber_id", subscriberID)
	}
	return q
}

func (h *Handler) isTicketAssignee(ctx context.Context, id int) bool {
	if h.console == nil {
		return false
	}
	as, err := h.console.ListTicketAssignees(ctx)
	if err != nil {
		return false
	}
	for _, a := range as {
		if a.ID == id {
			return true
		}
	}
	return false
}

// renderTicketQueue is the console-store path of Tickets (FR-CON-005).
func (h *Handler) renderTicketQueue(w http.ResponseWriter, r *http.Request, s Session) {
	d := h.page(s, "Support", "tickets")
	q := r.URL.Query()
	f := TicketFilter{
		View:          q.Get("view"),
		Priority:      q.Get("priority"),
		Query:         strings.TrimSpace(q.Get("q")),
		StaffUsername: s.Username,
		Limit:         300,
	}
	if f.View == "" {
		f.View = "open"
	}
	if !inStrings(TicketViews, f.View) {
		d.Error = "Unknown ticket list " + f.View + "; showing open tickets."
		f.View = "open"
	}
	if f.Priority != "" && !inStrings(TicketPriorities, f.Priority) {
		d.Error = "Unknown priority " + f.Priority + "; showing every priority."
		f.Priority = ""
	}
	if raw := strings.TrimSpace(q.Get("subscriber_id")); raw != "" {
		if id, err := strconv.Atoi(raw); err == nil && id > 0 {
			f.SubscriberID = id
		} else {
			d.Error = "Enter a numeric subscriber id."
		}
	}
	if q.Get("updated") != "" {
		d.Message = "Ticket updated."
	}

	td := ticketsData{
		QueueMode:  true,
		Statuses:   ticketStatuses,
		Filter:     f,
		Views:      TicketViews,
		Priorities: TicketPriorities,
	}
	fq := url.Values{"view": {f.View}}
	if f.Priority != "" {
		fq.Set("priority", f.Priority)
	}
	if f.Query != "" {
		fq.Set("q", f.Query)
	}
	td.FilterQuery = fq.Encode()

	var err error
	if td.Queue, err = h.console.ListTicketQueue(r.Context(), f); err != nil {
		log.Error().Err(err).Msg("staffui: ticket queue failed")
		d.Error = "Could not load tickets."
	}
	if sum, err := h.console.HelpdeskSummary(r.Context()); err == nil {
		td.Summary = sum
	} else {
		log.Error().Err(err).Msg("staffui: helpdesk summary failed")
	}
	if as, err := h.console.ListTicketAssignees(r.Context()); err == nil {
		td.Assignees = as
	}
	d.Data = td
	h.render(w, "tickets", d)
}

// enqueueTicketUpdate tells the subscriber their ticket's status changed
// (FR-NOTIF-007, TMPL-008). Failures are logged, never surfaced: the ticket
// is already updated at this point, so failing the request over an
// undelivered notice would tell the CSR their update did not take when it
// did.
func (h *Handler) enqueueTicketUpdate(ctx context.Context, ticket api.TicketRecord) {
	if h.tasks == nil {
		return
	}

	username := ""
	if h.subscribers != nil {
		if sub, err := h.subscribers.GetSubscriberByID(ctx, ticket.SubscriberID); err == nil && sub != nil {
			username = sub.Username
		}
	}

	payload, err := json.Marshal(tickets.UpdatePayload{
		SubscriberID: ticket.SubscriberID,
		Username:     username,
		TicketID:     ticket.ID,
		Status:       ticket.Status,
	})
	if err != nil {
		log.Warn().Err(err).Msg("staffui: ticket update payload marshal failed")
		return
	}

	task := jobqueue.NewTask(tickets.TaskTypeTicketUpdate, payload,
		jobqueue.Queue("notifications"),
		jobqueue.MaxRetry(3),
		jobqueue.Retention(24*time.Hour))
	if _, err := h.tasks.Enqueue(task); err != nil {
		log.Warn().Err(err).Int("ticket_id", ticket.ID).
			Msg("staffui: ticket update enqueue failed")
	}
}

// ── Revenue (isp_owner) ─────────────────────────────────────────────────────

type revenueData struct {
	Unbilled     int
	Variance     decimal.Decimal
	TotalBalance decimal.Decimal
	Subscribers  int
	VarianceOK   bool

	// Collections (FR-REV-003).
	Stages           []revenue.CollectionsStageRow
	TotalOutstanding decimal.Decimal
	AtRiskCount      int
	ThisMonth        decimal.Decimal
	LastMonth        decimal.Decimal
	RecoveryPct      decimal.Decimal
	// RecoveryComparable is false in a first month of operation, where a
	// change measured against zero has no meaning worth printing.
	RecoveryComparable bool
}

// Revenue is the owner's dashboard: the figures the nightly reconciliation
// computes, read live.
func (h *Handler) Revenue(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "revenue")
	if !ok {
		return
	}
	d := h.page(s, "Revenue", "revenue")
	if h.revenue == nil {
		d.Error = "Revenue reporting is not configured on this deployment."
		d.Data = revenueData{}
		h.render(w, "revenue", d)
		return
	}

	rd := revenueData{}
	if n, err := h.revenue.GetUnbilledActiveSubscribers(r.Context()); err == nil {
		rd.Unbilled = n
	}
	if v, err := h.revenue.GetLedgerVariance(r.Context()); err == nil {
		rd.Variance = v
		// Same threshold the nightly reconciliation alerts on, so the screen
		// and the alert cannot disagree about what "balanced" means.
		rd.VarianceOK = v.Abs().LessThanOrEqual(decimal.NewFromFloat(0.01))
	}
	if b, err := h.revenue.GetTotalWalletBalance(r.Context()); err == nil {
		rd.TotalBalance = b
	}
	if rows, err := h.revenue.ListSubscribers(r.Context(), nil); err == nil {
		rd.Subscribers = len(rows)
	}

	// Collections (FR-REV-003). A failure here degrades the panel rather
	// than the page: the reconciliation figures above are what the screen
	// has always been for, and losing them because a collections query
	// failed would be the worse trade.
	if stages, err := h.revenue.GetCollectionsByDunningStage(r.Context()); err != nil {
		log.Error().Err(err).Msg("staffui: collections by stage failed")
	} else {
		rd.Stages = stages
		for _, st := range stages {
			rd.TotalOutstanding = rd.TotalOutstanding.Add(st.Outstanding)
			if st.ServiceStopped {
				rd.AtRiskCount += st.Subscribers
			}
		}
	}
	if recovery, err := h.revenue.GetMonthlyRecovery(r.Context(), 2); err != nil {
		log.Error().Err(err).Msg("staffui: monthly recovery failed")
	} else {
		// Ordered most recent first by the query. A month with no
		// collections produces no row at all rather than a zero one, so
		// the slice can be shorter than asked for - indexing it blindly
		// would panic on a quiet month.
		if len(recovery) > 0 {
			rd.ThisMonth = recovery[0].Collected
		}
		if len(recovery) > 1 {
			rd.LastMonth = recovery[1].Collected
		}
		rd.RecoveryPct, rd.RecoveryComparable = revenue.RecoveryRate(rd.ThisMonth, rd.LastMonth)
	}

	d.Data = rd
	h.render(w, "revenue", d)
}

// ── LEA lookup (noc_engineer, isp_owner — and only with lea_access) ─────────

type leaData struct {
	PublicIP  string
	Timestamp string
	Result    *api.LEAResult
	Searched  bool
}

// LEAPage renders the lookup form.
func (h *Handler) LEAPage(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "lea")
	if !ok {
		return
	}
	d := h.page(s, "LEA Lookup", "lea")
	d.Data = leaData{Timestamp: time.Now().UTC().Format("2006-01-02T15:04")}
	h.render(w, "lea", d)
}

// LEALookup resolves which subscriber held an IP at a moment in time.
//
// Every attempt is written to lea_audit_log, found or not: an attempted query
// is as auditable as a successful one, and the table is append-only at the
// database level so the record cannot be tidied away afterwards.
func (h *Handler) LEALookup(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "lea")
	if !ok {
		return
	}
	d := h.page(s, "LEA Lookup", "lea")

	ip := strings.TrimSpace(r.PostFormValue("public_ip"))
	tsRaw := strings.TrimSpace(r.PostFormValue("timestamp"))
	ld := leaData{PublicIP: ip, Timestamp: tsRaw, Searched: true}

	if h.lea == nil {
		d.Error = "Lookup is not configured on this deployment."
		d.Data = ld
		h.render(w, "lea", d)
		return
	}

	at, err := time.Parse("2006-01-02T15:04", tsRaw)
	if err != nil {
		d.Error = "Enter the time as YYYY-MM-DDTHH:MM."
		d.Data = ld
		h.render(w, "lea", d)
		return
	}

	result, lookupErr := h.lea.LookupByPublicIP(r.Context(), ip, nil, at)

	rowCount := 0
	var resultSubID *int
	if result != nil {
		rowCount = 1
		resultSubID = &result.SubscriberID
	}
	// Recorded before the result is shown, and independently of whether the
	// lookup succeeded.
	if err := h.lea.RecordLEAAudit(r.Context(), api.LEAAuditEntry{
		AccessorIdentity:   s.Username,
		AccessorRole:       s.Role,
		QueriedPublicIP:    ip,
		QueriedTimestamp:   at,
		ResultSubscriberID: resultSubID,
		ResultRowCount:     rowCount,
	}); err != nil {
		log.Error().Err(err).Str("accessor", s.Username).Msg("staffui: LEA audit write failed")
	}

	switch {
	case lookupErr != nil:
		d.Error = "Lookup failed."
		log.Error().Err(lookupErr).Msg("staffui: LEA lookup failed")
	case result == nil:
		d.Message = "No subscriber held that address at that time."
	default:
		ld.Result = result
	}

	d.Data = ld
	h.render(w, "lea", d)
}
