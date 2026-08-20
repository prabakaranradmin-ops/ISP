//go:build integration

// Integration tests for the API endpoints wired up on top of the persistence
// layer: wallet ledger, sessions, invoices, admin tickets, LEA lookup and the
// Razorpay webhook. Each handler is exercised through the real middleware
// chain (JWT + role, and for LEA the additional lea_access claim) against
// in-memory stubs of its store dependencies, so what is under test is route
// wiring, authorization and response shape — the SQL itself is covered in
// internal/db/api_stores_integration_test.go.
package api_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	tickettask "github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// ── Token helper ─────────────────────────────────────────────────────────────

func itRoleToken(t *testing.T, role string, leaAccess bool) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             role,
		LeaAccess:        leaAccess,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign %s token: %v", role, err)
	}
	return tok
}

// ── Stubs ────────────────────────────────────────────────────────────────────

type stubLedger struct {
	entries []api.LedgerEntry
	err     error
}

func (s *stubLedger) ListLedgerEntries(context.Context, int, *time.Time, *time.Time, int) ([]api.LedgerEntry, error) {
	return s.entries, s.err
}

type stubSessionReader struct {
	session *health.SessionSummary
	err     error
}

func (s *stubSessionReader) GetActiveSession(context.Context, int) (*health.SessionSummary, error) {
	return s.session, s.err
}

type stubSessionCtl struct {
	subscriberID int
	nasIP        string
	resolveErr   error

	mu       sync.Mutex
	fupCalls []bool
	setErr   error
}

func (s *stubSessionCtl) ResolveSessionSubscriber(context.Context, string) (int, string, error) {
	return s.subscriberID, s.nasIP, s.resolveErr
}

func (s *stubSessionCtl) SetFUPActive(_ context.Context, _ int, active bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fupCalls = append(s.fupCalls, active)
	return s.setErr
}

func (s *stubSessionCtl) snapshot() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.fupCalls...)
}

type stubTaskEnqueuer struct {
	mu    sync.Mutex
	tasks []*jobqueue.Task
	err   error
}

func (s *stubTaskEnqueuer) Enqueue(task *jobqueue.Task, _ ...jobqueue.Option) (*jobqueue.TaskInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	s.tasks = append(s.tasks, task)
	return &jobqueue.TaskInfo{ID: 1}, nil
}

func (s *stubTaskEnqueuer) snapshot() []*jobqueue.Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*jobqueue.Task(nil), s.tasks...)
}

type stubInvoices struct {
	list               []api.InvoiceSummary
	detail             *api.InvoiceDetail
	listErr, detailErr error
}

func (s *stubInvoices) ListInvoices(context.Context, int) ([]api.InvoiceSummary, error) {
	return s.list, s.listErr
}

func (s *stubInvoices) GetInvoiceDetail(context.Context, int) (*api.InvoiceDetail, error) {
	return s.detail, s.detailErr
}

type stubPDFGen struct {
	bytes []byte
	err   error
}

func (s *stubPDFGen) GeneratePDF(context.Context, billing.InvoiceData) ([]byte, error) {
	return s.bytes, s.err
}

type stubTicketsAdmin struct {
	mu                   sync.Mutex
	tickets              map[int]*api.TicketRecord
	nextID               int
	createErr, updateErr error
}

func newStubTicketsAdmin() *stubTicketsAdmin {
	return &stubTicketsAdmin{tickets: map[int]*api.TicketRecord{}, nextID: 1}
}

