package staffui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// CreditNoteRequester files a credit-note approval request (FR-BIL-012).
// Satisfied by *api.Handler. The note itself is issued only when a
// different staff member approves the request on the Tasks screen.
type CreditNoteRequester interface {
	RequestCreditNote(ctx context.Context, invoiceID int, base decimal.Decimal, reason string) (*workflow.ApprovalRequest, error)
}

// canRequestCreditNote mirrors the API's admin gate on
// POST /invoices/{id}/credit-notes. A CSR can see invoices but not ask for
// money to go back against one.
func canRequestCreditNote(role string) bool { return role == "isp_owner" || role == "billing_admin" }

type creditNoteFormData struct {
	Invoice *InvoiceRow
	Amount  string
	Reason  string
	Request *workflow.ApprovalRequest
	Remains string
}

// CreditNotePage handles GET /staff/invoices/{id}/credit-note.
func (h *Handler) CreditNotePage(w http.ResponseWriter, r *http.Request) {
	s, ok := h.creditNoteGate(w, r)
	if !ok {
		return
	}
	h.renderCreditNoteForm(w, r, s, creditNoteFormData{}, "")
}

func (h *Handler) creditNoteGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "invoices")
	if !ok {
		return Session{}, false
	}
	if !canRequestCreditNote(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing staff and the owner can request a credit note.")
		return Session{}, false
	}
	if h.creditNotes == nil || h.console == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Credit notes are not configured on this deployment.")
		return Session{}, false
	}
	return s, true
}

// findInvoice loads one invoice's list row by id, through the same query the
// Invoices screen uses, so the form shows exactly the figures on that list.
func (h *Handler) findInvoice(r *http.Request, id int) (*InvoiceRow, error) {
	rows, _, err := h.console.ListInvoices(r.Context(), MoneyFilter{
		From: epoch, To: farFuture, Query: "INV-" + strconv.Itoa(id), Limit: 1,
	})
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func (h *Handler) renderCreditNoteForm(w http.ResponseWriter, r *http.Request, s Session, fd creditNoteFormData, errMsg string) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid invoice id.")
		return
	}
	inv, err := h.findInvoice(r, id)
	if err != nil {
		log.Error().Err(err).Int("invoice_id", id).Msg("staffui: invoice lookup failed")
		h.renderError(w, r, s, http.StatusInternalServerError, "Could not load that invoice.")
		return
	}
	if inv == nil {
		h.renderError(w, r, s, http.StatusNotFound, "No invoice with that id.")
		return
	}
	fd.Invoice = inv
	if base, err1 := decimal.NewFromString(inv.Base); err1 == nil {
		if total, err2 := decimal.NewFromString(inv.Total); err2 == nil {
			if credited, err3 := decimal.NewFromString(inv.Credited); err3 == nil && total.IsPositive() {
				// Taxable value still uncredited, assuming the credits so far
				// carried the invoice's own tax proportion — which
				// ComputeCreditNote guarantees for every note it wrote.
				fd.Remains = base.Mul(total.Sub(credited)).Div(total).Round(2).StringFixed(2)
			}
		}
	}
	d := h.page(s, "Credit note", "invoices")
	d.Error = errMsg
	d.Data = fd
	h.render(w, "credit_note", d)
}

// CreditNoteForm handles POST /staff/invoices/{id}/credit-note.
func (h *Handler) CreditNoteForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.creditNoteGate(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid invoice id.")
		return
	}
	fd := creditNoteFormData{
		Amount: strings.TrimSpace(r.PostFormValue("amount")),
		Reason: strings.TrimSpace(r.PostFormValue("reason")),
	}
	amount, err := decimal.NewFromString(fd.Amount)
	if err != nil {
		h.renderCreditNoteForm(w, r, s, fd, "Enter the amount to credit as a number, for example 100.00.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	req, err := h.creditNotes.RequestCreditNote(ctx, id, amount, fd.Reason)
	switch {
	case api.CreditNoteInvalidReason(err) != "":
		h.renderCreditNoteForm(w, r, s, fd, "Not requested: "+api.CreditNoteInvalidReason(err)+".")
	case errors.Is(err, api.ErrCreditNoteNoInvoice):
		h.renderError(w, r, s, http.StatusNotFound, "No invoice with that id.")
	case err != nil:
		log.Error().Err(err).Int("invoice_id", id).Msg("staffui: credit note request failed")
		h.renderCreditNoteForm(w, r, s, fd, "Could not file the request. Try again in a moment.")
	default:
		h.renderCreditNoteForm(w, r, s, creditNoteFormData{Request: req}, "")
	}
}

// epoch and farFuture bracket every invoice this system can hold, for
// looking one up by number through the windowed invoice list.
var (
	epoch     = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	farFuture = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)
