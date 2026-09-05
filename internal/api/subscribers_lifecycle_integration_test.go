//go:build integration

// Subscriber lifecycle endpoint tests — FR-LC-001..003, FR-BIL-010..011 |
// MDS §4.14.
//
// Each handler is exercised through the real middleware chain against
// in-memory stubs of its store dependencies (same shape as
// new_endpoints_integration_test.go), so what is under test is route wiring,
// authorization, proration/CoA/PoD side effects and response shape — the SQL
// itself is covered in internal/db/subscribers_integration_test.go and
// internal/db/billing_integration_test.go.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// itRoleTokenWithSubject mints a role token carrying a username in the
// Subject claim — itRoleToken (new_endpoints_integration_test.go) leaves
// Subject unset, which is fine for tests that only check authorization, but
// staff attribution (adjustedBy/refundedBy) reads SubjectFromContext, so a
// test asserting on that value needs a token shaped like the real ones
// staffui.auth issues (Subject: username), not a bare role token.
func itRoleTokenWithSubject(t *testing.T, role, subject string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             role,
		RegisteredClaims: jwt.RegisteredClaims{Subject: subject, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign %s token: %v", role, err)
	}
	return tok
}

// ── Stubs ────────────────────────────────────────────────────────────────────

type stubLifecycle struct {
	info    *api.PlanChangeInfo
	infoErr error

	changed   *api.SubscriberRecord
	changeErr error

	terminated   *api.SubscriberRecord
	terminateErr error

	lastNewPlanID  int
	lastNewExpiry  time.Time
	terminateCalls int
}

func (s *stubLifecycle) GetPlanChangeInfo(_ context.Context, _, newPlanID int) (*api.PlanChangeInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}
	return s.info, nil
}

func (s *stubLifecycle) SetSubscriberPlan(_ context.Context, id, newPlanID int, newExpiry time.Time) (*api.SubscriberRecord, error) {
	s.lastNewPlanID, s.lastNewExpiry = newPlanID, newExpiry
	if s.changeErr != nil {
		return nil, s.changeErr
	}
	if s.changed != nil {
		return s.changed, nil
	}
	return &api.SubscriberRecord{ID: id, Username: "changed_sub", PlanID: newPlanID, PlanExpiry: &newExpiry}, nil
}

func (s *stubLifecycle) TerminateSubscriber(_ context.Context, id int) (*api.SubscriberRecord, error) {
	s.terminateCalls++
	if s.terminateErr != nil {
		return nil, s.terminateErr
	}
	if s.terminated != nil {
		return s.terminated, nil
	}
	return &api.SubscriberRecord{ID: id, Username: "terminated_sub", Status: "terminated"}, nil
}

// stubRefunds implements api.RefundQuerier.
type stubRefunds struct {
	lastSubscriberID, lastLedgerEntryID int
	lastAmount                          decimal.Decimal
	lastReason, lastRefundedBy          string
	err                                 error
}

func (s *stubRefunds) CreateRefund(_ context.Context, subscriberID, ledgerEntryID int, amount decimal.Decimal, reason, refundedBy string) (int, error) {
	s.lastSubscriberID, s.lastLedgerEntryID = subscriberID, ledgerEntryID
	s.lastAmount, s.lastReason, s.lastRefundedBy = amount, reason, refundedBy
	if s.err != nil {
		return 0, s.err
	}
	return 42, nil
}

// stubSubCache implements api.SubscriberCacheInvalidator.
type stubSubCache struct {
	invalidated []string
}

func (s *stubSubCache) InvalidateSubscriber(_ context.Context, username string) error {
	s.invalidated = append(s.invalidated, username)
	return nil
}

// stubWalletFunded is stubWallet with a caller-chosen starting balance, so a
// debit (adjustment, refund, auto-renewal path) can be tested both when it
// fits and when it does not — stubWallet's fixed zero balance can only ever
// exercise the "insufficient" side.
type stubWalletFunded struct {
	balance           decimal.Decimal
	rechargeIDCounter int
}

