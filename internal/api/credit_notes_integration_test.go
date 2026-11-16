//go:build integration

// Credit-note request and approval — FR-BIL-012 | migration 053.
//
// Defended here: a credit note is never issued by the person who asked for
// it, the request names the invoice's own subscriber (not one the caller
// supplied), obviously impossible amounts are refused before anyone is asked
// to approve them, and approval issues exactly what was requested.
package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/shopspring/decimal"
)

type stubCreditNotes struct {
	calls        int
	gotApproval  int
	gotInvoice   int
	gotBase      decimal.Decimal
	gotRequester string
	gotApprover  string
}

func (s *stubCreditNotes) IssueCreditNote(_ context.Context, approvalID, invoiceID int, base decimal.Decimal,
	_, requestedBy, approvedBy string,
) (*billing.IssuedCreditNote, error) {
	s.calls++
	s.gotApproval, s.gotInvoice, s.gotBase, s.gotRequester, s.gotApprover = approvalID, invoiceID, base, requestedBy, approvedBy
	return &billing.IssuedCreditNote{ID: 9, InvoiceID: invoiceID, SubscriberID: 1, WalletLedgerID: 55,
		Amounts: billing.CreditNoteAmounts{Base: base, Total: base}}, nil
}

func newCreditNoteHarness(t *testing.T) (*http.ServeMux, *stubApprovals, *stubCreditNotes) {
	t.Helper()
	approvals, notes := newStubApprovals(), &stubCreditNotes{}
	invoices := &stubInvoices{detail: &api.InvoiceDetail{InvoiceSummary: api.InvoiceSummary{
		ID: 42, SubscriberID: 7, BaseAmount: "799.00", TotalAmount: "942.82",
	}}}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Approvals: approvals, Invoices: invoices, CreditNotes: notes,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux, approvals, notes
}

func cnDo(t *testing.T, mux *http.ServeMux, method, path, body, role, subject string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, subject))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestFR_BIL_012_CreditNoteRequestAndApproval(t *testing.T) {
	t.Run("a request is filed against the invoice's own subscriber and nothing is issued yet", func(t *testing.T) {
		mux, approvals, notes := newCreditNoteHarness(t)
		code, body := cnDo(t, mux, "POST", "/api/v1/invoices/42/credit-notes", `{"amount":"100.00","reason":"overcharge"}`, "billing_admin", "meena")
		if code != http.StatusAccepted {
			t.Fatalf("want 202, got %d: %s", code, body)
		}
		req := approvals.byID[1]
		if req == nil || req.ActionType != workflow.ActionCreditNote || req.SubscriberID != 7 ||
			req.InvoiceID == nil || *req.InvoiceID != 42 || req.Amount.String() != "100" || req.RequestedBy != "meena" {
			t.Fatalf("request: %+v", req)
		}
		if notes.calls != 0 {
			t.Error("requesting must not issue anything")
		}
	})

	for name, body := range map[string]string{
		"no reason":          `{"amount":"100","reason":"  "}`,
		"zero":               `{"amount":"0","reason":"x"}`,
		"more than invoiced": `{"amount":"800","reason":"x"}`,
		"not a number":       `{"amount":"lots","reason":"x"}`,
	} {
		t.Run("refused before anyone is asked: "+name, func(t *testing.T) {
			mux, approvals, _ := newCreditNoteHarness(t)
			if code, _ := cnDo(t, mux, "POST", "/api/v1/invoices/42/credit-notes", body, "billing_admin", "meena"); code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d", code)
			}
			if len(approvals.byID) != 0 {
				t.Error("no request may be filed")
			}
		})
	}

	t.Run("a CSR cannot ask", func(t *testing.T) {
		mux, _, _ := newCreditNoteHarness(t)
		if code, _ := cnDo(t, mux, "POST", "/api/v1/invoices/42/credit-notes", `{"amount":"1","reason":"x"}`, "csr", "divya"); code != http.StatusForbidden {
			t.Errorf("want 403, got %d", code)
		}
	})

	t.Run("the requester cannot approve their own credit note", func(t *testing.T) {
		mux, _, notes := newCreditNoteHarness(t)
		cnDo(t, mux, "POST", "/api/v1/invoices/42/credit-notes", `{"amount":"100","reason":"overcharge"}`, "billing_admin", "meena")
		if code, _ := cnDo(t, mux, "POST", "/api/v1/approvals/1/approve", `{}`, "billing_admin", "meena"); code != http.StatusForbidden {
			t.Errorf("self-approval: want 403, got %d", code)
		}
		if notes.calls != 0 {
			t.Error("a self-approved credit note must not be issued")
		}
	})

	t.Run("a second admin's approval issues exactly what was asked", func(t *testing.T) {
		mux, approvals, notes := newCreditNoteHarness(t)
		cnDo(t, mux, "POST", "/api/v1/invoices/42/credit-notes", `{"amount":"100.00","reason":"overcharge"}`, "billing_admin", "meena")
		code, body := cnDo(t, mux, "POST", "/api/v1/approvals/1/approve", `{}`, "isp_owner", "priya")
		if code != http.StatusOK {
			t.Fatalf("approve: want 200, got %d: %s", code, body)
		}
		if notes.calls != 1 || notes.gotApproval != 1 || notes.gotInvoice != 42 || !notes.gotBase.Equal(decimal.NewFromInt(100)) ||
			notes.gotRequester != "meena" || notes.gotApprover != "priya" {
			t.Errorf("issued: %+v", notes)
		}
		if st := approvals.byID[1].Status; st != workflow.StatusExecuted {
			t.Errorf("request status: want executed, got %s", st)
		}
		if !strings.Contains(body, `"ledger_entry_id":55`) {
			t.Errorf("the approval should record the wallet leg, got %s", body)
		}
	})
}
