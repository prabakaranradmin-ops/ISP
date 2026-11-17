package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ProformaManager issues, converts and cancels proforma invoices
// (FR-BIL-018). Satisfied by *api.Handler, so the console applies the same
// rules, writes the same audit entries and emits the same invoice event.
type ProformaManager interface {
	IssueProforma(ctx context.Context, req billing.ProformaRequest) (*billing.Proforma, error)
	ConvertProforma(ctx context.Context, id int) (*billing.ConvertedProforma, error)
	CancelProforma(ctx context.Context, id int, reason string) error
	ListProformas(ctx context.Context, f billing.ProformaFilter) ([]billing.Proforma, error)
	RenderProformaPDF(ctx context.Context, id int) ([]byte, error)
}

// proformaMessages words each outcome carried back in the URL as a code.
// "issued", "converted" and "cancelled" are successes; the rest errors.
var proformaMessages = map[string]string{
	"issued":    "Proforma issued. Download the PDF to send to the customer.",
	"converted": "Converted. The wallet was charged and the tax invoice is on the Invoices tab.",
	"cancelled": "Proforma cancelled.",
	"short": "Not converted: the wallet does not cover the invoice total. " +
		"Record the customer's payment on their Billing tab first, then convert.",
	"closed":  "That proforma has already been converted or cancelled.",
	"expired": "Not converted: the proforma is past its valid-until date, so its price no longer stands. Issue a new one.",
	"reason":  "Not cancelled: give a reason.",
	"missing": "No proforma with that number.",
	"failed":  "That did not go through. Nothing was changed; try again.",
}

var proformaSuccess = map[string]bool{"issued": true, "converted": true, "cancelled": true}

type proformaRow struct {
	billing.Proforma
	Expired bool
	// Invoice is the tax invoice's id, 0 if none. Proforma.InvoiceID is a
	// pointer, which a template would print as an address.
	Invoice int
}

type proformasData struct {
	Status       string // "open", "converted", "cancelled" or "all"
	SubscriberID int
	Rows         []proformaRow
	CanManage    bool
	Chips        []proformaChip
}

type proformaChip struct{ Key, Label string }

var proformaChips = []proformaChip{
	{billing.ProformaOpen, "Open"}, {billing.ProformaConverted, "Converted"},
	{billing.ProformaCancelled, "Cancelled"}, {"all", "All"},
}

func (h *Handler) proformaGate(w http.ResponseWriter, r *http.Request) (Session, bool) {
	s, ok := h.requireSection(w, r, "invoices")
	if !ok {
		return s, false
	}
	if h.proformas == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Proforma invoices are not configured on this deployment.")
		return s, false
	}
	return s, true
}

// Proformas handles GET /staff/invoices/proformas.
func (h *Handler) Proformas(w http.ResponseWriter, r *http.Request) {
	s, ok := h.proformaGate(w, r)
	if !ok {
		return
	}
	d := h.page(s, "Proforma invoices", "invoices")
	q := r.URL.Query()
	pd := proformasData{Status: q.Get("status"), CanManage: canRecordPayment(s.Role), Chips: proformaChips}
	f := billing.ProformaFilter{Limit: 200}
	switch pd.Status {
	case billing.ProformaConverted, billing.ProformaCancelled:
		f.Status = pd.Status
	case "all":
	default:
		pd.Status, f.Status = billing.ProformaOpen, billing.ProformaOpen
	}
	if n, err := strconv.Atoi(q.Get("subscriber")); err == nil && n > 0 {
		pd.SubscriberID, f.SubscriberID = n, n
	}
	if code := q.Get("pf"); code != "" {
		if m, ok := proformaMessages[code]; ok {
			if proformaSuccess[code] {
				d.Message = m
			} else {
				d.Error = m
			}
		}
	}
	list, err := h.proformas.ListProformas(r.Context(), f)
	if err != nil {
		log.Error().Err(err).Msg("staffui: list proformas failed")
		d.Error = "Could not load the proformas."
	}
	now := time.Now()
	for _, p := range list {
		row := proformaRow{Proforma: p,
			Expired: p.Status == billing.ProformaOpen && billing.ProformaExpired(p.ValidUntil, now)}
		if p.InvoiceID != nil {
			row.Invoice = *p.InvoiceID
		}
		pd.Rows = append(pd.Rows, row)
	}
	d.Data = pd
	h.render(w, "proformas", d)
}