func (s *stubWalletFunded) GetTransactionByToken(context.Context, string) (*billing.Transaction, error) {
	return nil, nil
}
func (s *stubWalletFunded) GetSubscriberBalance(context.Context, int) (decimal.Decimal, error) {
	return s.balance, nil
}
func (s *stubWalletFunded) RecordRecharge(_ context.Context, p billing.RechargePosting) (*billing.Transaction, error) {
	s.rechargeIDCounter++
	return &billing.Transaction{
		ID: s.rechargeIDCounter, SubscriberID: p.Credit.SubscriberID,
		EntryType: p.Credit.EntryType, Amount: p.Credit.Amount, BalanceAfter: p.NewBalance,
	}, nil
}

// ── Plan change (FR-LC-001) ─────────────────────────────────────────────────

// The fixture leaves 11 days rather than 10 deliberately. At exactly 10 the
// bonus works out to precisely 5 days, the total lands precisely on this
// test's own lower bound, and which side it falls is decided by the
// microseconds between the fixture being built and the handler reading the
// clock — so it failed roughly whenever it was run. 11 days gives 5 bonus
// days with room on either side.
//
// The boundary itself is worth knowing about and is pinned deterministically
// in TestComputePlanChangeExpiry_BonusDaysAreFloored, which can inject a
// clock; this test goes through the router and cannot.
func TestChangeSubscriberPlan_ProratesAndPersists(t *testing.T) {
	expiry := time.Now().Add(11 * 24 * time.Hour)
	lc := &stubLifecycle{info: &api.PlanChangeInfo{
		Username: "prorate_sub", CurrentExpiry: &expiry,
		OldPrice: decimal.NewFromInt(300), OldValidityDays: 30,
		NewPrice: decimal.NewFromInt(600), NewValidityDays: 30,
	}}
	cache := &stubSubCache{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: lc, SubCache: cache,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if lc.lastNewPlanID != 5 {
		t.Errorf("new_plan_id passed to store = %d, want 5", lc.lastNewPlanID)
	}
	// 11 days remaining at 10/day old value = 110 credit; new plan is 20/day,
	// so floor(5.5) = 5 bonus days on top of the new plan's own 30 = 35 total.
	wantMin := time.Now().Add(34 * 24 * time.Hour)
	wantMax := time.Now().Add(36 * 24 * time.Hour)
	if lc.lastNewExpiry.Before(wantMin) || lc.lastNewExpiry.After(wantMax) {
		t.Errorf("new_expiry = %v, want roughly 35 days out (between %v and %v)", lc.lastNewExpiry, wantMin, wantMax)
	}
	if len(cache.invalidated) != 1 || cache.invalidated[0] != "prorate_sub" {
		t.Errorf("auth-cache invalidation = %v, want exactly [prorate_sub]", cache.invalidated)
	}
}

func TestChangeSubscriberPlan_UnknownSubscriber_404(t *testing.T) {
	lc := &stubLifecycle{info: nil}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Lifecycle: lc,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/404/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestChangeSubscriberPlan_UnknownPlan_422(t *testing.T) {
	lc := &stubLifecycle{infoErr: api.ErrInvalidPlan}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Lifecycle: lc,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":999999}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an unknown new_plan_id, got %d — %s", rec.Code, rec.Body.String())
	}
}

// TestChangeSubscriberPlan_NoActiveSession_NoCoAEnqueued verifies a
// subscriber with no live session gets no CoA — there is nothing to
// reconfigure, and enqueueing one would just be a task that fails to find a
// session when it runs.
func TestChangeSubscriberPlan_NoActiveSession_NoCoAEnqueued(t *testing.T) {
	lc := &stubLifecycle{info: &api.PlanChangeInfo{Username: "u", OldValidityDays: 30, NewValidityDays: 30}}
	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: lc, Sessions: &stubSessionReader{session: nil}, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(tasks.snapshot()) != 0 {
		t.Errorf("no session active: want 0 enqueued tasks, got %d", len(tasks.snapshot()))
	}
}

// TestChangeSubscriberPlan_ActiveSession_EnqueuesCoA is the FR-AAA-007 closure
// this endpoint exists for: a live session must get a CoA so the new rate
// limit applies without waiting for reauthentication.
func TestChangeSubscriberPlan_ActiveSession_EnqueuesCoA(t *testing.T) {
	lc := &stubLifecycle{info: &api.PlanChangeInfo{Username: "u", OldValidityDays: 30, NewValidityDays: 30}}
	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: lc,
		Sessions:  &stubSessionReader{session: &health.SessionSummary{NasIP: "10.0.0.5"}},
		Tasks:     tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	got := tasks.snapshot()
	if len(got) != 1 || got[0].Type() != "network:coa_send" {
		t.Fatalf("want exactly 1 network:coa_send task, got %+v", got)
	}
}

