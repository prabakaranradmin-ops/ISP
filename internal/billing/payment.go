package billing

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Payment methods — FR-BIL-013..015 | migration 054.

// Payment methods payment_details accepts.
const (
	MethodOnline       = "online"
	MethodCash         = "cash"
	MethodCheque       = "cheque"
	MethodBankTransfer = "bank_transfer"
	MethodUPI          = "upi"
	MethodTDS          = "tds"
)

// PaymentMethods lists them in the order a form offers them.
var PaymentMethods = []string{MethodCash, MethodUPI, MethodBankTransfer, MethodCheque, MethodOnline, MethodTDS}

// PaymentDetail is how one payment was made.
type PaymentDetail struct {
	Method         string
	Reference      string // gateway id, bank UTR or UPI reference
	ChequeNumber   string
	ChequeBank     string
	ChequeDate     *time.Time
	TDSCertificate string
	RecordedBy     string
}

// ErrPaymentInvalid wraps an operator-facing sentence.
var ErrPaymentInvalid = errors.New("billing: invalid payment")

// PaymentInvalidReason returns the sentence inside an ErrPaymentInvalid.
func PaymentInvalidReason(err error) string {
	if !errors.Is(err, ErrPaymentInvalid) {
		return ""
	}
	return strings.TrimPrefix(err.Error(), ErrPaymentInvalid.Error()+": ")
}

// ErrNotBounceable is returned for a bounce against anything but a cleared
// cheque payment.
var ErrNotBounceable = errors.New("billing: only a cleared cheque payment can be marked bounced")

// Validate checks a payment has what its method needs to be traced later:
// a cheque its number, bank and date; a TDS credit its certificate. The same
// rules are CHECK constraints on payment_details; checking here turns them
// into a sentence instead of a constraint name.
func (p PaymentDetail) Validate(now time.Time) error {
	if !inMethods(p.Method) {
		return fmt.Errorf("%w: method must be one of %s", ErrPaymentInvalid, strings.Join(PaymentMethods, ", "))
	}
	switch p.Method {
	case MethodCheque:
		if strings.TrimSpace(p.ChequeNumber) == "" || strings.TrimSpace(p.ChequeBank) == "" || p.ChequeDate == nil {
			return fmt.Errorf("%w: a cheque needs its number, bank and date", ErrPaymentInvalid)
		}
		// A cheque is valid for three months from its date; one older than
		// that will be refused by the bank, so recording it as money in
		// would only set up a bounce.
		if p.ChequeDate.Before(now.AddDate(0, -3, 0)) {
			return fmt.Errorf("%w: a cheque dated over three months ago is stale and will not clear", ErrPaymentInvalid)
		}
	case MethodTDS:
		if strings.TrimSpace(p.TDSCertificate) == "" {
			return fmt.Errorf("%w: a TDS credit needs its certificate (Form 16A) number", ErrPaymentInvalid)
		}
	}
	return nil
}

// counterAccountFor is the wallet counter account a payment posts against:
// TDS Receivable for TDS, Cash / Bank (the gateway clearing account) for
// everything else, and for a recharge with no detail at all.
func counterAccountFor(p *PaymentDetail) string {
	if p != nil && p.Method == MethodTDS {
		return AccountTDSReceivable
	}
	return AccountGatewayClearing
}

func inMethods(m string) bool {
	for _, x := range PaymentMethods {
		if x == m {
			return true
		}
	}
	return false
}

// IsPaymentMethod reports whether m is a method payment_details accepts.
func IsPaymentMethod(m string) bool { return inMethods(m) }
