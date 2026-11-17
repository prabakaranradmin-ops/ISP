package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/rs/zerolog/log"
)

// ReceiptRenderer prints payment receipts (FR-BIL-021). Satisfied by
// *api.Handler, so the console serves the same document as the API.
type ReceiptRenderer interface {
	RenderReceiptPDF(ctx context.Context, paymentID int) ([]byte, error)
}

// ReceiptPDF handles GET /staff/invoices/payments/{id}/receipt, for anyone
// who can see the Payments list.
func (h *Handler) ReceiptPDF(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "invoices")
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid payment id.")
		return
	}
	if h.receipts == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Receipts are not configured on this deployment.")
		return
	}
	pdf, err := h.receipts.RenderReceiptPDF(r.Context(), id)
	switch {
	case errors.Is(err, api.ErrReceiptNotFound):
		h.renderError(w, r, s, http.StatusNotFound, "No payment with that id.")
		return
	case errors.Is(err, api.ErrPDFUnavailable), errors.Is(err, api.ErrInvoiceStoreUnavailable):
		h.renderError(w, r, s, http.StatusServiceUnavailable,
			"PDF generation is not available on this server: no Chromium-based browser (Edge or Chrome) was found.")
		return
	case err != nil:
		log.Error().Err(err).Int("payment_id", id).Msg("staffui: receipt PDF failed")
		h.renderError(w, r, s, http.StatusBadGateway, "The receipt could not be generated. Try again in a moment.")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s.pdf\"", billing.ReceiptNumber(id)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(pdf) //nolint:errcheck,gosec // application/pdf with nosniff; see api.GetInvoicePDF
}
