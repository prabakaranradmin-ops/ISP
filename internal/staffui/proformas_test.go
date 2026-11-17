package staffui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

type fakeProformas struct {
	err       error
	issued    []billing.ProformaRequest
	actor     string
	converted int
	cancelled int
	reason    string
	list      []billing.Proforma
	gotFilter billing.ProformaFilter
}

func (f *fakeProformas) IssueProforma(ctx context.Context, req billing.ProformaRequest) (*billing.Proforma, error) {
	if _, err := req.Normalise(); err != nil {
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	f.issued, f.actor = append(f.issued, req), middleware.SubjectFromContext(ctx)
	return &billing.Proforma{ID: 1}, nil
}

func (f *fakeProformas) ConvertProforma(ctx context.Context, id int) (*billing.ConvertedProforma, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.converted, f.actor = id, middleware.SubjectFromContext(ctx)
	return &billing.ConvertedProforma{ProformaID: id, InvoiceID: 9}, nil
}

func (f *fakeProformas) CancelProforma(ctx context.Context, id int, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: give a reason", billing.ErrProformaInvalid)
	}
	if f.err != nil {
		return f.err
	}
	f.cancelled, f.reason = id, reason
	return nil
}

func (f *fakeProformas) ListProformas(_ context.Context, flt billing.ProformaFilter) ([]billing.Proforma, error) {
	f.gotFilter = flt
	return f.list, nil
}

func (f *fakeProformas) RenderProformaPDF(_ context.Context, id int) ([]byte, error) {
	if id != 1 {
		return nil, billing.ErrProformaNotFound
	}
	return []byte("%PDF-1.4"), nil
}

