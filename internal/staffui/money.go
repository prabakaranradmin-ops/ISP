package staffui

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/rs/zerolog/log"
)

// InvoicePDFRenderer renders an invoice PDF. Satisfied by *api.Handler, so the
// console serves exactly the document the API and portal serve.
type InvoicePDFRenderer interface {
	RenderInvoicePDF(ctx context.Context, invoiceID int) ([]byte, error)
}

// maxMoneyRange caps the invoice/payment window. A year is a full GST
// return cycle; anything longer belongs in a report export.
const maxMoneyRange = 366 * 24 * time.Hour

const moneyPageLimit = 500

type moneyData struct {
	Tab       string // "invoices" or "payments"
	Query     string
	From, To  string // yyyy-mm-dd as the date inputs hold them; To is inclusive
	Invoices  []InvoiceRow
	InvTotals *InvoiceTotals
	Payments  []PaymentRow
	PayTotals *PaymentTotals
	Limit     int
	CSVQuery  string
	// CanCredit shows the per-invoice "Credit note" link (FR-BIL-012).
	CanCredit bool
}

// Money lists invoices or payments across every subscriber for a date range,
// with totals over everything matched, and exports the same list as CSV.
func (h *Handler) Money(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "invoices")
	if !ok {
		return
	}
	d := h.page(s, "Invoices & payments", "invoices")
	if h.console == nil {
		d.Error = "Invoice and payment lists are not configured on this deployment."
		h.render(w, "money", d)
		return
	}

	q := r.URL.Query()
	md := moneyData{Tab: q.Get("tab"), Query: strings.TrimSpace(q.Get("q")), Limit: moneyPageLimit,
		CanCredit: canRequestCreditNote(s.Role) && h.creditNotes != nil}
	if md.Tab != "payments" {
		md.Tab = "invoices"
	}
	from, to, msg := parseMoneyRange(q.Get("from"), q.Get("to"), time.Now())
	md.From, md.To = from.Format("2006-01-02"), to.Add(-24*time.Hour).Format("2006-01-02")
	if msg != "" {
		d.Error = msg
	}
	f := MoneyFilter{From: from, To: to, Query: md.Query, Limit: moneyPageLimit}
	asCSV := q.Get("format") == "csv"
	if asCSV {
		// An export is the whole match, not the screen's first page. Still
		// bounded: the window is capped at a year.
		f.Limit = 100000
	}

	var err error
	if md.Tab == "invoices" {
		md.Invoices, md.InvTotals, err = h.console.ListInvoices(r.Context(), f)
	} else {
		md.Payments, md.PayTotals, err = h.console.ListPayments(r.Context(), f)
	}
	if err != nil {
		log.Error().Err(err).Str("tab", md.Tab).Msg("staffui: money list failed")
		if asCSV {
			http.Error(w, "Could not load the list for export.", http.StatusInternalServerError)
			return
		}
		d.Error = "Could not load the list."
	}
	if asCSV {
		writeMoneyCSV(w, md)
		return
	}

	cq := q
	cq.Set("format", "csv")
	cq.Set("tab", md.Tab)
	cq.Set("from", md.From)
	cq.Set("to", md.To)
	md.CSVQuery = cq.Encode()
	d.Data = md
	h.render(w, "money", d)
}

// parseMoneyRange turns the date inputs into a half-open [from, to) window
// of whole local days. Blank means "this month so far". Reversed dates are
// swapped and an over-long range is clipped, each with a message saying so.
func parseMoneyRange(fromStr, toStr string, now time.Time) (time.Time, time.Time, string) {
	loc := now.Location()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	to := today.Add(24 * time.Hour)
	var msg string
	if fromStr != "" {
		if t, err := time.ParseInLocation("2006-01-02", fromStr, loc); err == nil {
			from = t
		} else {
			msg = "Dates must be in the form YYYY-MM-DD; showing this month."
		}
	}
	if toStr != "" {
		if t, err := time.ParseInLocation("2006-01-02", toStr, loc); err == nil {
			to = t.Add(24 * time.Hour)
		} else {
			msg = "Dates must be in the form YYYY-MM-DD; showing this month."
		}
	}
	if !to.After(from) {
		from, to = to.Add(-24*time.Hour), from.Add(24*time.Hour)
		msg = "The start date was after the end date, so they were swapped."
	}
	if to.Sub(from) > maxMoneyRange {
		from = to.Add(-maxMoneyRange)
		msg = "Lists cover at most a year at a time; the start date was moved. Use Reports for longer periods."
	}
	return from, to, msg
}

// csvSafe neutralises spreadsheet formula injection: a cell a spreadsheet
// would evaluate (leading =, +, -, @, tab or CR) is prefixed with a quote.
// Usernames and payment descriptions are user-influenced text, and this
// file is made to be opened in a spreadsheet.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func writeMoneyCSV(w http.ResponseWriter, md moneyData) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"%s-%s-to-%s.csv\"", md.Tab, md.From, md.To))
	cw := csv.NewWriter(w)
	if md.Tab == "invoices" {
		_ = cw.Write([]string{"invoice_number", "invoice_id", "date", "subscriber_id", "username", "base", "tax", "total", "credited"}) //nolint:errcheck
		for _, i := range md.Invoices {
			_ = cw.Write([]string{ //nolint:errcheck
				fmt.Sprintf("INV-%06d", i.ID), strconv.Itoa(i.ID), i.CreatedAt.Format("2006-01-02 15:04"),
				strconv.Itoa(i.SubscriberID), csvSafe(i.Username), i.Base, i.Tax, i.Total, i.Credited,
			})
		}
	} else {
		_ = cw.Write([]string{"payment_id", "date", "subscriber_id", "username", "amount", "method", "reference", "balance_after"}) //nolint:errcheck
		for _, p := range md.Payments {
			_ = cw.Write([]string{ //nolint:errcheck
				strconv.Itoa(p.ID), p.CreatedAt.Format("2006-01-02 15:04"), strconv.Itoa(p.SubscriberID),
				csvSafe(p.Username), p.Amount, csvSafe(p.Method), csvSafe(p.Reference), p.BalanceAfter,
			})
		}
	}
	cw.Flush()
}

// InvoicePDF serves one invoice as a PDF to staff with invoice reach.
func (h *Handler) InvoicePDF(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "invoices")
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid invoice id.")
		return
	}
	if h.invoicePDF == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Invoice PDFs are not configured on this deployment.")
		return
	}
	pdf, err := h.invoicePDF.RenderInvoicePDF(r.Context(), id)
	switch {
	case errors.Is(err, api.ErrInvoiceNotFound):
		h.renderError(w, r, s, http.StatusNotFound, "No invoice with that id.")
		return
	case errors.Is(err, api.ErrPDFUnavailable), errors.Is(err, api.ErrInvoiceStoreUnavailable):
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"PDF generation is not available on this server: no Chromium-based browser (Edge or Chrome) was found.")
		return
	case err != nil:
		log.Error().Err(err).Int("invoice_id", id).Msg("staffui: invoice PDF failed")
		h.renderError(w, r, s, http.StatusBadGateway, "The PDF could not be generated. Try again in a moment.")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"INV-%06d.pdf\"", id))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(pdf) //nolint:errcheck,gosec // served as application/pdf with nosniff; see api.GetInvoicePDF
}
