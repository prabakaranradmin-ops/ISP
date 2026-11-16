//go:build integration

// Payments with their method, TDS and cheque bounce — FR-BIL-013..015.
package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// recordingWallet keeps the postings it was asked to make, and treats a
// repeated idempotency token as already done, as the real store does.
type recordingWallet struct {
	postings []billing.RechargePosting
	tokens   map[string]*billing.Transaction
}

func (w *recordingWallet) GetTransactionByToken(_ context.Context, token string) (*billing.Transaction, error) {
	return w.tokens[token], nil
}

func (w *recordingWallet) RecordRecharge(_ context.Context, p billing.RechargePosting) (*billing.Transaction, error) {
	w.postings = append(w.postings, p)
	tx := &billing.Transaction{ID: len(w.postings), SubscriberID: p.Credit.SubscriberID, EntryType: "credit",
		Amount: p.Credit.Amount, BalanceAfter: p.NewBalance}
	if p.Credit.TransactionToken != nil {
		w.tokens[*p.Credit.TransactionToken] = tx
	}
	return tx, nil
}

func (w *recordingWallet) GetSubscriberBalance(context.Context, int) (decimal.Decimal, error) {
	return decimal.Zero, nil
}

type stubBouncer struct {
	err   error
	calls int
	gotID int
	gotBy string
}

func (s *stubBouncer) BounceCheque(_ context.Context, id int, _, by string) (*billing.Transaction, error) {
	s.calls++
	s.gotID, s.gotBy = id, by
	if s.err != nil {
		return nil, s.err
	}
	return &billing.Transaction{ID: 99, Amount: decimal.NewFromInt(1000)}, nil
}

func newPaymentMux(t *testing.T, bouncer *stubBouncer) (*http.ServeMux, *recordingWallet) {
	t.Helper()
	w := &recordingWallet{tokens: map[string]*billing.Transaction{}}
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(w), Cheques: bouncer})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux, w
}

func payDo(t *testing.T, mux *http.ServeMux, path, body, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, role+"-op"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestFR_BIL_013_RechargeRecordsTheMethod(t *testing.T) {
	today := time.Now().Format("2006-01-02")

	t.Run("a cheque carries its details into the posting", func(t *testing.T) {
		mux, w := newPaymentMux(t, nil)
		rec := payDo(t, mux, "/api/v1/wallets/recharge", fmt.Sprintf(
			`{"subscriber_id":1,"amount":"1000.00","payment_method":"cheque","cheque_number":"000123","cheque_bank":"SBI","cheque_date":%q}`, today), "billing_admin")
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
		}
		p := w.postings[0].Payment
		if p == nil || p.Method != "cheque" || p.ChequeNumber != "000123" || p.ChequeBank != "SBI" || p.ChequeDate == nil || p.RecordedBy != "billing_admin-op" {
			t.Errorf("payment detail: %+v", p)
		}
		if w.postings[0].Debit.Account != billing.AccountGatewayClearing {
			t.Errorf("a cheque posts against Cash / Bank, got %s", w.postings[0].Debit.Account)
		}
	})

	t.Run("TDS posts against TDS Receivable", func(t *testing.T) {
		mux, w := newPaymentMux(t, nil)
		payDo(t, mux, "/api/v1/wallets/recharge", `{"subscriber_id":1,"amount":"100","payment_method":"tds","tds_certificate":"16A-77"}`, "isp_owner")
		if len(w.postings) != 1 || w.postings[0].Debit.Account != billing.AccountTDSReceivable {
			t.Fatalf("postings: %+v", w.postings)
		}
	})

	for name, body := range map[string]string{
		"cheque without a bank":     fmt.Sprintf(`{"subscriber_id":1,"amount":"1","payment_method":"cheque","cheque_number":"1","cheque_date":%q}`, today),
		"tds without a certificate": `{"subscriber_id":1,"amount":"1","payment_method":"tds"}`,
		"a malformed cheque date":   `{"subscriber_id":1,"amount":"1","payment_method":"cheque","cheque_number":"1","cheque_bank":"SBI","cheque_date":"16/11/2026"}`,
	} {
		t.Run("refused with nothing posted: "+name, func(t *testing.T) {
			mux, w := newPaymentMux(t, nil)
			if rec := payDo(t, mux, "/api/v1/wallets/recharge", body, "billing_admin"); rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d: %s", rec.Code, rec.Body.String())
			}
			if len(w.postings) != 0 {
				t.Error("nothing may be posted")
			}
		})
	}

	t.Run("the same UPI reference recorded twice moves money once", func(t *testing.T) {
		mux, w := newPaymentMux(t, nil)
		body := `{"subscriber_id":1,"amount":"799","payment_method":"upi","reference":"4123-9876"}`
		payDo(t, mux, "/api/v1/wallets/recharge", body, "billing_admin")
		payDo(t, mux, "/api/v1/wallets/recharge", body, "billing_admin")
		if len(w.postings) != 1 {
			t.Errorf("want one posting, got %d", len(w.postings))
		}
	})

	t.Run("an unknown method keeps the old free-text behaviour", func(t *testing.T) {
		mux, w := newPaymentMux(t, nil)
		payDo(t, mux, "/api/v1/wallets/recharge", `{"subscriber_id":1,"amount":"10","payment_method":"voucher-shop"}`, "billing_admin")
		if len(w.postings) != 1 || w.postings[0].Payment != nil {
			t.Errorf("legacy recharge should post with no detail: %+v", w.postings)
		}
	})
}

func TestFR_BIL_014_BounceRoute(t *testing.T) {
	t.Run("billing records a bounce, attributed", func(t *testing.T) {
		b := &stubBouncer{}
		mux, _ := newPaymentMux(t, b)
		rec := payDo(t, mux, "/api/v1/payments/7/bounce", `{"reason":"insufficient funds"}`, "billing_admin")
		if rec.Code != http.StatusOK || b.gotID != 7 || b.gotBy != "billing_admin-op" {
			t.Errorf("got %d id=%d by=%q", rec.Code, b.gotID, b.gotBy)
		}
	})

	t.Run("no reason is refused before the store", func(t *testing.T) {
		b := &stubBouncer{}
		mux, _ := newPaymentMux(t, b)
		if rec := payDo(t, mux, "/api/v1/payments/7/bounce", `{"reason":" "}`, "billing_admin"); rec.Code != http.StatusUnprocessableEntity || b.calls != 0 {
			t.Errorf("got %d with %d calls", rec.Code, b.calls)
		}
	})

	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"already spent":   {fmt.Errorf("%w: wallet holds 0.00", billing.ErrInsufficientBalance), http.StatusConflict},
		"not a cheque":    {billing.ErrNotBounceable, http.StatusConflict},
		"no such payment": {fmt.Errorf("db: bounce cheque: payment 7: db: not found"), http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			mux, _ := newPaymentMux(t, &stubBouncer{err: tc.err})
			if rec := payDo(t, mux, "/api/v1/payments/7/bounce", `{"reason":"x"}`, "isp_owner"); rec.Code != tc.code {
				t.Errorf("want %d, got %d", tc.code, rec.Code)
			}
		})
	}

	t.Run("a CSR cannot", func(t *testing.T) {
		b := &stubBouncer{}
		mux, _ := newPaymentMux(t, b)
		if rec := payDo(t, mux, "/api/v1/payments/7/bounce", `{"reason":"x"}`, "csr"); rec.Code != http.StatusForbidden || b.calls != 0 {
			t.Errorf("got %d with %d calls", rec.Code, b.calls)
		}
	})
}