// ── Termination (FR-LC-002, now gated by FR-WFL-001) ────────────────────────

// TestTerminateSubscriber_FilesApprovalRequestRatherThanTerminating is the
// core of the FR-WFL-001 gate: the endpoint that used to end an account
// outright now only files a request. Nothing must reach the lifecycle store
// or the task queue until a second staff member approves.
func TestTerminateSubscriber_FilesApprovalRequestRatherThanTerminating(t *testing.T) {
	lc := &stubLifecycle{}
	tasks := &stubTaskEnqueuer{}
	cache := &stubSubCache{}
	approvals := newStubApprovals()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: lc, SubCache: cache, Approvals: approvals,
		Sessions: &stubSessionReader{session: &health.SessionSummary{NasIP: "10.0.0.5"}},
		Tasks:    tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"reason":"subscriber relocated"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/terminate", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, "billing_admin", "requester"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 (request filed, not executed), got %d — %s", rec.Code, rec.Body.String())
	}
	if lc.terminateCalls != 0 {
		t.Error("the subscriber must NOT have been terminated before a second approver signed off")
	}
	if len(tasks.snapshot()) != 0 {
		t.Error("no PoD may be enqueued before approval")
	}
	if len(cache.invalidated) != 0 {
		t.Error("no auth-cache invalidation may happen before approval")
	}
	if len(approvals.byID) != 1 {
		t.Fatalf("want exactly 1 approval request filed, got %d", len(approvals.byID))
	}
}

func TestTerminateSubscriber_RequiresReason_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: &stubLifecycle{}, Approvals: newStubApprovals(),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/terminate", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 without a reason, got %d — %s", rec.Code, rec.Body.String())
	}
}

// stubLifecycleAlwaysNotFound reports every lookup as a missing subscriber —
// TerminateSubscriber's (nil, nil) convention needs a dedicated stub since
// stubLifecycle's zero value instead returns a synthesized record.
type stubLifecycleAlwaysNotFound struct{}

func (s *stubLifecycleAlwaysNotFound) GetPlanChangeInfo(context.Context, int, int) (*api.PlanChangeInfo, error) {
	return nil, nil
}
func (s *stubLifecycleAlwaysNotFound) SetSubscriberPlan(context.Context, int, int, time.Time) (*api.SubscriberRecord, error) {
	return nil, nil
}
func (s *stubLifecycleAlwaysNotFound) TerminateSubscriber(context.Context, int) (*api.SubscriberRecord, error) {
	return nil, nil
}

// ── Adjustments (FR-BIL-010) ────────────────────────────────────────────────

// TestCreateAdjustment_CreditFilesApprovalRequest: a credit moves money into
// a subscriber's wallet, so FR-WFL-001 gates it — 202 and a pending request,
// with the wallet untouched until somebody else approves.
func TestCreateAdjustment_CreditFilesApprovalRequest(t *testing.T) {
	wallet := &stubWalletFunded{balance: decimal.NewFromInt(100)}
	approvals := newStubApprovals()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(wallet),
		Approvals: approvals,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"credit","reason":"goodwill credit"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, "billing_admin", "requester"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 (credit is gated), got %d — %s", rec.Code, rec.Body.String())
	}
	if wallet.rechargeIDCounter != 0 {
		t.Error("no money may move before a second approver signs off")
	}
	if len(approvals.byID) != 1 {
		t.Fatalf("want exactly 1 approval request filed, got %d", len(approvals.byID))
	}
}

// TestCreateAdjustment_DebitExecutesImmediately pins the deliberate
// asymmetry (MDS §4.15): a debit reduces what a subscriber can spend and is
// usually itself a correction, so it is not gated.
func TestCreateAdjustment_DebitExecutesImmediately(t *testing.T) {
	wallet := &stubWalletFunded{balance: decimal.NewFromInt(100)}
	approvals := newStubApprovals()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(wallet),
		Approvals: approvals,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"debit","reason":"correcting an earlier over-credit"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, "billing_admin", "requester"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201 (debit is not gated), got %d — %s", rec.Code, rec.Body.String())
	}
	if wallet.rechargeIDCounter != 1 {
		t.Error("a debit must execute immediately, not file a request")
	}
	if len(approvals.byID) != 0 {
		t.Error("a debit must not file an approval request")
	}
}

