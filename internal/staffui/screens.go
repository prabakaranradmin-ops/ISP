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
}

// Subscribers lists subscribers with an optional search.
//
// Searching by username or id covers what staff actually have in front of
// them: a customer on the phone gives a username, a ticket carries an id.
func (h *Handler) Subscribers(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	d := h.page(s, "Subscribers", "subscribers")

	if h.revenue == nil {
		d.Error = "The subscriber directory is not configured on this deployment."
		h.render(w, "subscribers", d)
		return
	}

	rows, err := h.revenue.ListSubscribers(r.Context(), nil)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list subscribers failed")
		d.Error = "Could not load subscribers."
		h.render(w, "subscribers", d)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
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

	d.Data = subscribersData{Query: query, Results: filtered, Total: len(rows)}
	h.render(w, "subscribers", d)
}

type subscriberDetailData struct {
	Subscriber *api.SubscriberRecord
	Health     *health.SubscriberRecord
	Session    *portal.ActiveSession
	Balance    decimal.Decimal
	Ledger     []api.LedgerEntry
	Tickets    []portal.TicketEntry
	// ShowBilling and ShowTickets gate the panels by role, so a NOC engineer
	// does not see a wallet ledger the API would refuse them.
	ShowBilling bool
	ShowTickets bool
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

	d := h.page(s, "Subscriber", "subscribers")
	detail := subscriberDetailData{
		ShowBilling: canAccess("billing", s.Role, s.LeaAccess),
		ShowTickets: canAccess("tickets", s.Role, s.LeaAccess),
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
}

// Tickets shows a subscriber's support queue.
func (h *Handler) Tickets(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "tickets")
	if !ok {
		return
	}
	d := h.page(s, "Support", "tickets")
	td := ticketsData{Statuses: []string{"open", "in_progress", "resolved", "closed"}}

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
	subscriberID := r.PostFormValue("subscriber_id")

	// Validated against the allowed set rather than passed through: the column
	// has no CHECK constraint, so an arbitrary string would persist and then
	// render as an unknown badge everywhere it appears.
	valid := map[string]bool{"open": true, "in_progress": true, "resolved": true, "closed": true}
	if !valid[status] {
		h.renderError(w, r, s, http.StatusUnprocessableEntity, "That is not a valid ticket status.")
		return
	}

	updated, err := h.tickets.UpdateTicketAdmin(r.Context(), ticketID, &status, nil, nil)
	if err != nil {
		log.Error().Err(err).Int("ticket_id", ticketID).Msg("staffui: ticket update failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not update that ticket.")
		return
	}

	// FR-NOTIF-007: tell the subscriber. A CSR resolving a ticket from here is
	// the exact "embarrassed in front of a subscriber" gap this closes — the
	// customer would otherwise never learn their ticket moved.
	if updated != nil {
		h.enqueueTicketUpdate(r.Context(), *updated)
	}

	// gosec G710 (open redirect): the target can never leave /staff/tickets —
	// it is a fixed literal, and subscriberID only ever fills a query
	// parameter's value, not the redirect's host. Genuinely fixed anyway:
	// subscriberID was raw PostFormValue, concatenated unescaped, so a value
	// containing & or # could inject additional query parameters or corrupt
	// the target the browser navigates to. url.Values.Encode is the correct
	// way to build this regardless of whether that was exploitable today.
	redirectQuery := url.Values{"subscriber_id": {subscriberID}, "updated": {"1"}}
	http.Redirect(w, r, "/staff/tickets?"+redirectQuery.Encode(), http.StatusSeeOther) //nolint:gosec
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
