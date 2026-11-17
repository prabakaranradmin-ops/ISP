package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// Proforma invoices — FR-BIL-018 | migration 056. See billing/proforma.go.

// ProformaStore issues, lists, converts and cancels proformas. Satisfied
// by *db.BillingStore.
type ProformaStore interface {
	CreateProforma(ctx context.Context, req billing.ProformaRequest, homeState, by string) (*billing.Proforma, error)
	GetProforma(ctx context.Context, id int) (*billing.Proforma, error)
	ListProformas(ctx context.Context, f billing.ProformaFilter) ([]billing.Proforma, error)
	ConvertProforma(ctx context.Context, id int, homeState, by string) (*billing.ConvertedProforma, error)
	CancelProforma(ctx context.Context, id int, reason, by string) error
}

// ErrProformasUnwired is returned when no proforma store is configured.
var ErrProformasUnwired = errors.New("api: proformas not configured")

func (h *Handler) homeState() string {
	if h.gstHomeState != "" {
		return h.gstHomeState
	}
	return billing.DefaultHomeState
}

// IssueProforma issues a proforma, GST estimated at today's rate.
func (h *Handler) IssueProforma(ctx context.Context, req billing.ProformaRequest) (*billing.Proforma, error) {
	if h.proformas == nil {
		return nil, ErrProformasUnwired
	}
	p, err := h.proformas.CreateProforma(ctx, req, h.homeState(), middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "proforma.issued", p.Number(), map[string]any{
		"subscriber_id": p.SubscriberID, "description": p.Description,
		"base": p.Base.StringFixed(2), "total": p.Total.StringFixed(2),
		"valid_until": p.ValidUntil.Format("2006-01-02"),
	})
	return p, nil
}

// ConvertProforma issues the tax invoice for a proforma, charging the
// wallet. The customer must have paid first.
func (h *Handler) ConvertProforma(ctx context.Context, id int) (*billing.ConvertedProforma, error) {
	if h.proformas == nil {
		return nil, ErrProformasUnwired
	}
	c, err := h.proformas.ConvertProforma(ctx, id, h.homeState(), middleware.SubjectFromContext(ctx))
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "proforma.converted", billing.ProformaNumber(id), map[string]any{
		"invoice": billing.InvoiceNumber(c.InvoiceID), "total": c.Invoice.TotalAmount.StringFixed(2),
	})
	// After the row exists, as for every invoice: a partner told an
	// invoice was generated must be able to fetch it.
	h.emit(ctx, "invoice.generated", c.InvoiceID)
	return c, nil
}

// CancelProforma withdraws an open proforma.
func (h *Handler) CancelProforma(ctx context.Context, id int, reason string) error {
	if h.proformas == nil {
		return ErrProformasUnwired
	}
	if err := h.proformas.CancelProforma(ctx, id, reason, middleware.SubjectFromContext(ctx)); err != nil {
		return err
	}
	middleware.Audit(ctx, "proforma.cancelled", billing.ProformaNumber(id), map[string]any{"reason": strings.TrimSpace(reason)})
	return nil
}

// ListProformas lists proformas newest first.
func (h *Handler) ListProformas(ctx context.Context, f billing.ProformaFilter) ([]billing.Proforma, error) {
	if h.proformas == nil {
		return nil, ErrProformasUnwired
	}
	return h.proformas.ListProformas(ctx, f)
}

// RenderProformaPDF renders a proforma document. Staff-trusted, like
// RenderInvoicePDF.
func (h *Handler) RenderProformaPDF(ctx context.Context, id int) ([]byte, error) {
	if h.proformas == nil {
		return nil, ErrProformasUnwired
	}
	if h.pdfGen == nil {
		return nil, ErrPDFUnavailable
	}
	p, err := h.proformas.GetProforma(ctx, id)
	if err != nil {
		return nil, err
	}
	return h.pdfGen.GeneratePDF(ctx, BuildProformaData(p))
}

