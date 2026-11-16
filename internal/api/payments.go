package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// Payments with their method — FR-BIL-013..015 | migration 054.

// ChequeBouncer reverses a bounced cheque. Satisfied by *db.BillingStore.
type ChequeBouncer interface {
	BounceCheque(ctx context.Context, walletLedgerID int, reason, by string) (*billing.Transaction, error)
}

// ErrPaymentsUnwired is returned when the wallet service is not configured.
var ErrPaymentsUnwired = errors.New("api: payments not configured")

// RecordPayment credits a subscriber's wallet for money received, with how
// it was paid, and sends the receipt. Shared by POST /wallets/recharge and
// the console's Record payment form.
//
// A reference (bank UTR, UPI reference, gateway id) doubles as the
// idempotency key, so the same transfer recorded twice moves money once.
// Cash, cheque and TDS have no such key; the console's confirmation step is
// what guards those against a double submit.
func (h *Handler) RecordPayment(ctx context.Context, subscriberID int, amount decimal.Decimal, pd billing.PaymentDetail) (*billing.Transaction, error) {
	if h.walletSvc == nil {
		return nil, ErrPaymentsUnwired
	}
	amount = amount.Round(2)
	if !amount.IsPositive() {
		return nil, fmt.Errorf("%w: the amount must be more than zero", billing.ErrPaymentInvalid)
	}
	if err := pd.Validate(time.Now()); err != nil {
		return nil, err
	}
	pd.Reference = strings.TrimSpace(pd.Reference)
	pd.RecordedBy = middleware.SubjectFromContext(ctx)
	token := ""
	if pd.Reference != "" && (pd.Method == billing.MethodOnline || pd.Method == billing.MethodUPI || pd.Method == billing.MethodBankTransfer) {
		token = pd.Method + ":" + pd.Reference
	}
	tx, err := h.walletSvc.Recharge(ctx, billing.RechargeRequest{
		SubscriberID:     subscriberID,
		Amount:           amount,
		TransactionToken: token,
		Description:      "recharge via " + pd.Method,
		Payment:          &pd,
	})
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "wallet.recharge", strconv.Itoa(subscriberID), map[string]any{
		"amount": amount.StringFixed(2), "method": pd.Method, "reference": pd.Reference,
	})
	// TDS is not new money from the subscriber, so no franchise commission
	// is earned on it; every other method is.
	if h.franchises != nil && pd.Method != billing.MethodTDS {
		revenue.SettleCommissionForRecharge(ctx, h.franchises, subscriberID, amount, token)
	}
	h.enqueuePaymentReceipt(ctx, subscriberID, amount.StringFixed(2), tx.BalanceAfter.String())
	return tx, nil
}

// BounceChequePayment marks a cheque bounced and takes its value back out of
// the wallet. See db.BillingStore.BounceCheque for what happens when the
// money has already been spent.
func (h *Handler) BounceChequePayment(ctx context.Context, walletLedgerID int, reason string) (*billing.Transaction, error) {
	if h.cheques == nil {
		return nil, ErrPaymentsUnwired
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: give the bank's reason for the bounce", billing.ErrPaymentInvalid)
	}
	by := middleware.SubjectFromContext(ctx)
	tx, err := h.cheques.BounceCheque(ctx, walletLedgerID, reason, by)
	if err != nil {
		return nil, err
	}
	middleware.Audit(ctx, "payment.cheque_bounced", strconv.Itoa(walletLedgerID), map[string]any{
		"reason": reason, "reversal_ledger_id": tx.ID, "amount": tx.Amount.StringFixed(2),
	})
	return tx, nil
}

type bounceRequest struct {
	Reason string `json:"reason"`
}

// BounceCheque handles POST /api/v1/payments/{id}/bounce; id is the payment's
// wallet ledger id.
func (h *Handler) BounceCheque(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", "invalid id")
		return
	}
	var req bounceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "ERR_BAD_REQUEST", err.Error())
		return
	}
	tx, err := h.BounceChequePayment(r.Context(), id, req.Reason)
	switch {
	case billing.PaymentInvalidReason(err) != "":
		writeError(w, http.StatusUnprocessableEntity, "ERR_VALIDATION", billing.PaymentInvalidReason(err))
	case errors.Is(err, billing.ErrNotBounceable):
		writeError(w, http.StatusConflict, "ERR_NOT_BOUNCEABLE", billing.ErrNotBounceable.Error())
	case errors.Is(err, billing.ErrInsufficientBalance):
		writeError(w, http.StatusConflict, "ERR_ALREADY_SPENT", err.Error())
	case isNotFound(err):
		writeError(w, http.StatusNotFound, "ERR_NOT_FOUND", "no payment with that id")
	case errors.Is(err, ErrPaymentsUnwired):
		writeError(w, http.StatusServiceUnavailable, "ERR_UNAVAILABLE", "payments not configured")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "ERR_INTERNAL", "could not record the bounce")
	default:
		writeJSON(w, http.StatusOK, tx)
	}
}

// isNotFound reports a store's not-found error without importing it: every
// store wraps its own ErrNotFound whose text ends "not found".
func isNotFound(err error) bool {
	return err != nil && strings.HasSuffix(err.Error(), "not found")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
