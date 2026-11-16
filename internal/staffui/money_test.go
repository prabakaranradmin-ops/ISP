package staffui

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
)

type fakePDF struct {
	err   error
	gotID int
}

func (f *fakePDF) RenderInvoicePDF(_ context.Context, id int) ([]byte, error) {
	f.gotID = id
	if f.err != nil {
		return nil, f.err
	}
	return []byte("%PDF-1.7 fake"), nil
}

func serveMoney(h *Handler, role, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", target, nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	w := httptest.NewRecorder()
	if strings.HasSuffix(r.URL.Path, "/pdf") {
		r.SetPathValue("id", strings.Split(strings.Trim(r.URL.Path, "/"), "/")[2])
		h.InvoicePDF(w, r)
	} else {
		h.Money(w, r)
	}
	return w
}

func TestParseMoneyRange(t *testing.T) {
	now := time.Date(2026, 11, 16, 15, 0, 0, 0, time.Local)
	day := func(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.Local) }

	from, to, msg := parseMoneyRange("", "", now)
	if !from.Equal(day(11, 1)) || !to.Equal(day(11, 17)) || msg != "" {
		t.Errorf("blank should be this month so far, got [%v, %v) %q", from, to, msg)
	}
	from, to, _ = parseMoneyRange("2026-10-01", "2026-10-31", now)
	if !from.Equal(day(10, 1)) || !to.Equal(day(11, 1)) {
		t.Errorf("October should be [Oct 1, Nov 1), got [%v, %v)", from, to)
	}
	from, to, msg = parseMoneyRange("2020-01-01", "2026-11-16", now)
	if to.Sub(from) > maxMoneyRange || msg == "" {
		t.Errorf("an over-long range must be clipped and explained, got %v %q", to.Sub(from), msg)
	}
	if _, _, msg = parseMoneyRange("2026-11-10", "2026-11-01", now); !strings.Contains(msg, "swapped") {
		t.Errorf("reversed dates should be swapped and explained, got %q", msg)
	}
}

func TestCSVSafe(t *testing.T) {
	for in, want := range map[string]string{
		"test_user":         "test_user",
		"=HYPERLINK(\"x\")": "'=HYPERLINK(\"x\")",
		"+91":               "'+91",
		"-1":                "'-1",
		"@SUM(A1)":          "'@SUM(A1)",
		"":                  "",
		"cash":              "cash",
	} {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMoneyScreen(t *testing.T) {
	now := time.Now()
	invoices := []InvoiceRow{{ID: 42, SubscriberID: 1, Username: "test_user", CreatedAt: now, Base: "799.00", Tax: "143.82", Total: "942.82", Credited: "0.00"}}
	payments := []PaymentRow{{ID: 7, SubscriberID: 1, Username: "=cmd|' /C calc'!A0", CreatedAt: now, Amount: "799.00", BalanceAfter: "799.00", Method: "cash", Reference: "pay_123"}}

	t.Run("roles: owner, billing and CSR in; technician and NOC out", func(t *testing.T) {
		for role, want := range map[string]int{"isp_owner": 200, "billing_admin": 200, "csr": 200, "technician": 403, "noc_engineer": 403} {
			h := &Handler{console: &fakeConsole{invoices: invoices}}
			if code := serveMoney(h, role, "/staff/invoices").Code; code != want {
				t.Errorf("%s: want %d, got %d", role, want, code)
			}
		}
	})

	t.Run("invoice list shows the formatted number and PDF link", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{invoices: invoices}}
		body := serveMoney(h, "csr", "/staff/invoices").Body.String()
		for _, want := range []string{"INV-000042", "942.82", "/staff/invoices/42/pdf"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("filter reaches the store with a whole-day window", func(t *testing.T) {
		fc := &fakeConsole{}
		h := &Handler{console: fc}
		serveMoney(h, "csr", "/staff/invoices?tab=payments&from=2026-10-01&to=2026-10-31&q=+pay_123+")
		f := fc.gotMoney
		if f == nil || f.Query != "pay_123" || f.From.Day() != 1 || f.To.Month() != time.November || f.To.Day() != 1 {
			t.Fatalf("filter wrong: %+v", f)
		}
	})

	t.Run("CSV export is an attachment, unpaged, and formula-safe", func(t *testing.T) {
		fc := &fakeConsole{payments: payments}
		h := &Handler{console: fc}
		w := serveMoney(h, "billing_admin", "/staff/invoices?tab=payments&format=csv")
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
			t.Fatalf("content type %q", ct)
		}
		if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") {
			t.Errorf("should download, got %q", cd)
		}
		if fc.gotMoney.Limit <= moneyPageLimit {
			t.Errorf("an export must not be cut to one screen's page, limit %d", fc.gotMoney.Limit)
		}
		recs, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
		if err != nil || len(recs) != 2 {
			t.Fatalf("want header + 1 row, got %v err %v", recs, err)
		}
		if !strings.HasPrefix(recs[1][3], "'=") {
			t.Errorf("a username starting with = must be neutralised, got %q", recs[1][3])
		}
	})

	t.Run("a store failure is reported", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{err: errors.New("boom")}}
		if body := serveMoney(h, "csr", "/staff/invoices").Body.String(); !strings.Contains(body, "Could not load the list.") {
			t.Error("failure must be reported")
		}
		if code := serveMoney(h, "csr", "/staff/invoices?format=csv").Code; code != 500 {
			t.Errorf("a failed export must not download an empty file as success, got %d", code)
		}
	})
}