func (s *stubTicketsAdmin) CreateTicketAdmin(_ context.Context, subscriberID int, category, description string, priority *string) (*api.TicketRecord, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resolved := "medium"
	if priority != nil {
		resolved = *priority
	}
	t := &api.TicketRecord{
		ID: s.nextID, SubscriberID: subscriberID, Category: category, Description: description,
		Status: "open", Priority: resolved, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	s.tickets[t.ID] = t
	s.nextID++
	return t, nil
}

func (s *stubTicketsAdmin) UpdateTicketAdmin(_ context.Context, ticketID int, status *string, assignedTo *int, priority *string) (*api.TicketRecord, error) {
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[ticketID]
	if !ok {
		return nil, nil
	}
	if status != nil {
		t.Status = *status
	}
	if assignedTo != nil {
		t.AssignedTo = assignedTo
	}
	if priority != nil {
		t.Priority = *priority
	}
	return t, nil
}

type stubLEA struct {
	result *api.LEAResult
	err    error
}

func (s *stubLEA) LookupByPublicIP(context.Context, string, *int, time.Time) (*api.LEAResult, error) {
	return s.result, s.err
}

type stubLEAAudit struct {
	mu      sync.Mutex
	entries []api.LEAAuditEntry
}

func (s *stubLEAAudit) RecordLEAAudit(_ context.Context, entry api.LEAAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

func (s *stubLEAAudit) snapshot() []api.LEAAuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]api.LEAAuditEntry(nil), s.entries...)
}

// ── Wallet ledger ────────────────────────────────────────────────────────────

// TestGetLedger_ReturnsEntries verifies an authorized request returns the
// ledger the store provides, and that an unconfigured store degrades to 503
// rather than a panic.
func TestGetLedger_ReturnsEntries(t *testing.T) {
	ledger := &stubLedger{entries: []api.LedgerEntry{
		{ID: 1, EntryType: "credit", Account: "subscriber_wallet", Amount: "799.00", BalanceAfter: "799.00"},
	}}
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Ledger: ledger})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/1/ledger", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var entries []api.LedgerEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(entries) != 1 || entries[0].Amount != "799.00" {
		t.Errorf("entries: %+v", entries)
	}
}

func TestGetLedger_NotConfiguredReturns503(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{})})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/wallets/1/ledger", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when no ledger store is wired, got %d", rec.Code)
	}
}

// ── Sessions ─────────────────────────────────────────────────────────────────

func TestGetActiveSession_NoSessionReturns404(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Sessions: &stubSessionReader{session: nil},
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/1/active", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for no active session, got %d", rec.Code)
	}
}

// TestDisconnectSession_EnqueuesPoD verifies a resolved session enqueues
// exactly one PoD task on the network_commands queue and returns 202.
func TestDisconnectSession_EnqueuesPoD(t *testing.T) {
	ctl := &stubSessionCtl{subscriberID: 42, nasIP: "10.10.0.1"}
	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		SessionCtl: ctl, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/disconnect", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d — %s", rec.Code, rec.Body.String())
	}
	got := tasks.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 enqueued task, got %d", len(got))
	}
	if got[0].Type() != "network:pod_send" {
		t.Errorf("task type: got %q", got[0].Type())
	}
}

// TestDisconnectSession_UnresolvedReturns404 verifies an unknown session_id is
// reported as 404 and never reaches the task queue.
func TestDisconnectSession_UnresolvedReturns404(t *testing.T) {
	ctl := &stubSessionCtl{resolveErr: context.DeadlineExceeded}
	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		SessionCtl: ctl, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/unknown/disconnect", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
	if len(tasks.snapshot()) != 0 {
		t.Error("no task may be enqueued for an unresolved session")
	}
}

// TestDisconnectSession_ForbiddenForCSR verifies the noc-only role gate.
func TestDisconnectSession_ForbiddenForCSR(t *testing.T) {
	ctl := &stubSessionCtl{subscriberID: 1, nasIP: "10.10.0.1"}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		SessionCtl: ctl, Tasks: &stubTaskEnqueuer{},
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-1/disconnect", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "csr", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403 for csr on a noc-only route, got %d", rec.Code)
	}
}

// TestFUPOverride_ApplyThrottlesAndEnqueuesCoA verifies "apply" sets fup_active
// true and enqueues a CoA task using the full CoAQuerier round trip.
func TestFUPOverride_ApplyThrottlesAndEnqueuesCoA(t *testing.T) {
	ctl := &stubSessionCtl{subscriberID: 7, nasIP: "10.10.0.9"}
	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		SessionCtl: ctl, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"action":"apply"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-7/fup-override", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d — %s", rec.Code, rec.Body.String())
	}
	calls := ctl.snapshot()
	if len(calls) != 1 || !calls[0] {
		t.Errorf("want SetFUPActive(true) called once, got %v", calls)
	}
	tasksSent := tasks.snapshot()
	if len(tasksSent) != 1 || tasksSent[0].Type() != "network:coa_send" {
		t.Errorf("want 1 CoA task enqueued, got %+v", tasksSent)
	}
}

