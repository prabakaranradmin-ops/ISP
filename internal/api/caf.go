package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Customer Application Form — FR-DOC-002.
//
// Assembled here rather than in internal/billing because it draws on three
// stores — the subscriber, their plan, and their KYC record — and the
// document layer should not be reaching into any of them. billing.CAFData is
// a flat value for exactly that reason.

// CAFQuerier reads what the form prints. Satisfied by *db.APIStore.
type CAFQuerier interface {
	// GetCAFDetail returns the subscriber, plan and KYC facts the form
	// needs, or nil when there is no such subscriber.
	//
	// Deliberately one call. Three separate reads would let a form print a
	// plan the subscriber had already moved off, and a CAF is a document
	// somebody signs.
	GetCAFDetail(ctx context.Context, subscriberID int) (*billing.CAFDetail, error)
}

// CAFPDFGenerator prints the form. Satisfied by *billing.InvoicePDFClient.
type CAFPDFGenerator interface {
	GenerateCAFPDF(ctx context.Context, data billing.CAFData) ([]byte, error)
}

var (
	// ErrCAFUnavailable is returned when the form cannot be printed because
	// a dependency is missing.
	ErrCAFUnavailable = errors.New("api: the application form cannot be printed on this deployment")
	// ErrCAFSubscriberNotFound is returned for an unknown subscriber.
	ErrCAFSubscriberNotFound = errors.New("api: no such subscriber")
)

// RenderCAFPDF prints a subscriber's Customer Application Form —
// FR-DOC-002, through the invoice PDF pipeline.
//
// Staff-trusted, like RenderInvoicePDF: there is no subscriber-ownership
// check here, so only staff routes may call it. The form carries the
// customer's contact details and which documents are held, which is not
// something to serve on a portal token.
func (h *Handler) RenderCAFPDF(ctx context.Context, subscriberID int) ([]byte, error) {
	if h.cafDB == nil || h.cafPDF == nil {
		return nil, ErrCAFUnavailable
	}

	detail, err := h.cafDB.GetCAFDetail(ctx, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("api: read application form detail for subscriber %d: %w", subscriberID, err)
	}
	if detail == nil {
		return nil, ErrCAFSubscriberNotFound
	}

	data := billing.CAFData{
		Supplier:  h.gstSupplier,
		CAFNumber: detail.CAFNumber,
		FullName:  detail.FullName,
		Username:  detail.Username,

		MobileNumber:    detail.MobileNumber,
		Email:           detail.Email,
		RegisteredState: detail.RegisteredState,
		GSTIN:           detail.GSTIN,
		Franchise:       detail.Franchise,

		PlanName:     detail.PlanName,
		PlanPrice:    detail.PlanPrice,
		RateLimit:    detail.RateLimit,
		VolumeGB:     detail.VolumeGB,
		ValidityDays: detail.ValidityDays,

		KYCStatus: detail.KYCStatus,
		// Whether each document is held, never its number — see
		// billing.CAFData for why there is no field that could carry one.
		AadhaarOnRecord: detail.AadhaarOnRecord,
		PANOnRecord:     detail.PANOnRecord,
		KYCVerifiedAt:   detail.KYCVerifiedAt,

		AppliedOn: detail.CreatedAt,
		PrintedOn: time.Now(),
		PrintedBy: middleware.SubjectFromContext(ctx),
	}

	body, err := h.cafPDF.GenerateCAFPDF(ctx, data)
	if err != nil {
		return nil, err
	}

	// Printed because somebody asked for it, and the form carries the
	// customer's contact details and which identity documents are held —
	// so who printed one, and when, is worth being able to answer.
	middleware.Audit(ctx, "subscriber.caf_printed", strconv.Itoa(subscriberID), map[string]any{
		"subscriber_id": subscriberID,
		"caf_number":    detail.CAFNumber,
		"size_bytes":    len(body),
	})
	return body, nil
}
