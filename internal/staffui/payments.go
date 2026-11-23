package staffui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// PaymentManager records payments with their method and reverses bounced
// cheques (FR-BIL-013..015). Satisfied by *api.Handler, so a payment taken
// at the counter goes through the same validation, idempotency, ledger
// posting, commission settlement and receipt as one taken over the API.
type PaymentManager interface {
	RecordPayment(ctx context.Context, subscriberID int, amount decimal.Decimal, pd billing.PaymentDetail) (*billing.Transaction, error)
	BounceChequePayment(ctx context.Context, walletLedgerID int, reason string) (*billing.Transaction, error)
}

// canRecordPayment mirrors the API's admin gate on POST /wallets/recharge
// and POST /payments/{id}/bounce.
func canRecordPayment(role string) bool { return role == "isp_owner" || role == "billing_admin" }

var methodLabels = map[string]string{
	billing.MethodCash: "Cash", billing.MethodUPI: "UPI", billing.MethodBankTransfer: "Bank transfer",
	billing.MethodCheque: "Cheque", billing.MethodOnline: "Online (gateway)", billing.MethodTDS: "TDS deducted",
}

// RecordPaymentForm handles POST /staff/subscribers/{id}/payment.
func (h *Handler) RecordPaymentForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "subscribers")
	if !ok {
		return
	}
	if !canRecordPayment(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing staff and the owner can record payments.")
		return
	}
	if h.payments == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Recording payments is not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid subscriber id.")
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(r.PostFormValue("amount")))
	if err != nil {
		h.renderSubscriberDetail(w, r, s, id, "Payment not recorded: enter the amount as a number, for example 799.00.")
		return
	}
	pd := billing.PaymentDetail{
		Method:         r.PostFormValue("method"),
		Reference:      strings.TrimSpace(r.PostFormValue("reference")),
		ChequeNumber:   strings.TrimSpace(r.PostFormValue("cheque_number")),
		ChequeBank:     strings.TrimSpace(r.PostFormValue("cheque_bank")),
		TDSCertificate: strings.TrimSpace(r.PostFormValue("tds_certificate")),
	}
	if v := r.PostFormValue("cheque_date"); v != "" {
		d, err := time.ParseInLocation("2006-01-02", v, time.Local)
		if err != nil {
			h.renderSubscriberDetail(w, r, s, id, "Payment not recorded: the cheque date is not a date.")
			return
		}
		pd.ChequeDate = &d
	}

	ctx := middleware.WithSubject(r.Context(), s.Username)
	tx, err := h.payments.RecordPayment(ctx, id, amount, pd)
	if reason := billing.PaymentInvalidReason(err); reason != "" {
		h.renderSubscriberDetail(w, r, s, id, "Payment not recorded: "+reason+".")
		return
	}
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", id).Msg("staffui: record payment failed")
		h.renderSubscriberDetail(w, r, s, id, "Payment not recorded. Nothing was credited; try again.")
		return
	}
	// G710 flags a redirect built from non-constant parts. The path is a
	// literal prefix and the two pieces are strconv.Itoa of an int and a
	// decimal fixed to two places — digits, a dot and possibly a minus sign.
	// None of those can introduce "//" or a scheme, so the target cannot
	// leave this origin.
	//nolint:gosec // G710: see above
	http.Redirect(w, r, "/staff/subscribers/"+strconv.Itoa(id)+"?paid="+tx.Amount.StringFixed(2), http.StatusSeeOther)
}

// BounceChequeForm handles POST /staff/invoices/payments/{id}/bounce.
func (h *Handler) BounceChequeForm(w http.ResponseWriter, r *http.Request) {
	s, ok := h.requireSection(w, r, "invoices")
	if !ok {
		return
	}
	if !canRecordPayment(s.Role) {
		h.renderError(w, r, s, http.StatusForbidden, "Only billing staff and the owner can mark a cheque bounced.")
		return
	}
	if h.payments == nil {
		h.renderError(w, r, s, http.StatusServiceUnavailable, "Payments are not configured on this deployment.")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		h.renderError(w, r, s, http.StatusBadRequest, "That is not a valid payment id.")
		return
	}
	ctx := middleware.WithSubject(r.Context(), s.Username)
	_, err = h.payments.BounceChequePayment(ctx, id, r.PostFormValue("reason"))
	// The outcome travels as a fixed code mapped to a sentence on the way
	// back (bounceMessages), never as text in the URL: a link that could
	// carry its own message could make the console say anything.
	code := "ok"
	switch {
	case err == nil:
	case billing.PaymentInvalidReason(err) != "":
		code = "reason"
	case errors.Is(err, billing.ErrNotBounceable):
		code = "not_cheque"
	case errors.Is(err, billing.ErrInsufficientBalance):
		code = "spent"
	default:
		log.Error().Err(err).Int("payment_id", id).Msg("staffui: bounce cheque failed")
		code = "failed"
	}
	http.Redirect(w, r, "/staff/invoices?tab=payments&from=2000-01-01&bounce="+code, http.StatusSeeOther)
}

// bounceMessages words each bounce outcome. ok is a success message; the
// rest are errors.
var bounceMessages = map[string]string{
	"ok":         "Cheque marked bounced and its value taken back out of the wallet.",
	"reason":     "Not recorded: give the bank's reason for the bounce.",
	"not_cheque": "Only a cleared cheque can be marked bounced; this payment is not one.",
	"spent": "Not recorded: the subscriber has already spent this money, so it cannot be taken back out of the wallet. " +
		"Collect the amount from them, or adjust their account, then record the bounce.",
	"failed": "Could not record the bounce. Nothing was changed.",
}