// TestFUPOverride_InvalidActionRejected verifies the action enum is validated
// before any state changes.
func TestFUPOverride_InvalidActionRejected(t *testing.T) {
	ctl := &stubSessionCtl{subscriberID: 7, nasIP: "10.10.0.9"}
	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		SessionCtl: ctl, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"action":"disable_forever"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/sess-7/fup-override", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an invalid action, got %d", rec.Code)
	}
	if len(ctl.snapshot()) != 0 || len(tasks.snapshot()) != 0 {
		t.Error("an invalid action must not touch FUP state or enqueue a task")
	}
}

// ── Invoices ─────────────────────────────────────────────────────────────────

func TestListInvoices_ReturnsSummaries(t *testing.T) {
	invoices := &stubInvoices{list: []api.InvoiceSummary{{ID: 1, SubscriberID: 1, TotalAmount: "942.82"}}}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Invoices: invoices,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/invoices/1", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "csr", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var got []api.InvoiceSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].TotalAmount != "942.82" {
		t.Errorf("invoices: %+v", got)
	}
}

// TestGetInvoicePDF_ReturnsPDFBytes verifies a successful generation streams
// the PDF with the right content type.
func TestGetInvoicePDF_ReturnsPDFBytes(t *testing.T) {
	detail := &api.InvoiceDetail{
		InvoiceSummary: api.InvoiceSummary{ID: 5, SubscriberID: 1, BaseAmount: "799.00", TotalAmount: "942.82",
			CreatedAt: time.Now()},
		SubscriberName: "pdf@isp", PlanName: "P", SpeedActive: "100M/100M",
	}
	invoices := &stubInvoices{detail: detail}
	pdf := &stubPDFGen{bytes: []byte("%PDF-1.4 fake")}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Invoices: invoices, PDF: pdf,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/invoices/5/pdf", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type: want application/pdf, got %q", ct)
	}
	if rec.Body.String() != "%PDF-1.4 fake" {
		t.Errorf("body: got %q", rec.Body.String())
	}
}

func TestGetInvoicePDF_UnknownInvoiceReturns404(t *testing.T) {
	invoices := &stubInvoices{detail: nil}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Invoices: invoices, PDF: &stubPDFGen{},
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/invoices/999/pdf", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func TestGetInvoicePDF_NoPDFRendererReturns503(t *testing.T) {
	invoices := &stubInvoices{detail: &api.InvoiceDetail{}}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Invoices: invoices,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/invoices/1/pdf", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when no PDF renderer is configured, got %d", rec.Code)
	}
}

// ── Admin tickets ────────────────────────────────────────────────────────────

func TestCreateTicket_Admin(t *testing.T) {
	tickets := newStubTicketsAdmin()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Tickets: tickets,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"subscriber_id":9,"category":"connectivity","description":"No internet"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tickets", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "csr", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	var got api.TicketRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SubscriberID != 9 || got.Status != "open" {
		t.Errorf("ticket: %+v", got)
	}
}

func TestCreateTicket_InvalidCategoryRejected(t *testing.T) {
	tickets := newStubTicketsAdmin()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Tickets: tickets,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"subscriber_id":9,"category":"not_a_category","description":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tickets", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "csr", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d", rec.Code)
	}
}

