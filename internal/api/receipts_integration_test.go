//go:build integration

// Payment receipts — FR-BIL-021. API route and gate; the payment set is
// proven against a real database in internal/db.
package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

type stubReceipts struct{}

func (stubReceipts) GetPaymentReceipt(_ context.Context, id int) (*billing.PaymentReceipt, error) {
	if id != 81 {
		return nil, nil
	}
	return &billing.PaymentReceipt{PaymentID: 81, SubscriberID: 1, Username: "acme", ReceivedAt: time.Now(),
		Amount: decimal.RequireFromString("1180"), Method: billing.MethodCash, Status: "cleared"}, nil
}

type stubReceiptPDF struct{ got billing.ReceiptData }

func (s *stubReceiptPDF) GenerateReceiptPDF(_ context.Context, d billing.ReceiptData) ([]byte, error) {
	s.got = d
	return []byte("%PDF-1.4"), nil
}

func receiptHarness(t *testing.T, gen api.ReceiptPDFGenerator) *http.ServeMux {
	t.Helper()
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Receipts: stubReceipts{}, ReceiptPDF: gen, GSTSupplier: billing.Supplier{Name: "Maaran Net", GSTIN: "33ABCDE1234F1Z5"}})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func TestFR_BIL_021_ReceiptAPI(t *testing.T) {
	t.Run("billing and the front desk can fetch a receipt, headed with the supplier", func(t *testing.T) {
		for _, role := range []string{"billing_admin", "csr", "isp_owner"} {
			gen := &stubReceiptPDF{}
			req := httptestGet(t, receiptHarness(t, gen), "/api/v1/payments/81/receipt", role)
			if req.Code != 200 || req.Header().Get("Content-Type") != "application/pdf" ||
				!strings.Contains(req.Header().Get("Content-Disposition"), "RCPT-000081.pdf") {
				t.Fatalf("%s: got %d %v", role, req.Code, req.Header())
			}
			if gen.got.SupplierName != "Maaran Net" || gen.got.SupplierGSTIN != "33ABCDE1234F1Z5" || gen.got.Number != "RCPT-000081" {
				t.Errorf("%s: receipt data %+v", role, gen.got)
			}
		}
	})

	t.Run("something that is not a payment is a 404", func(t *testing.T) {
		if code := httptestGet(t, receiptHarness(t, &stubReceiptPDF{}), "/api/v1/payments/14/receipt", "isp_owner").Code; code != 404 {
			t.Errorf("got %d", code)
		}
	})

	t.Run("no browser on the server is a 503, not a broken file", func(t *testing.T) {
		if code := httptestGet(t, receiptHarness(t, nil), "/api/v1/payments/81/receipt", "isp_owner").Code; code != 503 {
			t.Errorf("got %d", code)
		}
	})

	t.Run("a technician cannot fetch receipts", func(t *testing.T) {
		if code := httptestGet(t, receiptHarness(t, &stubReceiptPDF{}), "/api/v1/payments/81/receipt", "technician").Code; code != http.StatusForbidden {
			t.Errorf("got %d", code)
		}
	})
}

func httptestGet(t *testing.T, mux *http.ServeMux, path, role string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, role+"_user"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}
