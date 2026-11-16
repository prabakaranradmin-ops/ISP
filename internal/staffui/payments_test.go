package staffui

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

type fakePayments struct {
	err       error
	recorded  int
	gotSub    int
	gotAmount decimal.Decimal
	gotPD     billing.PaymentDetail
	gotActor  string
	bounced   int
	gotBounce int
}

func (f *fakePayments) RecordPayment(ctx context.Context, sub int, amount decimal.Decimal, pd billing.PaymentDetail) (*billing.Transaction, error) {
	f.recorded++
	f.gotSub, f.gotAmount, f.gotPD, f.gotActor = sub, amount, pd, middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &billing.Transaction{ID: 1, Amount: amount}, nil
}

func (f *fakePayments) BounceChequePayment(ctx context.Context, id int, _ string) (*billing.Transaction, error) {
	f.bounced++
	f.gotBounce, f.gotActor = id, middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &billing.Transaction{ID: 2}, nil
}

func servePay(h *Handler, role, target string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	w := httptest.NewRecorder()
	if strings.HasSuffix(r.URL.Path, "/bounce") {
		r.SetPathValue("id", parts[3])
		h.BounceChequeForm(w, r)
	} else {
		r.SetPathValue("id", parts[2])
		h.RecordPaymentForm(w, r)
	}
	return w
}

func TestRecordPaymentForm(t *testing.T) {
	t.Run("billing records a cheque with its details, attributed", func(t *testing.T) {
		fp := &fakePayments{}
		w := servePay(&Handler{payments: fp}, "billing_admin", "/staff/subscribers/5/payment", url.Values{
			"amount": {"1000.00"}, "method": {"cheque"}, "cheque_number": {"000123"},
			"cheque_bank": {"SBI"}, "cheque_date": {time.Now().Format("2006-01-02")},
		})
		if w.Code != 303 || fp.gotSub != 5 || !fp.gotAmount.Equal(decimal.NewFromInt(1000)) {
			t.Fatalf("got %d sub=%d amount=%v", w.Code, fp.gotSub, fp.gotAmount)
		}
		if fp.gotPD.Method != "cheque" || fp.gotPD.ChequeNumber != "000123" || fp.gotPD.ChequeDate == nil || fp.gotActor != "billing_admin_user" {
			t.Errorf("detail: %+v actor %q", fp.gotPD, fp.gotActor)
		}
		if loc := w.Header().Get("Location"); loc != "/staff/subscribers/5?paid=1000.00" {
			t.Errorf("redirect: %q", loc)
		}
	})

	t.Run("a CSR cannot record a payment", func(t *testing.T) {
		fp := &fakePayments{}
		if code := servePay(&Handler{payments: fp}, "csr", "/staff/subscribers/5/payment", url.Values{"amount": {"1"}, "method": {"cash"}}).Code; code != 403 || fp.recorded != 0 {
			t.Errorf("got %d with %d recorded", code, fp.recorded)
		}
	})

	t.Run("a non-number never reaches the wallet", func(t *testing.T) {
		fp := &fakePayments{}
		servePay(&Handler{payments: fp}, "billing_admin", "/staff/subscribers/5/payment", url.Values{"amount": {"lots"}, "method": {"cash"}})
		if fp.recorded != 0 {
			t.Error("a non-number must be refused in the console")
		}
	})
}

func TestBounceChequeForm(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"ok":         {nil, "ok"},
		"spent":      {fmt.Errorf("%w: wallet holds 0", billing.ErrInsufficientBalance), "spent"},
		"not cheque": {billing.ErrNotBounceable, "not_cheque"},
		"no reason":  {fmt.Errorf("%w: give the bank's reason", billing.ErrPaymentInvalid), "reason"},
	} {
		t.Run(name, func(t *testing.T) {
			fp := &fakePayments{err: tc.err}
			w := servePay(&Handler{payments: fp}, "isp_owner", "/staff/invoices/payments/9/bounce", url.Values{"reason": {"x"}})
			if fp.gotBounce != 9 {
				t.Fatalf("payment id: %d", fp.gotBounce)
			}
			if loc := w.Header().Get("Location"); !strings.HasSuffix(loc, "&bounce="+tc.code) {
				t.Errorf("want code %s, got %q", tc.code, loc)
			}
		})
	}

	t.Run("the URL carries a code, never text, and an unknown code shows nothing", func(t *testing.T) {
		for code := range bounceMessages {
			if strings.ContainsAny(code, " :") {
				t.Errorf("code %q looks like text", code)
			}
		}
		h := &Handler{console: &fakeConsole{}}
		r := httptest.NewRequest("GET", "/staff/invoices?tab=payments&bounce=You+have+been+hacked", nil)
		r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: "x", Role: "csr"}))
		w := httptest.NewRecorder()
		h.Money(w, r)
		if strings.Contains(w.Body.String(), "hacked") {
			t.Error("text in the URL must never be shown")
		}
	})

	t.Run("a CSR cannot mark a bounce", func(t *testing.T) {
		fp := &fakePayments{}
		if code := servePay(&Handler{payments: fp}, "csr", "/staff/invoices/payments/9/bounce", url.Values{"reason": {"x"}}).Code; code != 403 || fp.bounced != 0 {
			t.Errorf("got %d with %d calls", code, fp.bounced)
		}
	})
}

func TestRenderPaymentsWithDetails(t *testing.T) {
	h := &Handler{}
	now := time.Now()
	assertRendersOK(t, h, "money", pageData{Session: testSession(), Data: moneyData{
		Tab: "payments", CanBounce: true,
		PayTotals: &PaymentTotals{Count: 3, Amount: "1.00", TDS: "1.00", BouncedCount: 1, Bounced: "5.00"},
		Payments: []PaymentRow{
			{ID: 1, Username: "u", CreatedAt: now, Method: "cheque", Status: "cleared", ChequeNumber: "1", ChequeBank: "SBI", ChequeDate: &now, HasDetail: true},
			{ID: 2, Username: "u", CreatedAt: now, Method: "cheque", Status: "bounced", BounceReason: "funds", HasDetail: true},
			{ID: 3, Username: "u", CreatedAt: now, Method: "tds", Status: "cleared", TDSCertificate: "16A"},
		},
	}})
}
