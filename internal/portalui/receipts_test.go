package portalui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// Payment receipts — FR-BIL-021. A subscriber can download a receipt for
// their own payment and nobody else's.

type stubPayments struct {
	byID     map[int]*billing.PaymentReceipt
	listedBy int
}

func (s *stubPayments) ListSubscriberPayments(_ context.Context, subscriberID, _ int) ([]billing.PaymentReceipt, error) {
	s.listedBy = subscriberID
	var out []billing.PaymentReceipt
	for _, p := range s.byID {
		if p.SubscriberID == subscriberID {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *stubPayments) GetPaymentReceipt(_ context.Context, id int) (*billing.PaymentReceipt, error) {
	return s.byID[id], nil
}

type stubReceiptPDF struct{ got billing.ReceiptData }

func (s *stubReceiptPDF) GenerateReceiptPDF(_ context.Context, d billing.ReceiptData) ([]byte, error) {
	s.got = d
	return []byte("%PDF-1.4 receipt"), nil
}

func receiptDeps() (*stubPayments, *stubReceiptPDF) {
	now := time.Now()
	return &stubPayments{byID: map[int]*billing.PaymentReceipt{
		81: {PaymentID: 81, SubscriberID: 1, Username: "testuser", ReceivedAt: now,
			Amount: decimal.RequireFromString("1180.00"), Method: billing.MethodCash, Status: "cleared"},
		82: {PaymentID: 82, SubscriberID: 1, Username: "testuser", ReceivedAt: now,
			Amount: decimal.RequireFromString("500.00"), Method: billing.MethodCheque, Status: "bounced"},
		99: {PaymentID: 99, SubscriberID: 2, Username: "someone_else", ReceivedAt: now,
			Amount: decimal.RequireFromString("799.00"), Method: billing.MethodUPI, Status: "cleared"},
	}}, &stubReceiptPDF{}
}

func getAs(t *testing.T, mux *http.ServeMux, cookie *http.Cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil) //nolint:noctx
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestFR_BIL_021_PortalReceipts(t *testing.T) {
	payments, pdf := receiptDeps()
	deps := baseTestDeps()
	deps.Payments, deps.ReceiptPDF = payments, pdf
	deps.Supplier = billing.Supplier{Name: "Maaran Net"}
	mux := newTestMuxWithDeps(t, deps)
	cookie := loginAndGetCookie(t, mux, "testuser", "testpass").Cookie

	t.Run("the payments list shows only their own, with receipt links", func(t *testing.T) {
		rec := getAs(t, mux, cookie, "/ui/invoices")
		body := rec.Body.String()
		if rec.Code != 200 || payments.listedBy != 1 {
			t.Fatalf("got %d, listed for %d", rec.Code, payments.listedBy)
		}
		for _, want := range []string{"/ui/payments/81/receipt", "RCPT-000081", "1180.00", "Cash", "cheque returned"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.Contains(body, "/ui/payments/99/receipt") {
			t.Error("another subscriber's payment was listed")
		}
	})

	t.Run("their own receipt downloads, headed with the supplier", func(t *testing.T) {
		rec := getAs(t, mux, cookie, "/ui/payments/81/receipt")
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(rec.Body.String(), "%PDF") {
			t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
		}
		if pdf.got.Number != "RCPT-000081" || pdf.got.SupplierName != "Maaran Net" ||
			pdf.got.AmountInWords != "Rupees One Thousand One Hundred Eighty Only" {
			t.Errorf("receipt data: %+v", pdf.got)
		}
	})

	t.Run("someone else's receipt is refused without saying why", func(t *testing.T) {
		pdf.got = billing.ReceiptData{}
		rec := getAs(t, mux, cookie, "/ui/payments/99/receipt")
		if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "someone_else") {
			t.Errorf("got %d: %s", rec.Code, rec.Body.String())
		}
		if pdf.got.Number != "" {
			t.Error("no document may be rendered for another subscriber's payment")
		}
	})

	t.Run("an unknown payment is a 404; no login is a 401", func(t *testing.T) {
		if code := getAs(t, mux, cookie, "/ui/payments/404/receipt").Code; code != http.StatusNotFound {
			t.Errorf("unknown: %d", code)
		}
		if code := getAs(t, mux, nil, "/ui/payments/81/receipt").Code; code != http.StatusUnauthorized {
			t.Errorf("anonymous: %d", code)
		}
	})
}
