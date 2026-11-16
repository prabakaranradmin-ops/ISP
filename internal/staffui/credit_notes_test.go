package staffui

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/shopspring/decimal"
)

type fakeCreditNotes struct {
	err      error
	calls    int
	gotID    int
	gotBase  decimal.Decimal
	gotActor string
}

func (f *fakeCreditNotes) RequestCreditNote(ctx context.Context, id int, base decimal.Decimal, reason string) (*workflow.ApprovalRequest, error) {
	f.calls++
	f.gotID, f.gotBase, f.gotActor = id, base, middleware.SubjectFromContext(ctx)
	if f.err != nil {
		return nil, f.err
	}
	return &workflow.ApprovalRequest{ID: 3, InvoiceID: &id, Amount: &base, ActionType: workflow.ActionCreditNote}, nil
}

func serveCN(h *Handler, role, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	r.SetPathValue("id", strings.Split(strings.Trim(r.URL.Path, "/"), "/")[2])
	w := httptest.NewRecorder()
	if method == "POST" {
		h.CreditNoteForm(w, r)
	} else {
		h.CreditNotePage(w, r)
	}
	return w
}

func cnHandler(fc *fakeCreditNotes) *Handler {
	inv := InvoiceRow{ID: 42, SubscriberID: 1, Username: "test_user", CreatedAt: time.Now(),
		Base: "799.00", Tax: "143.82", Total: "942.82", Credited: "118.00"}
	return &Handler{creditNotes: fc, console: &fakeConsole{invoices: []InvoiceRow{inv}}}
}

func TestCreditNoteForm(t *testing.T) {
	t.Run("billing and owner may ask; CSR may not, even by posting directly", func(t *testing.T) {
		for role, want := range map[string]int{"billing_admin": 200, "isp_owner": 200, "csr": 403} {
			fc := &fakeCreditNotes{}
			if code := serveCN(cnHandler(fc), role, "GET", "/staff/invoices/42/credit-note", nil).Code; code != want {
				t.Errorf("%s GET: want %d, got %d", role, want, code)
			}
		}
		fc := &fakeCreditNotes{}
		if code := serveCN(cnHandler(fc), "csr", "POST", "/staff/invoices/42/credit-note", url.Values{"amount": {"1"}, "reason": {"x"}}).Code; code != 403 || fc.calls != 0 {
			t.Errorf("CSR POST: want 403 and no request, got %d with %d", code, fc.calls)
		}
	})

	t.Run("the form shows what remains uncredited", func(t *testing.T) {
		body := serveCN(cnHandler(&fakeCreditNotes{}), "billing_admin", "GET", "/staff/invoices/42/credit-note", nil).Body.String()
		// 799.00 x (942.82 - 118.00) / 942.82 = 699.00
		if !strings.Contains(body, "Up to 699.00 remains") {
			t.Error("remaining taxable value should be shown")
		}
	})

	t.Run("a request is filed, attributed, and nothing is said to be issued", func(t *testing.T) {
		fc := &fakeCreditNotes{}
		body := serveCN(cnHandler(fc), "billing_admin", "POST", "/staff/invoices/42/credit-note",
			url.Values{"amount": {"100.00"}, "reason": {"overcharge"}}).Body.String()
		if fc.calls != 1 || fc.gotID != 42 || !fc.gotBase.Equal(decimal.NewFromInt(100)) || fc.gotActor != "billing_admin_user" {
			t.Fatalf("request: %+v", fc)
		}
		if !strings.Contains(body, "Nothing has been issued yet") {
			t.Error("the operator must be told it awaits a second approver")
		}
	})

	t.Run("a non-number never reaches the request", func(t *testing.T) {
		fc := &fakeCreditNotes{}
		body := serveCN(cnHandler(fc), "billing_admin", "POST", "/staff/invoices/42/credit-note",
			url.Values{"amount": {"lots"}, "reason": {"x"}}).Body.String()
		if fc.calls != 0 || !strings.Contains(body, "as a number") {
			t.Error("a non-number must be refused in the console")
		}
	})

	t.Run("a refusal from the API is shown with what was typed", func(t *testing.T) {
		fc := &fakeCreditNotes{err: fmt.Errorf("%w: 800.00 is more than the invoice's taxable value of 799.00", api.ErrCreditNoteInvalid)}
		body := serveCN(cnHandler(fc), "isp_owner", "POST", "/staff/invoices/42/credit-note",
			url.Values{"amount": {"800"}, "reason": {"keep me"}}).Body.String()
		if !strings.Contains(body, "Not requested: 800.00 is more than") || !strings.Contains(body, `value="keep me"`) {
			t.Error("reason and form text should be shown")
		}
	})

	t.Run("an unknown invoice is 404", func(t *testing.T) {
		if code := serveCN(cnHandler(&fakeCreditNotes{}), "isp_owner", "GET", "/staff/invoices/99/credit-note", nil).Code; code != 404 {
			t.Errorf("want 404, got %d", code)
		}
	})
}