func servePF(h *Handler, role, method, target string, form url.Values, fn func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
	var r *http.Request
	if method == "GET" {
		r = httptest.NewRequest("GET", target, nil)
	} else {
		r = httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// /staff/subscribers/{id}/proforma and /staff/invoices/proformas/{id}/...
	if len(parts) > 3 && parts[2] == "proformas" {
		r.SetPathValue("id", parts[3])
	} else if len(parts) > 2 {
		r.SetPathValue("id", parts[2])
	}
	w := httptest.NewRecorder()
	fn(w, r)
	return w
}

func TestFR_BIL_018_IssueProformaForm(t *testing.T) {
	ok := url.Values{"description": {"Installation"}, "amount": {"1500.00"}, "valid_days": {"10"}}

	t.Run("billing issues one, attributed, and lands on that subscriber's proformas", func(t *testing.T) {
		fp := &fakeProformas{}
		h := &Handler{proformas: fp}
		w := servePF(h, "billing_admin", "POST", "/staff/subscribers/5/proforma", ok, h.IssueProformaForm)
		if w.Code != 303 || len(fp.issued) != 1 || fp.issued[0].SubscriberID != 5 || fp.issued[0].ValidDays != 10 || fp.actor != "billing_admin_user" {
			t.Fatalf("got %d issued %+v actor %q", w.Code, fp.issued, fp.actor)
		}
		if loc := w.Header().Get("Location"); loc != "/staff/invoices/proformas?subscriber=5&pf=issued" {
			t.Errorf("redirect %q", loc)
		}
	})

	t.Run("a CSR cannot issue", func(t *testing.T) {
		fp := &fakeProformas{}
		h := &Handler{proformas: fp}
		if code := servePF(h, "csr", "POST", "/staff/subscribers/5/proforma", ok, h.IssueProformaForm).Code; code != 403 || len(fp.issued) != 0 {
			t.Errorf("got %d with %d issued", code, len(fp.issued))
		}
	})

	for name, form := range map[string]url.Values{
		"not a number":   {"description": {"x"}, "amount": {"lots"}},
		"no description": {"description": {" "}, "amount": {"10"}},
		"days in words":  {"description": {"x"}, "amount": {"10"}, "valid_days": {"a week"}},
	} {
		t.Run("stays on the page: "+name, func(t *testing.T) {
			fp := &fakeProformas{}
			h := &Handler{proformas: fp}
			w := servePF(h, "isp_owner", "POST", "/staff/subscribers/5/proforma", form, h.IssueProformaForm)
			if w.Code == 303 || len(fp.issued) != 0 || !strings.Contains(w.Body.String(), "Proforma not issued") {
				t.Errorf("got %d, issued %d: %.200s", w.Code, len(fp.issued), w.Body.String())
			}
		})
	}
}

func TestFR_BIL_018_ConvertAndCancelForms(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code string
	}{
		"converted": {nil, "converted"},
		"short":     {fmt.Errorf("%w: wallet holds ₹1", billing.ErrInsufficientBalance), "short"},
		"closed":    {billing.ErrProformaNotOpen, "closed"},
		"expired":   {billing.ErrProformaExpired, "expired"},
		"missing":   {billing.ErrProformaNotFound, "missing"},
		"failed":    {fmt.Errorf("boom"), "failed"},
	} {
		t.Run("convert: "+name, func(t *testing.T) {
			fp := &fakeProformas{err: tc.err}
			h := &Handler{proformas: fp}
			w := servePF(h, "billing_admin", "POST", "/staff/invoices/proformas/4/convert",
				url.Values{"from_status": {"open"}, "from_subscriber": {"5"}}, h.ConvertProformaForm)
			loc := w.Header().Get("Location")
			if w.Code != 303 || !strings.Contains(loc, "pf="+tc.code) || !strings.Contains(loc, "status=open") || !strings.Contains(loc, "subscriber=5") {
				t.Errorf("got %d to %q", w.Code, loc)
			}
		})
	}

	t.Run("cancel carries the reason; none is refused", func(t *testing.T) {
		fp := &fakeProformas{}
		h := &Handler{proformas: fp}
		w := servePF(h, "isp_owner", "POST", "/staff/invoices/proformas/4/cancel", url.Values{"reason": {""}}, h.CancelProformaForm)
		if !strings.Contains(w.Header().Get("Location"), "pf=reason") || fp.cancelled != 0 {
			t.Errorf("no reason: %q", w.Header().Get("Location"))
		}
		w = servePF(h, "isp_owner", "POST", "/staff/invoices/proformas/4/cancel", url.Values{"reason": {"declined"}}, h.CancelProformaForm)
		if !strings.Contains(w.Header().Get("Location"), "pf=cancelled") || fp.cancelled != 4 || fp.reason != "declined" {
			t.Errorf("cancel: %q %d %q", w.Header().Get("Location"), fp.cancelled, fp.reason)
		}
	})

	t.Run("a CSR can neither convert nor cancel", func(t *testing.T) {
		fp := &fakeProformas{}
		h := &Handler{proformas: fp}
		if code := servePF(h, "csr", "POST", "/staff/invoices/proformas/4/convert", nil, h.ConvertProformaForm).Code; code != 403 {
			t.Errorf("convert: %d", code)
		}
		if code := servePF(h, "csr", "POST", "/staff/invoices/proformas/4/cancel", url.Values{"reason": {"x"}}, h.CancelProformaForm).Code; code != 403 {
			t.Errorf("cancel: %d", code)
		}
		if fp.converted+fp.cancelled != 0 {
			t.Error("a CSR changed a proforma")
		}
	})
}