// BuildProformaData maps a proforma onto the shared invoice document.
func BuildProformaData(p *billing.Proforma) billing.InvoiceData {
	return billing.InvoiceData{
		Proforma:        true,
		InvoiceNumber:   p.Number(),
		InvoiceDate:     p.CreatedAt,
		ValidUntil:      p.ValidUntil,
		SubscriberName:  p.Username,
		MobileNumber:    p.MobileNumber,
		RegisteredState: p.RegisteredState,
		LineDescription: p.Description,
		BaseAmount:      p.Base,
		CGSTRate:        p.CGSTRate,
		CGSTAmount:      p.CGST,
		SGSTRate:        p.SGSTRate,
		SGSTAmount:      p.SGST,
		IGSTRate:        p.IGSTRate,
		IGSTAmount:      p.IGST,
		TotalAmount:     p.Total,
	}
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

type proformaJSON struct {
	ID            int     `json:"id"`
	Number        string  `json:"number"`
	SubscriberID  int     `json:"subscriber_id"`
	Username      string  `json:"username"`
	Description   string  `json:"description"`
	Base          string  `json:"base_amount"`
	CGST          string  `json:"cgst_amount"`
	SGST          string  `json:"sgst_amount"`
	IGST          string  `json:"igst_amount"`
	Total         string  `json:"total_amount"`
	ValidUntil    string  `json:"valid_until"`
	Status        string  `json:"status"`
	CreatedBy     string  `json:"created_by"`
	CreatedAt     string  `json:"created_at"`
	InvoiceID     *int    `json:"invoice_id,omitempty"`
	InvoiceNumber *string `json:"invoice_number,omitempty"`
	CancelReason  string  `json:"cancel_reason,omitempty"`
}

func toProformaJSON(p billing.Proforma) proformaJSON {
	out := proformaJSON{
		ID: p.ID, Number: p.Number(), SubscriberID: p.SubscriberID, Username: p.Username,
		Description: p.Description, Base: p.Base.StringFixed(2), CGST: p.CGST.StringFixed(2),
		SGST: p.SGST.StringFixed(2), IGST: p.IGST.StringFixed(2), Total: p.Total.StringFixed(2),
		ValidUntil: p.ValidUntil.Format("2006-01-02"), Status: p.Status, CreatedBy: p.CreatedBy,
		CreatedAt: p.CreatedAt.Format("2006-01-02T15:04:05Z07:00"), InvoiceID: p.InvoiceID,
		CancelReason: p.CancelReason,
	}
	if p.InvoiceID != nil {
		n := billing.InvoiceNumber(*p.InvoiceID)
		out.InvoiceNumber = &n
	}
	return out
}

// writeProformaError maps a proforma failure to a status and code.
func writeProformaError(w http.ResponseWriter, err error) {
	switch {
	case billing.ProformaInvalidReason(err) != "":
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", billing.ProformaInvalidReason(err))
	case errors.Is(err, billing.ErrProformaNotOpen):
		writeError(w, http.StatusConflict, "ERR_PROFORMA_CLOSED", "this proforma has already been converted or cancelled")
	case errors.Is(err, billing.ErrProformaExpired):
		writeError(w, http.StatusConflict, "ERR_PROFORMA_EXPIRED", "this proforma is past its valid-until date; issue a new one")
	case errors.Is(err, billing.ErrInsufficientBalance):
		// The amounts are the useful part: how much is missing.
		writeError(w, http.StatusConflict, "ERR_INSUFFICIENT_BALANCE",
			strings.TrimPrefix(err.Error(), billing.ErrInsufficientBalance.Error()+": ")+"; record the customer's payment first")
	case errors.Is(err, billing.ErrProformaNotFound), isNotFound(err):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "not found")
	case errors.Is(err, ErrProformasUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "proformas not configured")
	default:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "proforma operation failed")
	}
}

type issueProformaBody struct {
	Description string `json:"description"`
	Amount      string `json:"amount"` // taxable value, before GST
	ValidDays   int    `json:"valid_days"`
}

// IssueProformaHTTP handles POST /api/v1/subscribers/{id}/proformas.
func (h *Handler) IssueProformaHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b issueProformaBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	base, err := decimal.NewFromString(strings.TrimSpace(b.Amount))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "amount must be a number, for example \"1500.00\"")
		return
	}
	p, err := h.IssueProforma(r.Context(), billing.ProformaRequest{
		SubscriberID: id, Description: b.Description, Base: base, ValidDays: b.ValidDays,
	})
	if err != nil {
		writeProformaError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, toProformaJSON(*p))
}

// ListProformasHTTP handles GET /api/v1/proformas?status=&subscriber_id=.
func (h *Handler) ListProformasHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := billing.ProformaFilter{Status: q.Get("status")}
	switch f.Status {
	case "", billing.ProformaOpen, billing.ProformaConverted, billing.ProformaCancelled:
	default:
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "status must be open, converted or cancelled")
		return
	}
	if v := q.Get("subscriber_id"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "subscriber_id must be a positive number")
			return
		}
		f.SubscriberID = n
	}
	f.Limit = parseLimit(q, 100, 500)
	list, err := h.ListProformas(r.Context(), f)
	if err != nil {
		writeProformaError(w, err)
		return
	}
	out := make([]proformaJSON, 0, len(list))
	for _, p := range list {
		out = append(out, toProformaJSON(p))
	}
	writeJSON(w, http.StatusOK, out)
}

// GetProformaHTTP handles GET /api/v1/proformas/{id}.
func (h *Handler) GetProformaHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	if h.proformas == nil {
		writeProformaError(w, ErrProformasUnwired)
		return
	}
	p, err := h.proformas.GetProforma(r.Context(), id)
	if err != nil {
		writeProformaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toProformaJSON(*p))
}

// GetProformaPDFHTTP handles GET /api/v1/proformas/{id}/pdf.
func (h *Handler) GetProformaPDFHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	pdf, err := h.RenderProformaPDF(r.Context(), id)
	switch {
	case errors.Is(err, ErrPDFUnavailable):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "PDF generation is not available (no Chromium-based browser found)")
		return
	case errors.Is(err, billing.ErrProformaNotFound), errors.Is(err, ErrProformasUnwired):
		writeProformaError(w, err)
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "ERR_GATEWAY", "PDF generation failed")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s.pdf\"", billing.ProformaNumber(id)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf) //nolint:errcheck,gosec // application/pdf with nosniff; see GetInvoicePDF
}

// ConvertProformaHTTP handles POST /api/v1/proformas/{id}/convert.
func (h *Handler) ConvertProformaHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	c, err := h.ConvertProforma(r.Context(), id)
	if err != nil {
		writeProformaError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"proforma_id": id, "proforma_number": billing.ProformaNumber(id),
		"invoice_id": c.InvoiceID, "invoice_number": billing.InvoiceNumber(c.InvoiceID),
		"base_amount": c.Invoice.BaseAmount.StringFixed(2), "cgst_amount": c.Invoice.CgstAmount.StringFixed(2),
		"sgst_amount": c.Invoice.SgstAmount.StringFixed(2), "igst_amount": c.Invoice.IgstAmount.StringFixed(2),
		"total_amount": c.Invoice.TotalAmount.StringFixed(2), "wallet_balance": c.NewBalance.StringFixed(2),
	})
}

// CancelProformaHTTP handles POST /api/v1/proformas/{id}/cancel.
func (h *Handler) CancelProformaHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	if err := h.CancelProforma(r.Context(), id, b.Reason); err != nil {
		writeProformaError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proforma_id": id, "status": billing.ProformaCancelled})
}