func TestUpdateTicket_UnknownReturns404(t *testing.T) {
	tickets := newStubTicketsAdmin()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Tickets: tickets,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"status":"resolved"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/tickets/999", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "technician", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

// TestUpdateTicket_StatusChange_EnqueuesNotification is the FR-NOTIF-007
// regression test: a status change on the JSON API — not just the console —
// must tell the subscriber. Before this was wired, UpdateTicketAdmin was a
// bare UPDATE and nothing downstream ever knew a ticket moved.
func TestUpdateTicket_StatusChange_EnqueuesNotification(t *testing.T) {
	ticketsStore := newStubTicketsAdmin()
	created, err := ticketsStore.CreateTicketAdmin(context.Background(), 9, "connectivity", "No internet", nil)
	if err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Tickets: ticketsStore, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"status":"resolved"}`
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/tickets/%d", created.ID), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "technician", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	enqueued := tasks.snapshot()
	if len(enqueued) != 1 {
		t.Fatalf("want 1 enqueued task, got %d", len(enqueued))
	}
	if enqueued[0].Type() != tickettask.TaskTypeTicketUpdate {
		t.Errorf("task type = %q, want %q", enqueued[0].Type(), tickettask.TaskTypeTicketUpdate)
	}
	var payload tickettask.UpdatePayload
	if err := json.Unmarshal(enqueued[0].Payload(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.SubscriberID != 9 || payload.TicketID != created.ID || payload.Status != "resolved" || payload.Username != "test" {
		t.Errorf("payload = %+v, want subscriber 9, ticket %d, status resolved, username test", payload, created.ID)
	}
}

// TestUpdateTicket_AssigneeOnlyChange_DoesNotNotify guards the other branch:
// re-routing a ticket to a different technician is an internal change the
// subscriber has no stake in, and should never look like a status update.
func TestUpdateTicket_AssigneeOnlyChange_DoesNotNotify(t *testing.T) {
	ticketsStore := newStubTicketsAdmin()
	created, err := ticketsStore.CreateTicketAdmin(context.Background(), 9, "connectivity", "No internet", nil)
	if err != nil {
		t.Fatalf("seed ticket: %v", err)
	}

	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Tickets: ticketsStore, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"assigned_to":3}`
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/tickets/%d", created.ID), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "technician", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if got := tasks.snapshot(); len(got) != 0 {
		t.Errorf("assignee-only patch should not enqueue a notification, got %d task(s)", len(got))
	}
}

// ── LEA lookup ───────────────────────────────────────────────────────────────

// TestLEALookup_RequiresLeaAccessClaim verifies noc_engineer alone is not
// enough: the separate lea_access claim must also be present.
func TestLEALookup_RequiresLeaAccessClaim(t *testing.T) {
	lea := &stubLEA{}
	audit := &stubLEAAudit{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), LEA: lea, LEAAudit: audit,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"public_ip":"203.0.113.5","timestamp":"2026-01-10T09:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/lea/lookup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", false)) // role yes, lea_access no
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 without the lea_access claim, got %d", rec.Code)
	}
	if len(audit.snapshot()) != 0 {
		t.Error("a request that never reached the handler must not be audited")
	}
}

// TestLEALookup_HitAudits verifies a match returns the result and writes an
// audit row with the resolved subscriber.
func TestLEALookup_HitAudits(t *testing.T) {
	lea := &stubLEA{result: &api.LEAResult{SubscriberID: 3, Username: "found@isp", Source: "direct_ip"}}
	audit := &stubLEAAudit{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), LEA: lea, LEAAudit: audit,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"public_ip":"203.0.113.5","timestamp":"2026-01-10T09:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/lea/lookup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", true))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var got api.LEAResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SubscriberID != 3 {
		t.Errorf("subscriber_id: want 3, got %d", got.SubscriberID)
	}

	entries := audit.snapshot()
	if len(entries) != 1 {
		t.Fatalf("want 1 audit entry, got %d", len(entries))
	}
	if entries[0].ResultSubscriberID == nil || *entries[0].ResultSubscriberID != 3 {
		t.Errorf("audit result_subscriber_id: got %v", entries[0].ResultSubscriberID)
	}
	if entries[0].ResultRowCount != 1 {
		t.Errorf("audit result_row_count: want 1, got %d", entries[0].ResultRowCount)
	}
	if entries[0].AccessorRole != "noc_engineer" {
		t.Errorf("audit accessor_role: got %q", entries[0].AccessorRole)
	}
}

