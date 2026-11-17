package staffui

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
)

// Payment receipts — FR-BIL-021.

type fakeReceipts struct{ asked int }

func (f *fakeReceipts) RenderReceiptPDF(_ context.Context, id int) ([]byte, error) {
	f.asked = id
	if id != 81 {
		return nil, api.ErrReceiptNotFound
	}
	return []byte("%PDF-1.4"), nil
}

func TestFR_BIL_021_ReceiptsInTheConsole(t *testing.T) {
	t.Run("every payment row links its receipt, whoever can see the list", func(t *testing.T) {
		for _, canBounce := range []bool{true, false} {
			rec := httptest.NewRecorder()
			(&Handler{}).render(rec, "money", pageData{Session: testSession(), Data: moneyData{
				Tab: "payments", CanBounce: canBounce,
				Payments: []PaymentRow{{ID: 81, Username: "u", CreatedAt: time.Now(), Method: "cash", Status: "cleared"}},
			}})
			if !strings.Contains(rec.Body.String(), `/staff/invoices/payments/81/receipt`) {
				t.Errorf("canBounce=%v: no receipt link", canBounce)
			}
		}
	})

	serve := func(h *Handler, role, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
		r.SetPathValue("id", strings.Split(strings.Trim(path, "/"), "/")[3])
		w := httptest.NewRecorder()
		h.ReceiptPDF(w, r)
		return w
	}

	t.Run("the front desk downloads a receipt", func(t *testing.T) {
		f := &fakeReceipts{}
		w := serve(&Handler{receipts: f}, "csr", "/staff/invoices/payments/81/receipt")
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/pdf" ||
			!strings.Contains(w.Header().Get("Content-Disposition"), "RCPT-000081.pdf") {
			t.Errorf("got %d %v", w.Code, w.Header())
		}
	})

	t.Run("not a payment is a 404", func(t *testing.T) {
		if code := serve(&Handler{receipts: &fakeReceipts{}}, "csr", "/staff/invoices/payments/14/receipt").Code; code != 404 {
			t.Errorf("got %d", code)
		}
	})

	t.Run("a technician cannot reach it", func(t *testing.T) {
		f := &fakeReceipts{}
		if code := serve(&Handler{receipts: f}, "technician", "/staff/invoices/payments/81/receipt").Code; code != 403 || f.asked != 0 {
			t.Errorf("got %d, asked %d", code, f.asked)
		}
	})
}
