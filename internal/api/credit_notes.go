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
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/shopspring/decimal"
)

// Credit notes — FR-BIL-012 | migration 053.
//
// Issuing one is a two-person action: a request names the invoice and the
// taxable value to credit, and a different staff member approves it, at
// which point CreditNoteIssuer writes the GST document, returns the money to
// the wallet and reverses the revenue and GST in one transaction. A direct
// "issue" endpoint would be a way to move money that a plain wallet credit
// cannot take without a second approver (FR-WFL-001).

// CreditNoteIssuer writes an approved credit note. Satisfied by
// *db.BillingStore.
type CreditNoteIssuer interface {
	IssueCreditNote(ctx context.Context, approvalID, invoiceID int, base decimal.Decimal,
		reason, requestedBy, approvedBy string) (*billing.IssuedCreditNote, error)
}

// Errors RequestCreditNote reports.
var (
	ErrCreditNoteInvalid   = errors.New("api: invalid credit note request")
	ErrCreditNoteNoInvoice = errors.New("api: invoice not found")
	ErrCreditNoteUnwired   = errors.New("api: credit notes not configured")
)

// CreditNoteInvalidReason returns the sentence inside an ErrCreditNoteInvalid.
func CreditNoteInvalidReason(err error) string {
	if !errors.Is(err, ErrCreditNoteInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrCreditNoteInvalid.Error()+": ")
}

// RequestCreditNote files an approval request to credit base (taxable value)
// against an invoice. Nothing is issued until a different staff member
// approves it. The amount is checked against the invoice's taxable value now,
// so an obviously impossible request is refused at once; the authoritative
// check, against what is still uncredited, runs again inside the issuing
// transaction.
func (h *Handler) RequestCreditNote(ctx context.Context, invoiceID int, base decimal.Decimal, reason string) (*workflow.ApprovalRequest, error) {
	if h.approvals == nil || h.invoices == nil || h.creditNotes == nil {
		return nil, ErrCreditNoteUnwired
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: a reason is required; it is printed on the credit note", ErrCreditNoteInvalid)
	}
	base = base.Round(2)
	if !base.IsPositive() {
		return nil, fmt.Errorf("%w: the amount to credit must be more than zero", ErrCreditNoteInvalid)
	}
	inv, err := h.invoices.GetInvoiceDetail(ctx, invoiceID)
	if err != nil {
		return nil, fmt.Errorf("api: invoice lookup: %w", err)
	}
	if inv == nil {
		return nil, ErrCreditNoteNoInvoice
	}
	invBase, err := decimal.NewFromString(inv.BaseAmount)
	if err != nil {
		return nil, fmt.Errorf("api: invoice %d base amount: %w", invoiceID, err)
	}
	if base.GreaterThan(invBase) {
		return nil, fmt.Errorf("%w: %s is more than the invoice's taxable value of %s", ErrCreditNoteInvalid,
			base.StringFixed(2), invBase.StringFixed(2))
	}
	return h.createApprovalRequest(ctx, middleware.SubjectFromContext(ctx), workflow.ApprovalRequest{
		ActionType:   workflow.ActionCreditNote,
		SubscriberID: inv.SubscriberID,
		Amount:       &base,
		Reason:       reason,
		InvoiceID:    &invoiceID,
	})
}

// executeCreditNote is performGatedAction's credit_note branch.
func (h *Handler) executeCreditNote(ctx context.Context, req *workflow.ApprovalRequest) (*int, error) {
	if h.creditNotes == nil {
		return nil, ErrCreditNoteUnwired
	}
	if req.InvoiceID == nil || req.Amount == nil {
		// Unreachable through the API and refused by the schema
		// (chk_approval_invoice_by_type, chk_approval_amount_by_type), but
		// a nil deref in a money path is checked, not assumed.
		return nil, errors.New("credit note request has no invoice or amount")
	}
	approvedBy := ""
	if req.DecidedBy != nil {
		approvedBy = *req.DecidedBy
	}
	note, err := h.creditNotes.IssueCreditNote(ctx, req.ID, *req.InvoiceID, *req.Amount, req.Reason, req.RequestedBy, approvedBy)
	if err != nil {
		return nil, err
	}
	auditGatedAction(ctx, req, map[string]any{
		"credit_note": note.Number(), "invoice_id": note.InvoiceID,
		"total": note.Amounts.Total.String(), "ledger_txn": note.WalletLedgerID,
	})
	return &note.WalletLedgerID, nil
}

type creditNoteRequestBody struct {
	Amount string `json:"amount"`
	Reason string `json:"reason"`
}

// CreateCreditNoteRequest handles POST /api/v1/invoices/{invoice_id}/credit-notes.
// It files the approval request and answers 202; the note is issued when
// the request is approved.
func (h *Handler) CreateCreditNoteRequest(w http.ResponseWriter, r *http.Request) {
	invoiceID, err := pathInt(r, "invoice_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid invoice_id")
		return
	}
	var body creditNoteRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(body.Amount))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", "amount must be a decimal, for example 100.00")
		return
	}
	req, err := h.RequestCreditNote(r.Context(), invoiceID, amount, body.Reason)
	switch {
	case CreditNoteInvalidReason(err) != "":
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", CreditNoteInvalidReason(err))
	case errors.Is(err, ErrCreditNoteNoInvoice):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "invoice "+strconv.Itoa(invoiceID)+" not found")
	case errors.Is(err, ErrCreditNoteUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "credit notes not configured")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not file the credit note request")
	default:
		writeJSON(w, http.StatusAccepted, toApprovalResponse(req))
	}
}