// IssueProformaForm handles POST /staff/subscribers/{id}/proforma.
func (h *Handler) IssueProformaForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canRecordPayment(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing staff and the owner can issue proformas.")
		return
	}
	if h.proformas == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Proforma invoices are not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}
	base, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue("amount")))
	if err != nil {
		h.renderSubscriberDetail(w, r, s, id, "Proforma not issued: enter the amount as a number, for example 1500.00.")
		return
	}
	days := 0
	if v := strings.TrimSpace(r.PostFormValue("valid_days")); v != "" {
		if days, err = strconv.Atoi(v); err != nil {
			h.renderSubscriberDetail(w, r, s, id, "Proforma not issued: validity is a whole number of days.")
			return
		}
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	_, err = h.proformas.IssueProforma(ctx, billing.ProformaRequest{
		SubscriberID: id, Description: r.PostFormValue("description"), Base: base, ValidDays: days,
	})
	switch {
	case err == nil:
		http.Redirect(w, r, fmt.Sprintf("/staff/invoices/proformas?subscriber=%d&pf=issued", id), http.StatusSeeOther)
	case billing.ProformaInvalidReason(err) != "":
		h.renderSubscriberDetail(w, r, s, id, "Proforma not issued: "+billing.ProformaInvalidReason(err)+".")
	default:
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: issue proforma failed")
		h.renderSubscriberDetail(w, r, s, id, "Proforma not issued. Nothing was saved; try again.")
	}
}

// proformaAction runs a convert or cancel and redirects back with a code.
func (h *Handler) proformaAction(w http.ResponseWriter, r *http.Request, act func(context.Context, int) error) {
	s, ok := h.proformaGate(w, r)
	if !ok {
		return
	}
	if !canRecordPayment(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing staff and the owner can convert or cancel proformas.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid proforma id.")
		return
	}
	err = act(middleware.WithSubject(r.Context(), s.Username), id)
	code := ""
	switch {
	case err == nil:
		code = "" // set by the caller's success code below
	case errors.Is(err, billing.ErrInsufficientBalance):
		code = "short"
	case errors.Is(err, billing.ErrProformaNotOpen):
		code = "closed"
	case errors.Is(err, billing.ErrProformaExpired):
		code = "expired"
	case errors.Is(err, billing.ErrProformaNotFound):
		code = "missing"
	case errors.Is(err, billing.ErrProformaInvalid):
		code = "reason"
	default:
		log.Error().Err(err).Int("proforma_id", id).Msg("staffui: proforma action failed")
		code = "failed"
	}
	if code == "" {
		if strings.HasSuffix(r.URL.Path, "/convert") {
			code = "converted"
		} else {
			code = "cancelled"
		}
	}
	// Back to the list it was acted on from: the status tab stays as it
	// was, so a converted proforma visibly leaves the Open list.
	back := url.Values{"pf": {code}}
	if st := r.PostFormValue("from_status"); st != "" {
		back.Set("status", st)
	}
	if sub := r.PostFormValue("from_subscriber"); sub != "" {
		back.Set("subscriber", sub)
	}
	http.Redirect(w, r, "/staff/invoices/proformas?"+back.Encode(), http.StatusSeeOther)
}

// ConvertProformaForm handles POST /staff/invoices/proformas/{id}/convert.
func (h *Handler) ConvertProformaForm(w http.ResponseWriter, r *http.Request) {
	h.proformaAction(w, r, func(ctx context.Context, id int) error {
		_, err := h.proformas.ConvertProforma(ctx, id)
		return err
	})
}

// CancelProformaForm handles POST /staff/invoices/proformas/{id}/cancel.
func (h *Handler) CancelProformaForm(w http.ResponseWriter, r *http.Request) {
	h.proformaAction(w, r, func(ctx context.Context, id int) error {
		return h.proformas.CancelProforma(ctx, id, r.PostFormValue("reason"))
	})
}

// ProformaPDF handles GET /staff/invoices/proformas/{id}/pdf.
func (h *Handler) ProformaPDF(w http.ResponseWriter, r *http.Request) {
	s, ok := h.proformaGate(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid proforma id.")
		return
	}
	pdf, err := h.proformas.RenderProformaPDF(r.Context(), id)
	switch {
	case errors.Is(err, billing.ErrProformaNotFound):
		h.renderError(w, r, s, http.StatusNotFound, "No proforma with that number.")
		return
	case errors.Is(err, api.ErrPDFUnavailable):
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"PDF generation is not available on this server: no Chromium-based browser (Edge or Chrome) was found.")
		return
	case err != nil:
		log.Error().Err(err).Int("proforma_id", id).Msg("staffui: proforma PDF failed")
		h.renderError(w, r, s, http.StatusBadGateway, "The PDF could not be generated. Try again in a moment.")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s.pdf\"", billing.ProformaNumber(id)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(pdf) //nolint:errcheck,gosec // application/pdf with nosniff; see api.GetInvoicePDF
}