func TestFR_BIL_018_ProformaList(t *testing.T) {
	now := time.Now()
	invoice := 9
	list := []billing.Proforma{
		{ID: 1, SubscriberID: 5, Username: "acme", Description: "Installation", Base: decimal.NewFromInt(1000),
			CGST: decimal.NewFromInt(90), SGST: decimal.NewFromInt(90), Total: decimal.NewFromInt(1180),
			ValidUntil: now.AddDate(0, 0, 5), Status: "open", CreatedAt: now, CreatedBy: "meena"},
		{ID: 2, SubscriberID: 5, Username: "acme", Description: "Old quote", Base: decimal.NewFromInt(10),
			Total: decimal.NewFromInt(10), ValidUntil: now.AddDate(0, 0, -3), Status: "open", CreatedAt: now},
		{ID: 3, SubscriberID: 5, Username: "acme", Description: "Done", Base: decimal.NewFromInt(10),
			Total: decimal.NewFromInt(10), ValidUntil: now, Status: "converted", InvoiceID: &invoice, CreatedAt: now},
	}

	t.Run("billing sees convert and cancel on open, unexpired proformas only", func(t *testing.T) {
		fp := &fakeProformas{list: list}
		h := &Handler{proformas: fp}
		w := servePF(h, "billing_admin", "GET", "/staff/invoices/proformas?status=all", nil, h.Proformas)
		body := w.Body.String()
		if w.Code != 200 || fp.gotFilter.Status != "" {
			t.Fatalf("got %d, filter %+v", w.Code, fp.gotFilter)
		}
		for _, want := range []string{"PF-000001", "Installation", "1180.00", "/staff/invoices/proformas/1/convert", "expired", "INV-000009"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.Contains(body, "/staff/invoices/proformas/2/convert") {
			t.Error("an expired proforma must not offer Convert")
		}
		if !strings.Contains(body, "/staff/invoices/proformas/2/cancel") {
			t.Error("an expired proforma can still be cancelled")
		}
		if strings.Contains(body, "/staff/invoices/proformas/3/convert") || strings.Contains(body, "/staff/invoices/proformas/3/cancel") {
			t.Error("a converted proforma offers no actions")
		}
	})

	t.Run("a CSR sees the list and PDFs but no actions", func(t *testing.T) {
		fp := &fakeProformas{list: list}
		h := &Handler{proformas: fp}
		body := servePF(h, "csr", "GET", "/staff/invoices/proformas", nil, h.Proformas).Body.String()
		if fp.gotFilter.Status != "open" {
			t.Errorf("default tab should be open, got %q", fp.gotFilter.Status)
		}
		if !strings.Contains(body, "/staff/invoices/proformas/1/pdf") || strings.Contains(body, "/convert") || strings.Contains(body, "/cancel") {
			t.Error("CSR view")
		}
	})

	t.Run("a technician cannot open it", func(t *testing.T) {
		h := &Handler{proformas: &fakeProformas{}}
		if code := servePF(h, "technician", "GET", "/staff/invoices/proformas", nil, h.Proformas).Code; code != 403 {
			t.Errorf("got %d", code)
		}
	})

	t.Run("the URL carries a code, never text", func(t *testing.T) {
		h := &Handler{proformas: &fakeProformas{}}
		body := servePF(h, "csr", "GET", "/staff/invoices/proformas?pf=You+have+been+hacked", nil, h.Proformas).Body.String()
		if strings.Contains(body, "hacked") {
			t.Error("text in the URL must never be shown")
		}
	})

	t.Run("an unknown proforma's PDF is a 404", func(t *testing.T) {
		h := &Handler{proformas: &fakeProformas{}}
		if code := servePF(h, "csr", "GET", "/staff/invoices/proformas/77/pdf", nil, h.ProformaPDF).Code; code != 404 {
			t.Errorf("got %d", code)
		}
	})

	t.Run("the subscriber page offers the issue form to billing only", func(t *testing.T) {
		for role, want := range map[string]bool{"billing_admin": true, "csr": false} {
			rec := httptest.NewRecorder()
			s := testSession()
			s.Role = role
			(&Handler{}).render(rec, "subscriber_detail", pageData{Session: s, Data: subscriberDetailData{
				Subscriber: &api.SubscriberRecord{ID: 5, Username: "acme", Status: "active"}, ShowBilling: true,
				ShowProforma: canRecordPayment(role), ProformaDefaultValid: billing.ProformaDefaultValid,
			}})
			if got := strings.Contains(rec.Body.String(), `action="/staff/subscribers/5/proforma"`); got != want {
				t.Errorf("%s: form shown = %v", role, got)
			}
		}
	})
}
