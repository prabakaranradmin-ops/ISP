package portalui

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

type invoicesData struct {
	baseData
	Invoices []api.InvoiceSummary
	Payments []billing.PaymentReceipt
}

// PaymentLister reads a subscriber's own payments and one payment for its
// receipt (FR-BIL-021). Satisfied by *db.BillingStore.
type PaymentLister interface {
	ListSubscriberPayments(ctx context.Context, subscriberID, limit int) ([]billing.PaymentReceipt, error)
	GetPaymentReceipt(ctx context.Context, paymentID int) (*billing.PaymentReceipt, error)
}

// Invoices handles GET /ui/invoices.
func (h *Handler) Invoices(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())

	var invoices []api.InvoiceSummary
	if h.invoices != nil {
		var err error
		invoices, err = h.invoices.ListInvoices(r.Context(), subID)
		if err != nil {
			http.Error(w, "failed to load invoices", http.StatusInternalServerError)
			return
		}
	}

	var payments []billing.PaymentReceipt
	if h.payments != nil {
		var err error
		payments, err = h.payments.ListSubscriberPayments(r.Context(), subID, 50)
		if err != nil {
			http.Error(w, "failed to load payments", http.StatusInternalServerError)
			return
		}
	}

	renderPage(w, http.StatusOK, "invoices", invoicesData{
		baseData: baseData{Authenticated: true, Active: "invoices"},
		Invoices: invoices,
		Payments: payments,
	})
}

// ReceiptPDF handles GET /ui/payments/{id}/receipt: a subscriber's receipt
// for one of their own payments. Same ownership rule as InvoicePDF, with
// the same deliberately uninformative refusal.
func (h *Handler) ReceiptPDF(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())
	paymentID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || paymentID <= 0 {
		http.Error(w, "invalid payment id", http.StatusBadRequest)
		return
	}
	if h.payments == nil || h.receiptPDF == nil {
		http.Error(w, "receipts are not available", http.StatusServiceUnavailable)
		return
	}
	p, err := h.payments.GetPaymentReceipt(r.Context(), paymentID)
	if err != nil {
		http.Error(w, "failed to load payment", http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "payment not found", http.StatusNotFound)
		return
	}
	if p.SubscriberID != subID {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	pdf, err := h.receiptPDF.GenerateReceiptPDF(r.Context(), billing.NewReceiptData(*p, h.supplier))
	if err != nil {
		http.Error(w, "PDF generation failed", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s.pdf\"", billing.ReceiptNumber(p.PaymentID)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf) //nolint:errcheck,gosec // application/pdf with nosniff
}

// InvoicePDF handles GET /ui/invoices/{id}/pdf — the browsable equivalent of
// the staff-only GET /api/v1/invoices/{invoice_id}/pdf. Unlike that route,
// this one is reachable with a subscriber's own credentials, so it adds an
// ownership check the admin route intentionally has no need for.
func (h *Handler) InvoicePDF(w http.ResponseWriter, r *http.Request) {
	subID := middleware.SubscriberIDFromContext(r.Context())

	invoiceID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || invoiceID <= 0 {
		http.Error(w, "invalid invoice id", http.StatusBadRequest)
		return
	}
	if h.invoices == nil || h.pdfGen == nil {
		http.Error(w, "invoice PDF is not available", http.StatusServiceUnavailable)
		return
	}

	detail, err := h.invoices.GetInvoiceDetail(r.Context(), invoiceID)
	if err != nil {
		http.Error(w, "failed to load invoice", http.StatusInternalServerError)
		return
	}
	if detail == nil {
		http.Error(w, "invoice not found", http.StatusNotFound)
		return
	}
	if detail.SubscriberID != subID {
		// A generic "forbidden" with no further detail — it does not say
		// *why* (wrong owner vs. anything else), so it leaks nothing about
		// the invoice's contents or its actual owner.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	pdfBytes, err := h.pdfGen.GeneratePDF(r.Context(), api.BuildInvoiceData(detail))
	if err != nil {
		http.Error(w, "PDF generation failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"invoice-%d.pdf\"", detail.ID))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdfBytes) //nolint:errcheck
}