// TestCreateAdjustment_DebitExceedsBalance_422 is the negative control for
// the overdraft guard: a debit larger than the wallet holds must be refused,
// not silently taken negative.
func TestCreateAdjustment_DebitExceedsBalance_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), // balance always 0
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"debit","reason":"correction"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for a debit exceeding balance, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAdjustment_MissingReason_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWalletFunded{balance: decimal.NewFromInt(100)}),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"credit","reason":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for a missing reason, got %d", rec.Code)
	}
}

func TestCreateAdjustment_InvalidDirection_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWalletFunded{balance: decimal.NewFromInt(100)}),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"sideways","reason":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an invalid direction, got %d", rec.Code)
	}
}

// ── Refunds (FR-BIL-011, now gated by FR-WFL-001) ───────────────────────────

// TestCreateRefund_FilesApprovalRequestRatherThanRefunding: a refund moves
// money out of the wallet, so it is gated. Neither the wallet debit nor the
// payment_refunds row may be written before approval.
func TestCreateRefund_FilesApprovalRequestRatherThanRefunding(t *testing.T) {
	refunds := &stubRefunds{}
	wallet := &stubWalletFunded{balance: decimal.NewFromInt(500)}
	approvals := newStubApprovals()
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(wallet),
		Refunds: refunds, Approvals: approvals,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"200.00","reason":"duplicate recharge"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/refunds", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, "billing_admin", "priya.billing"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 (refund is gated), got %d — %s", rec.Code, rec.Body.String())
	}
	if wallet.rechargeIDCounter != 0 {
		t.Error("no wallet debit may happen before a second approver signs off")
	}
	if refunds.lastSubscriberID != 0 {
		t.Error("no payment_refunds row may be written before approval")
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "pending" {
		t.Errorf("filed request status = %v, want pending", got["status"])
	}
	if got["requested_by"] != "priya.billing" {
		t.Errorf("requested_by = %v, want priya.billing", got["requested_by"])
	}
}

func TestCreateRefund_RequiresReason_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWalletFunded{balance: decimal.NewFromInt(500)}),
		Refunds: &stubRefunds{}, Approvals: newStubApprovals(),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"200.00","reason":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/refunds", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 without a reason, got %d", rec.Code)
	}
}

// ── Authorization and configuration ─────────────────────────────────────────

// TestLifecycleRoutes_ForbiddenForCSR verifies these money/lifecycle-mutating
// routes are restricted to billing_admin/isp_owner, the same gate as
// PATCH /subscribers/{id} and POST /wallets/recharge.
func TestLifecycleRoutes_ForbiddenForCSR(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: &stubLifecycle{info: &api.PlanChangeInfo{OldValidityDays: 30, NewValidityDays: 30}},
		Refunds:   &stubRefunds{}, Approvals: newStubApprovals(),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	reqs := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/subscribers/9/plan-change", `{"new_plan_id":1}`},
		{http.MethodPost, "/api/v1/subscribers/9/terminate", `{"reason":"x"}`},
		{http.MethodPost, "/api/v1/subscribers/9/adjustments", `{"amount":"10","direction":"credit","reason":"x"}`},
		{http.MethodPost, "/api/v1/subscribers/9/refunds", `{"amount":"10","reason":"x"}`},
	}
	for _, tc := range reqs {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "csr", false))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403 for csr, got %d", tc.path, rec.Code)
		}
	}
}

func TestLifecycleRoutes_UnconfiguredStoreReturns503(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		// Lifecycle, Refunds and Approvals left nil.
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	reqs := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/subscribers/9/plan-change", `{"new_plan_id":1}`},
		{http.MethodPost, "/api/v1/subscribers/9/terminate", `{"reason":"x"}`},
		{http.MethodPost, "/api/v1/subscribers/9/refunds", `{"amount":"10","reason":"x"}`},
		{http.MethodPost, "/api/v1/approvals/1/approve", ``},
		{http.MethodPost, "/api/v1/approvals/1/reject", `{"reason":"x"}`},
	}
	for _, tc := range reqs {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: want 503 when unconfigured, got %d — %s", tc.path, rec.Code, rec.Body.String())
		}
	}
}
