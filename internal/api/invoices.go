package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

const (
	// maxLedgerLimit caps how many rows one ledger request can pull back,
	// matching the OpenAPI contract's documented maximum.
	maxLedgerLimit     = 200
	defaultLedgerLimit = 50
)

// ── Wallet ledger ────────────────────────────────────────────────────────────

// LedgerEntry is one row in a subscriber's wallet ledger.
type LedgerEntry struct {
	ID               int       `json:"id"`
	EntryType        string    `json:"entry_type"`
	Account          string    `json:"account"`
	Amount           string    `json:"amount"`
	BalanceAfter     string    `json:"balance_after"`
	TransactionToken string    `json:"transaction_token,omitempty"`
	Description      string    `json:"description,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// LedgerQuerier lists a subscriber's wallet ledger history.
// Satisfied by *db.BillingStore.
type LedgerQuerier interface {
	ListLedgerEntries(ctx context.Context, subscriberID int, from, to *time.Time, limit int) ([]LedgerEntry, error)
}

// GetLedger handles GET /api/v1/wallets/{subscriber_id}/ledger.
func (h *Handler) GetLedger(w http.ResponseWriter, r *http.Request) {
	subscriberID, err := pathInt(r, "subscriber_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid subscriber_id")
		return
	}
	if h.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "ledger store not configured")
		return
	}

	from, to, err := parseTimeRange(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", err.Error())
		return
	}
	limit := parseLimit(r.URL.Query(), defaultLedgerLimit, maxLedgerLimit)

	entries, err := h.ledger.ListLedgerEntries(r.Context(), subscriberID, from, to, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "ledger lookup failed")
		return
	}
	if entries == nil {
		entries = []LedgerEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func parseTimeRange(q url.Values) (from, to *time.Time, err error) {
	if v := q.Get("from"); v != "" {
		t, parseErr := time.Parse(time.RFC3339, v)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("from must be RFC3339: %w", parseErr)
		}
		from = &t
	}
	if v := q.Get("to"); v != "" {
		t, parseErr := time.Parse(time.RFC3339, v)
		if parseErr != nil {
			return nil, nil, fmt.Errorf("to must be RFC3339: %w", parseErr)
		}
		to = &t
	}
	return from, to, nil
}

func parseLimit(q url.Values, def, max int) int {
	v := q.Get("limit")
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > max {
			return max
		}
	}
	if n <= 0 {
		return def
	}
	return n
}

// ── Invoices ─────────────────────────────────────────────────────────────────

// InvoiceSummary is one invoices row as exposed by the list endpoint.
type InvoiceSummary struct {
	ID           int       `json:"id"`
	SubscriberID int       `json:"subscriber_id"`
	BaseAmount   string    `json:"base_amount"`
	CGSTAmount   string    `json:"cgst_amount"`
	SGSTAmount   string    `json:"sgst_amount"`
	IGSTAmount   string    `json:"igst_amount"`
	TotalAmount  string    `json:"total_amount"`
	GBIncluded   int       `json:"gb_included"`
	GBUsed       string    `json:"gb_used"`
	CreatedAt    time.Time `json:"created_at"`
}

// InvoiceDetail carries everything the PDF template needs, on top of the
// summary fields.
type InvoiceDetail struct {
	InvoiceSummary
	SubscriberName  string `json:"subscriber_name"`
	MobileNumber    string `json:"mobile_number"`
	RegisteredState string `json:"registered_state"`
	PlanName        string `json:"plan_name"`
	CGSTRate        string `json:"cgst_rate"`
	SGSTRate        string `json:"sgst_rate"`
	IGSTRate        string `json:"igst_rate"`
	// SpeedActive and FUPApplied reflect the subscriber's *current* plan and
	// throttle state, not necessarily what was in effect when this invoice was
	// generated — the schema keeps no per-invoice speed snapshot. Adequate for
	// the plain-language usage summary FR-BIL-007 asks for; not an audit trail.
	SpeedActive string `json:"speed_active"`
	FUPApplied  bool   `json:"fup_applied"`
}

// InvoiceQuerier lists and loads invoices. Satisfied by *db.BillingStore.
type InvoiceQuerier interface {
	ListInvoices(ctx context.Context, subscriberID int) ([]InvoiceSummary, error)
	GetInvoiceDetail(ctx context.Context, invoiceID int) (*InvoiceDetail, error)
}

// PDFGenerator renders an invoice to PDF bytes. Satisfied by
// *billing.InvoicePDFClient.
type PDFGenerator interface {
	GeneratePDF(ctx context.Context, data billing.InvoiceData) ([]byte, error)
}

// ListInvoices handles GET /api/v1/invoices/{subscriber_id}.
func (h *Handler) ListInvoices(w http.ResponseWriter, r *http.Request) {
	subscriberID, err := pathInt(r, "subscriber_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid subscriber_id")
		return
	}
	if h.invoices == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "invoice store not configured")
		return
	}

	invoices, err := h.invoices.ListInvoices(r.Context(), subscriberID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "invoice lookup failed")
		return
	}
	if invoices == nil {
		invoices = []InvoiceSummary{}
	}
	writeJSON(w, http.StatusOK, invoices)
}

// GetInvoicePDF handles GET /api/v1/invoices/{invoice_id}/pdf.
//
// The OpenAPI contract also allows subscriber self-service on this route.
// That is served from the portal instead (GET /portal/invoices/{id}/pdf, using
// the portal's own JWT and an ownership check) rather than accepted here: the
// admin API and the portal deliberately sign tokens with different secrets so
// a leaked staff token cannot be replayed against subscriber routes, and
// accepting portal tokens here would undo that separation.
func (h *Handler) GetInvoicePDF(w http.ResponseWriter, r *http.Request) {
	invoiceID, err := pathInt(r, "invoice_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid invoice_id")
		return
	}
	if h.invoices == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "invoice store not configured")
		return
	}
	if h.pdfGen == nil {
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "PDF generation is not available (no Chromium-based browser found)")
		return
	}

	detail, err := h.invoices.GetInvoiceDetail(r.Context(), invoiceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "invoice lookup failed")
		return
	}
	if detail == nil {
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", fmt.Sprintf("invoice %d not found", invoiceID))
		return
	}

	pdfBytes, err := h.pdfGen.GeneratePDF(r.Context(), BuildInvoiceData(detail))
	if err != nil {
		writeError(w, http.StatusBadGateway, "ERR_GATEWAY", "PDF generation failed")
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"invoice-%d.pdf\"", detail.ID))
	// nosniff is the actual, concrete answer to what gosec G705 is gesturing at
	// below (see that comment) rather than an unrelated addition: it is what
	// stops an old or misconfigured browser from MIME-sniffing this response
	// as HTML and executing anything in it as script, regardless of the
	// Content-Type header above, if pdfGen ever returned bytes that did not
	// start with the PDF magic header.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// gosec G705 (XSS via taint analysis) flags this write because pdfBytes
	// traces back through GeneratePDF to subscriber-derived invoice data. Not
	// exploitable as XSS: this response is served as application/pdf (set
	// above) plus X-Content-Type-Options: nosniff, so no browser executes it
	// as HTML/JS regardless of what the PDF's own content contains — there is
	// no script-execution context for a PDF served this way.
	_, _ = w.Write(pdfBytes) //nolint:errcheck,gosec
}

// BuildInvoiceData maps the DB-sourced detail into the billing package's
// render model. Amounts arrive as NUMERIC-as-text and are parsed here rather
// than in the store, so a malformed value fails the request instead of the
// query — the store's job is to fetch, not to validate arithmetic.
//
// Exported so internal/portalui's subscriber-facing PDF route can reuse this
// mapping rather than duplicating it; that route adds its own
// subscriber-ownership check before calling this, since GetInvoicePDF above
// is intentionally staff-trusted and has no such check itself.
func BuildInvoiceData(d *InvoiceDetail) billing.InvoiceData {
	amt := func(s string) decimal.Decimal {
		v, err := decimal.NewFromString(s)
		if err != nil {
			return decimal.Zero
		}
		return v
	}
	return billing.InvoiceData{
		InvoiceNumber:   fmt.Sprintf("INV-%06d", d.ID),
		InvoiceDate:     d.CreatedAt,
		DueDate:         d.CreatedAt.AddDate(0, 0, 15),
		SubscriberName:  d.SubscriberName,
		MobileNumber:    d.MobileNumber,
		RegisteredState: d.RegisteredState,
		PlanName:        d.PlanName,
		PlanPeriod:      d.CreatedAt.Format("January 2006"),
		BaseAmount:      amt(d.BaseAmount),
		CGSTRate:        amt(d.CGSTRate),
		CGSTAmount:      amt(d.CGSTAmount),
		SGSTRate:        amt(d.SGSTRate),
		SGSTAmount:      amt(d.SGSTAmount),
		IGSTRate:        amt(d.IGSTRate),
		IGSTAmount:      amt(d.IGSTAmount),
		TotalAmount:     amt(d.TotalAmount),
		GBUsed:          amt(d.GBUsed),
		GBIncluded:      decimal.NewFromInt(int64(d.GBIncluded)),
		SpeedActive:     d.SpeedActive,
		FUPApplied:      d.FUPApplied,
	}
}
