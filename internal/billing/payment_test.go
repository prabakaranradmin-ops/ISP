package billing

import (
	"errors"
	"testing"
	"time"
)

func TestPaymentDetailValidate(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	today := now
	stale := now.AddDate(0, -3, -1)

	cases := []struct {
		name string
		p    PaymentDetail
		ok   bool
	}{
		{"cash needs nothing more", PaymentDetail{Method: MethodCash}, true},
		{"upi with a reference", PaymentDetail{Method: MethodUPI, Reference: "4123"}, true},
		{"unknown method", PaymentDetail{Method: "barter"}, false},
		{"cheque complete", PaymentDetail{Method: MethodCheque, ChequeNumber: "000123", ChequeBank: "SBI", ChequeDate: &today}, true},
		{"cheque without bank", PaymentDetail{Method: MethodCheque, ChequeNumber: "000123", ChequeDate: &today}, false},
		{"cheque without date", PaymentDetail{Method: MethodCheque, ChequeNumber: "000123", ChequeBank: "SBI"}, false},
		{"stale cheque", PaymentDetail{Method: MethodCheque, ChequeNumber: "1", ChequeBank: "SBI", ChequeDate: &stale}, false},
		{"tds with certificate", PaymentDetail{Method: MethodTDS, TDSCertificate: "16A-2026-77"}, true},
		{"tds without certificate", PaymentDetail{Method: MethodTDS}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate(now)
			if tc.ok && err != nil {
				t.Errorf("want ok, got %v", err)
			}
			if !tc.ok {
				if !errors.Is(err, ErrPaymentInvalid) || PaymentInvalidReason(err) == "" {
					t.Errorf("want a worded ErrPaymentInvalid, got %v", err)
				}
			}
		})
	}
}

func TestCounterAccountFor(t *testing.T) {
	if got := counterAccountFor(&PaymentDetail{Method: MethodTDS}); got != AccountTDSReceivable {
		t.Errorf("TDS must post to TDS Receivable, got %s", got)
	}
	for _, m := range []string{MethodCash, MethodCheque, MethodUPI, MethodBankTransfer, MethodOnline} {
		if got := counterAccountFor(&PaymentDetail{Method: m}); got != AccountGatewayClearing {
			t.Errorf("%s must post to Cash / Bank, got %s", m, got)
		}
	}
	if got := counterAccountFor(nil); got != AccountGatewayClearing {
		t.Errorf("a recharge with no detail keeps Cash / Bank, got %s", got)
	}
	if GLAccountCode(AccountTDSReceivable) != "1300" {
		t.Error("TDS Receivable must map to GL 1300")
	}
}
