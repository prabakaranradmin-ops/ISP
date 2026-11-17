//go:build integration

// Proforma invoices — FR-BIL-018 | migration 056.
//
// Defended here: who issued or converted a proforma comes from the token;
// the supplier's configured state reaches the tax calculation; a CSR can
// read proformas but cannot issue, convert or cancel one; and each refusal
// the store can raise reaches the caller as its own status and code, with
// the shortfall stated when the wallet cannot cover a conversion.
package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

type stubProformas struct {
	issued     []billing.ProformaRequest
	issuedBy   string
	homeState  string
	convertErr error
	converted  int
	convertBy  string
	cancelled  int
	cancelWhy  string
}

func (s *stubProformas) sample(id int) *billing.Proforma {
	return &billing.Proforma{ID: id, SubscriberID: 7, Username: "acme", Description: "Installation",
		Base: decimal.RequireFromString("1000.00"), CGST: decimal.RequireFromString("90.00"),
		SGST: decimal.RequireFromString("90.00"), Total: decimal.RequireFromString("1180.00"),
		ValidUntil: time.Now().AddDate(0, 0, 15), Status: billing.ProformaOpen, CreatedBy: "meena", CreatedAt: time.Now()}
}

func (s *stubProformas) CreateProforma(_ context.Context, req billing.ProformaRequest, home, by string) (*billing.Proforma, error) {
	req, err := req.Normalise()
	if err != nil {
		return nil, err
	}
	s.issued, s.issuedBy, s.homeState = append(s.issued, req), by, home
	p := s.sample(3)
	p.SubscriberID, p.Description, p.Base = req.SubscriberID, req.Description, req.Base
	return p, nil
}

func (s *stubProformas) GetProforma(_ context.Context, id int) (*billing.Proforma, error) {
	if id != 3 {
		return nil, billing.ErrProformaNotFound
	}
	return s.sample(id), nil
}

func (s *stubProformas) ListProformas(_ context.Context, f billing.ProformaFilter) ([]billing.Proforma, error) {
	return []billing.Proforma{*s.sample(3)}, nil
}

func (s *stubProformas) ConvertProforma(_ context.Context, id int, home, by string) (*billing.ConvertedProforma, error) {
	if s.convertErr != nil {
		return nil, s.convertErr
	}
	s.converted, s.convertBy, s.homeState = id, by, home
	return &billing.ConvertedProforma{ProformaID: id, InvoiceID: 41, NewBalance: decimal.RequireFromString("20.00"),
		Invoice: billing.Invoice{BaseAmount: decimal.RequireFromString("1000.00"), TotalAmount: decimal.RequireFromString("1180.00")}}, nil
}

func (s *stubProformas) CancelProforma(_ context.Context, id int, reason, _ string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: give a reason for cancelling", billing.ErrProformaInvalid)
	}
	s.cancelled, s.cancelWhy = id, reason
	return nil
}

type recordingEmitter struct{ events []string }

func (e *recordingEmitter) Emit(_ context.Context, eventType string, id int) {
	e.events = append(e.events, fmt.Sprintf("%s:%d", eventType, id))
}

