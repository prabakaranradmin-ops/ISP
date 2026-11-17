package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

// Payment receipts — FR-BIL-021.

// ReceiptStore loads payments for their receipts. Satisfied by
// *db.BillingStore.
type ReceiptStore interface {
	GetPaymentReceipt(ctx context.Context, paymentID int) (*billing.PaymentReceipt, error)
}

// ReceiptPDFGenerator prints a receipt. Satisfied by
// *billing.InvoicePDFClient; separate from PDFGenerator so the many invoice
// stubs need not grow a method they never use.
type ReceiptPDFGenerator interface {
	GenerateReceiptPDF(ctx context.Context, data billing.ReceiptData) ([]byte, error)
}

// ErrReceiptNotFound: the id is not a payment.
var ErrReceiptNotFound = errors.New("api: no payment with that id")

// RenderReceiptPDF prints the receipt for one payment. Staff-trusted, like
// RenderInvoicePDF: the portal checks ownership before calling it.
func (h *Handler) RenderReceiptPDF(ctx context.Context, paymentID int) ([]byte, error) {
	if h.receipts == nil {
		return nil, ErrInvoiceStoreUnavailable
	}
	if h.receiptPDF == nil {
		return nil, ErrPDFUnavailable
	}
	p, err := h.receipts.GetPaymentReceipt(ctx, paymentID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvoiceLookup, err)
	}
	if p == nil {
		return nil, ErrReceiptNotFound
	}
	return h.receiptPDF.GenerateReceiptPDF(ctx, billing.NewReceiptData(*p, h.gstSupplier))
}

// GetReceiptPDFHTTP handles GET /api/v1/payments/{id}/receipt.
func (h *Handler) GetReceiptPDFHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	pdf, err := h.RenderReceiptPDF(r.Context(), id)
	switch {
	case errors.Is(err, ErrInvoiceStoreUnavailable):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "payment store not configured")
		return
	case errors.Is(err, ErrPDFUnavailable):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "PDF generation is not available (no Chromium-based browser found)")
		return
	case errors.Is(err, ErrReceiptNotFound):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no payment with that id")
		return
	case errors.Is(err, errInvoiceLookup):
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "payment lookup failed")
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "ERR_GATEWAY", "PDF generation failed")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s.pdf\"", billing.ReceiptNumber(id)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf) //nolint:errcheck,gosec // application/pdf with nosniff; see GetInvoicePDF
}