func TestInvoicePDF(t *testing.T) {
	t.Run("serves the API's PDF", func(t *testing.T) {
		fp := &fakePDF{}
		h := &Handler{invoicePDF: fp}
		w := serveMoney(h, "csr", "/staff/invoices/42/pdf")
		if w.Code != 200 || w.Header().Get("Content-Type") != "application/pdf" || fp.gotID != 42 {
			t.Fatalf("got %d %q id=%d", w.Code, w.Header().Get("Content-Type"), fp.gotID)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Error("nosniff must be set, as on the API route")
		}
	})
	t.Run("technician is refused", func(t *testing.T) {
		fp := &fakePDF{}
		h := &Handler{invoicePDF: fp}
		if code := serveMoney(h, "technician", "/staff/invoices/42/pdf").Code; code != 403 || fp.gotID != 0 {
			t.Errorf("want 403 and no render, got %d (rendered id %d)", code, fp.gotID)
		}
	})
	for name, tc := range map[string]struct {
		err  error
		code int
	}{
		"missing":     {api.ErrInvoiceNotFound, 404},
		"no browser":  {api.ErrPDFUnavailable, 503},
		"render fail": {fmt.Errorf("chromium crashed"), 502},
	} {
		t.Run(name, func(t *testing.T) {
			h := &Handler{invoicePDF: &fakePDF{err: tc.err}}
			if code := serveMoney(h, "csr", "/staff/invoices/42/pdf").Code; code != tc.code {
				t.Errorf("want %d, got %d", tc.code, code)
			}
		})
	}
}

func TestRenderMoney(t *testing.T) {
	h := &Handler{}
	now := time.Now()
	for name, d := range map[string]moneyData{
		"invoices empty": {Tab: "invoices", From: "2026-11-01", To: "2026-11-16", InvTotals: &InvoiceTotals{Base: "0.00", Tax: "0.00", Total: "0.00", Credited: "0.00"}},
		"invoices rows":  {Tab: "invoices", Invoices: []InvoiceRow{{ID: 1, Username: "u", CreatedAt: now, Credited: "10.00"}}, InvTotals: &InvoiceTotals{Count: 900}},
		"payments rows":  {Tab: "payments", Payments: []PaymentRow{{ID: 1, Username: "u", CreatedAt: now}}, PayTotals: &PaymentTotals{Count: 1, Amount: "1.00"}},
		"no totals":      {Tab: "payments"},
	} {
		t.Run(name, func(t *testing.T) {
			assertRendersOK(t, h, "money", pageData{Session: testSession(), Data: d})
		})
	}
}