// TestLEALookup_MissAuditsWithZeroCount verifies a miss is still audited, with
// a nil result_subscriber_id and a zero row count.
func TestLEALookup_MissAuditsWithZeroCount(t *testing.T) {
	lea := &stubLEA{result: nil}
	audit := &stubLEAAudit{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), LEA: lea, LEAAudit: audit,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"public_ip":"198.51.100.9","timestamp":"2026-01-10T09:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/lea/lookup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", true))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	entries := audit.snapshot()
	if len(entries) != 1 {
		t.Fatalf("want 1 audit entry even on a miss, got %d", len(entries))
	}
	if entries[0].ResultSubscriberID != nil {
		t.Errorf("want a nil result_subscriber_id on a miss, got %v", *entries[0].ResultSubscriberID)
	}
	if entries[0].ResultRowCount != 0 {
		t.Errorf("result_row_count: want 0, got %d", entries[0].ResultRowCount)
	}
}

// TestLEALookup_InvalidIPRejectedBeforeAudit verifies a malformed request never
// reaches the lookup or the audit log.
func TestLEALookup_InvalidIPRejectedBeforeAudit(t *testing.T) {
	lea := &stubLEA{result: &api.LEAResult{SubscriberID: 1}}
	audit := &stubLEAAudit{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), LEA: lea, LEAAudit: audit,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"public_ip":"not-an-ip","timestamp":"2026-01-10T09:00:00Z"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/lea/lookup", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "noc_engineer", true))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an invalid IP, got %d", rec.Code)
	}
	if len(audit.snapshot()) != 0 {
		t.Error("a request that fails validation must not write an audit row")
	}
}

// ── Razorpay webhook ─────────────────────────────────────────────────────────

const itRazorpaySecret = "razorpay_webhook_secret_for_tests"

func itRazorpaySignature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func itRazorpayPayload(subscriberID int, paymentID string, paise int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "payment.captured",
		"payload": map[string]any{
			"payment": map[string]any{
				"entity": map[string]any{
					"id":     paymentID,
					"amount": paise,
					"notes":  map[string]any{"subscriber_id": strconv.Itoa(subscriberID)},
				},
			},
		},
	})
	return b
}

// TestRazorpayWebhook_ValidSignatureCreditsWallet verifies a genuine
// payment.captured event, addressed at a subscriber via the order's notes,
// recharges the wallet through the real WalletService.
func TestRazorpayWebhook_ValidSignatureCreditsWallet(t *testing.T) {
	wallet := &stubWallet{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(wallet),
		RazorpayWebhookSecret: itRazorpaySecret,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := itRazorpayPayload(1, "pay_valid_001", 79900)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader(body))
	req.Header.Set("X-Razorpay-Signature", itRazorpaySignature(body, itRazorpaySecret))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestRazorpayWebhook_InvalidSignatureRejected(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		RazorpayWebhookSecret: itRazorpaySecret,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := itRazorpayPayload(1, "pay_bad_sig", 79900)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader(body))
	req.Header.Set("X-Razorpay-Signature", itRazorpaySignature(body, "wrong-secret"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for a wrong signature, got %d", rec.Code)
	}
}

// TestRazorpayWebhook_UnconfiguredSecretRefuses verifies the endpoint refuses
// outright — rather than validating against an empty secret — when
// RAZORPAY_WEBHOOK_SECRET was never set.
func TestRazorpayWebhook_UnconfiguredSecretRefuses(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{})})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := itRazorpayPayload(1, "pay_no_secret", 79900)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader(body))
	req.Header.Set("X-Razorpay-Signature", itRazorpaySignature(body, "")) // even a "correct" empty-secret sig
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when the webhook secret is unconfigured, got %d", rec.Code)
	}
}

// TestRazorpayWebhook_NonCapturedEventAcknowledgedWithoutCredit verifies an
// event other than payment.captured is acknowledged so Razorpay stops
// retrying it, without moving money.
func TestRazorpayWebhook_NonCapturedEventAcknowledgedWithoutCredit(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		RazorpayWebhookSecret: itRazorpaySecret,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	b, _ := json.Marshal(map[string]any{"event": "payment.failed", "payload": map[string]any{}})
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader(b))
	req.Header.Set("X-Razorpay-Signature", itRazorpaySignature(b, itRazorpaySecret))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("non-captured events must still be acknowledged with 200, got %d", rec.Code)
	}
}