func newProformaHarness(t *testing.T) (*http.ServeMux, *stubProformas, *recordingEmitter) {
	t.Helper()
	pf, ev := &stubProformas{}, &recordingEmitter{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Proformas: pf, GSTHomeState: "KA", Events: ev,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux, pf, ev
}

func TestFR_BIL_018_ProformaAPI(t *testing.T) {
	t.Run("billing issues a proforma, attributed, taxed against the configured home state", func(t *testing.T) {
		mux, pf, _ := newProformaHarness(t)
		code, body := cnDo(t, mux, "POST", "/api/v1/subscribers/7/proformas",
			`{"description":"Installation and router","amount":"1500.00","valid_days":10,"created_by":"someone-else"}`, "billing_admin", "meena")
		if code != http.StatusCreated {
			t.Fatalf("want 201, got %d: %s", code, body)
		}
		if len(pf.issued) != 1 || pf.issued[0].SubscriberID != 7 || pf.issued[0].ValidDays != 10 || pf.issuedBy != "meena" || pf.homeState != "KA" {
			t.Fatalf("issued %+v by %q home %q", pf.issued, pf.issuedBy, pf.homeState)
		}
		var got map[string]any
		_ = json.Unmarshal([]byte(body), &got)
		if got["number"] != "PF-000003" {
			t.Errorf("number: %v", got["number"])
		}
	})

	for name, body := range map[string]string{
		"no description": `{"description":" ","amount":"10"}`,
		"not a number":   `{"description":"x","amount":"ten"}`,
		"zero":           `{"description":"x","amount":"0"}`,
		"a year":         `{"description":"x","amount":"10","valid_days":365}`,
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			mux, pf, _ := newProformaHarness(t)
			if code, resp := cnDo(t, mux, "POST", "/api/v1/subscribers/7/proformas", body, "billing_admin", "meena"); code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d: %s", code, resp)
			}
			if len(pf.issued) != 0 {
				t.Error("nothing may be issued")
			}
		})
	}

	t.Run("conversion returns the tax invoice and announces it", func(t *testing.T) {
		mux, pf, ev := newProformaHarness(t)
		code, body := cnDo(t, mux, "POST", "/api/v1/proformas/3/convert", `{}`, "isp_owner", "owner")
		if code != http.StatusCreated || !strings.Contains(body, `"invoice_number":"INV-000041"`) {
			t.Fatalf("want 201 with the invoice number, got %d: %s", code, body)
		}
		if pf.converted != 3 || pf.convertBy != "owner" || pf.homeState != "KA" {
			t.Errorf("converted %d by %q home %q", pf.converted, pf.convertBy, pf.homeState)
		}
		if len(ev.events) != 1 || ev.events[0] != "invoice.generated:41" {
			t.Errorf("events: %v", ev.events)
		}
	})

	for name, tc := range map[string]struct {
		err      error
		status   int
		code     string
		mentions string
	}{
		"wallet short":  {fmt.Errorf("%w: the wallet holds ₹100.00 and the invoice is ₹1180.00", billing.ErrInsufficientBalance), 409, "ERR_INSUFFICIENT_BALANCE", "₹1180.00"},
		"already done":  {fmt.Errorf("%w: it is converted", billing.ErrProformaNotOpen), 409, "ERR_PROFORMA_CLOSED", ""},
		"expired":       {billing.ErrProformaExpired, 409, "ERR_PROFORMA_EXPIRED", "issue a new one"},
		"no such thing": {billing.ErrProformaNotFound, 404, "ERR_NOT_FOUND", ""},
		"database down": {fmt.Errorf("db: connection refused"), 500, "ERR_INTERNAL", ""},
	} {
		t.Run("conversion refused: "+name, func(t *testing.T) {
			mux, pf, ev := newProformaHarness(t)
			pf.convertErr = tc.err
			code, body := cnDo(t, mux, "POST", "/api/v1/proformas/3/convert", `{}`, "billing_admin", "meena")
			if code != tc.status || !strings.Contains(body, tc.code) || !strings.Contains(body, tc.mentions) {
				t.Errorf("want %d %s mentioning %q, got %d: %s", tc.status, tc.code, tc.mentions, code, body)
			}
			if len(ev.events) != 0 {
				t.Error("a failed conversion must announce nothing")
			}
		})
	}

	t.Run("cancelling needs a reason", func(t *testing.T) {
		mux, pf, _ := newProformaHarness(t)
		if code, _ := cnDo(t, mux, "POST", "/api/v1/proformas/3/cancel", `{"reason":""}`, "billing_admin", "meena"); code != 422 {
			t.Errorf("no reason: want 422, got %d", code)
		}
		if code, _ := cnDo(t, mux, "POST", "/api/v1/proformas/3/cancel", `{"reason":"customer declined"}`, "billing_admin", "meena"); code != 200 || pf.cancelWhy != "customer declined" {
			t.Errorf("got %d, reason %q", code, pf.cancelWhy)
		}
	})

	t.Run("a CSR reads proformas but cannot issue, convert or cancel", func(t *testing.T) {
		mux, pf, _ := newProformaHarness(t)
		if code, _ := cnDo(t, mux, "GET", "/api/v1/proformas?status=open", ``, "csr", "divya"); code != 200 {
			t.Errorf("list: %d", code)
		}
		if code, _ := cnDo(t, mux, "GET", "/api/v1/proformas/3", ``, "csr", "divya"); code != 200 {
			t.Errorf("get: %d", code)
		}
		for path, body := range map[string]string{
			"/api/v1/subscribers/7/proformas": `{"description":"x","amount":"10"}`,
			"/api/v1/proformas/3/convert":     `{}`,
			"/api/v1/proformas/3/cancel":      `{"reason":"x"}`,
		} {
			if code, _ := cnDo(t, mux, "POST", path, body, "csr", "divya"); code != http.StatusForbidden {
				t.Errorf("%s: want 403, got %d", path, code)
			}
		}
		if len(pf.issued) != 0 || pf.converted != 0 || pf.cancelled != 0 {
			t.Error("a CSR changed a proforma")
		}
	})

	t.Run("a technician cannot even read them", func(t *testing.T) {
		mux, _, _ := newProformaHarness(t)
		if code, _ := cnDo(t, mux, "GET", "/api/v1/proformas", ``, "technician", "ravi"); code != http.StatusForbidden {
			t.Errorf("want 403, got %d", code)
		}
	})

	t.Run("an unknown status filter is refused, not ignored", func(t *testing.T) {
		mux, _, _ := newProformaHarness(t)
		if code, _ := cnDo(t, mux, "GET", "/api/v1/proformas?status=paid", ``, "csr", "divya"); code != 422 {
			t.Errorf("want 422, got %d", code)
		}
	})
}

func TestFR_BIL_018_ProformaDocumentData(t *testing.T) {
	p := (&stubProformas{}).sample(3)
	p.MobileNumber = "+919800000007"
	d := api.BuildProformaData(p)
	if !d.Proforma || d.InvoiceNumber != "PF-000003" || d.LineDescription != "Installation" ||
		!d.TotalAmount.Equal(p.Total) || d.MobileNumber != "+919800000007" {
		t.Errorf("document data: %+v", d)
	}
}
