//go:build integration

// Task & approval workflow tests — FR-WFL-001..002 | MDS §4.15.
//
// The requirement these exist to defend is a single sentence — a sensitive
// action must be signed off by a *second* person before it takes effect —
// and almost every test here is a negative control on some way that could
// silently fail to hold: the requester approving their own request, two
// approvers racing so the action runs twice, a reject that executes anyway,
// or a request executing while still pending.
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
)

// ── Stub approval store ──────────────────────────────────────────────────────

// stubApprovals is an in-memory ApprovalQuerier. ClaimApprovalRequest and
// RejectApprovalRequest hold a mutex across the read-check-write so they
// reproduce the real store's atomic conditional UPDATE — a stub that let
// both of two racing claims succeed would make the race test pass against a
// store that could never actually behave that way, which is worse than not
// testing it.
type stubApprovals struct {
	mu     sync.Mutex
	byID   map[int]*workflow.ApprovalRequest
	nextID int

	createErr, claimErr error
	finalized           []workflow.Status
}

func newStubApprovals() *stubApprovals {
	return &stubApprovals{byID: map[int]*workflow.ApprovalRequest{}, nextID: 1}
}

// seed inserts an already-pending request, for tests that start from "a
// request exists" rather than filing one over HTTP first.
func (s *stubApprovals) seed(req workflow.ApprovalRequest) *workflow.ApprovalRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	req.ID = s.nextID
	s.nextID++
	if req.Status == "" {
		req.Status = workflow.StatusPending
	}
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now()
	}
	s.byID[req.ID] = &req
	return &req
}

func (s *stubApprovals) CreateApprovalRequest(_ context.Context, req workflow.ApprovalRequest) (*workflow.ApprovalRequest, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	return s.seed(req), nil
}

func (s *stubApprovals) GetApprovalRequest(_ context.Context, id int) (*workflow.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	cp := *req
	return &cp, nil
}

func (s *stubApprovals) ListApprovalRequests(_ context.Context, status *workflow.Status, subscriberID *int) ([]workflow.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []workflow.ApprovalRequest
	for _, req := range s.byID {
		if status != nil && req.Status != *status {
			continue
		}
		if subscriberID != nil && req.SubscriberID != *subscriberID {
			continue
		}
		out = append(out, *req)
	}
	return out, nil
}

func (s *stubApprovals) ClaimApprovalRequest(_ context.Context, id int, decidedBy string) (*workflow.ApprovalRequest, error) {
	if s.claimErr != nil {
		return nil, s.claimErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.byID[id]
	if !ok || req.Status != workflow.StatusPending {
		return nil, nil // the conditional UPDATE matched no row
	}
	req.Status = workflow.StatusApproved
	req.DecidedBy = &decidedBy
	now := time.Now()
	req.DecidedAt = &now
	cp := *req
	return &cp, nil
}

func (s *stubApprovals) RejectApprovalRequest(_ context.Context, id int, decidedBy, reason string) (*workflow.ApprovalRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.byID[id]
	if !ok || req.Status != workflow.StatusPending {
		return nil, nil
	}
	req.Status = workflow.StatusRejected
	req.DecidedBy = &decidedBy
	req.DecisionReason = &reason
	now := time.Now()
	req.DecidedAt = &now
	cp := *req
	return &cp, nil
}

func (s *stubApprovals) FinalizeApprovalExecution(_ context.Context, id int, status workflow.Status, execErr *string, ledgerEntryID *int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized = append(s.finalized, status)
	if req, ok := s.byID[id]; ok {
		req.Status = status
		req.ExecutionError = execErr
		req.LedgerEntryID = ledgerEntryID
	}
	return nil
}

// ── Stub field-task store ────────────────────────────────────────────────────

type stubFieldTasks struct {
	mu     sync.Mutex
	byID   map[int]*workflow.FieldTask
	nextID int
	err    error
}

func newStubFieldTasks() *stubFieldTasks {
	return &stubFieldTasks{byID: map[int]*workflow.FieldTask{}, nextID: 1}
}

func (s *stubFieldTasks) CreateFieldTask(_ context.Context, t workflow.FieldTask) (*workflow.FieldTask, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t.ID = s.nextID
	s.nextID++
	t.Status = workflow.TaskOpen
	t.CreatedAt, t.UpdatedAt = time.Now(), time.Now()
	s.byID[t.ID] = &t
	return &t, nil
}

func (s *stubFieldTasks) GetFieldTask(_ context.Context, id int) (*workflow.FieldTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	return t, nil
}

func (s *stubFieldTasks) ListFieldTasks(_ context.Context, assignedTo, status *string) ([]workflow.FieldTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []workflow.FieldTask
	for _, t := range s.byID {
		if assignedTo != nil && t.AssignedTo != *assignedTo {
			continue
		}
		if status != nil && t.Status != *status {
			continue
		}
		out = append(out, *t)
	}
	return out, nil
}

func (s *stubFieldTasks) UpdateFieldTask(_ context.Context, id int, status, assignedTo *string, dueDate *time.Time) (*workflow.FieldTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	if status != nil {
		t.Status = *status
		if *status == workflow.TaskCompleted {
			now := time.Now()
			t.CompletedAt = &now
		} else {
			t.CompletedAt = nil
		}
	}
	if assignedTo != nil {
		t.AssignedTo = *assignedTo
	}
	if dueDate != nil {
		t.DueDate = dueDate
	}
	return t, nil
}

// ── Harness ──────────────────────────────────────────────────────────────────

type workflowHarness struct {
	mux       *http.ServeMux
	approvals *stubApprovals
	wallet    *stubWalletFunded
	refunds   *stubRefunds
	lifecycle *stubLifecycle
	tasks     *stubTaskEnqueuer
	cache     *stubSubCache
}

func newWorkflowHarness(t *testing.T) *workflowHarness {
	t.Helper()
	h := &workflowHarness{
		approvals: newStubApprovals(),
		wallet:    &stubWalletFunded{balance: decimal.NewFromInt(1000)},
		refunds:   &stubRefunds{},
		lifecycle: &stubLifecycle{},
		tasks:     &stubTaskEnqueuer{},
		cache:     &stubSubCache{},
	}
	handler := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{},
		Wallet:    billing.NewWalletService(h.wallet),
		Approvals: h.approvals, Refunds: h.refunds, Lifecycle: h.lifecycle,
		SubCache: h.cache, Tasks: h.tasks,
		Sessions:   &stubSessionReader{session: &health.SessionSummary{NasIP: "10.0.0.5"}},
		FieldTasks: newStubFieldTasks(),
	})
	h.mux = http.NewServeMux()
	handler.RegisterRoutes(h.mux, itJWTSecret)
	return h
}

func (h *workflowHarness) do(t *testing.T, method, path, body, role, subject string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, subject))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// ── The core requirement, and the ways it could silently fail ───────────────

// TestFR_WFL_001_SelfApprovalIsRefused is the single most important test in
// this module: if the requester can approve their own request, the entire
// second-approver control is decorative. A pass here must mean the money did
// not move, not merely that a 403 was returned.
func TestFR_WFL_001_SelfApprovalIsRefused(t *testing.T) {
	h := newWorkflowHarness(t)

	filed := h.do(t, http.MethodPost, "/api/v1/subscribers/9/adjustments",
		`{"amount":"500.00","direction":"credit","reason":"goodwill"}`, "billing_admin", "alice")
	if filed.Code != http.StatusAccepted {
		t.Fatalf("filing: want 202, got %d — %s", filed.Code, filed.Body.String())
	}

	// alice, who filed it, now tries to approve it.
	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "alice")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-approval: want 403, got %d — %s", rec.Code, rec.Body.String())
	}
	if h.wallet.rechargeIDCounter != 0 {
		t.Error("SELF-APPROVAL EXECUTED THE ACTION — the second-approver gate does not hold")
	}
	req, _ := h.approvals.GetApprovalRequest(context.Background(), 1)
	if req.Status != workflow.StatusPending {
		t.Errorf("status after refused self-approval = %q, want it left pending", req.Status)
	}
}

// TestFR_WFL_001_SelfRejectionIsRefused: the same rule applies to rejecting.
// A requester quietly withdrawing their own request by "rejecting" it would
// be a different feature (withdrawal) wearing the approver's identity, and
// would leave decided_by_username claiming a review that never happened.
func TestFR_WFL_001_SelfRejectionIsRefused(t *testing.T) {
	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 9,
		Reason: "relocated", RequestedBy: "alice",
	})

	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/reject", `{"reason":"changed my mind"}`, "billing_admin", "alice")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-rejection: want 403, got %d — %s", rec.Code, rec.Body.String())
	}
	req, _ := h.approvals.GetApprovalRequest(context.Background(), 1)
	if req.Status != workflow.StatusPending {
		t.Errorf("status after refused self-rejection = %q, want it left pending", req.Status)
	}
}

// TestFR_WFL_001_ApprovalByASecondStaffMemberExecutes is the positive
// control the negatives above only mean something against: the gate must
// actually let a legitimate second approver through and run the action.
func TestFR_WFL_001_ApprovalByASecondStaffMemberExecutes(t *testing.T) {
	h := newWorkflowHarness(t)

	h.do(t, http.MethodPost, "/api/v1/subscribers/9/adjustments",
		`{"amount":"500.00","direction":"credit","reason":"goodwill"}`, "billing_admin", "alice")

	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "bob")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if h.wallet.rechargeIDCounter != 1 {
		t.Fatalf("want exactly 1 wallet posting after approval, got %d", h.wallet.rechargeIDCounter)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != string(workflow.StatusExecuted) {
		t.Errorf("status = %v, want executed", got["status"])
	}
	if got["decided_by"] != "bob" {
		t.Errorf("decided_by = %v, want bob", got["decided_by"])
	}
	if got["requested_by"] != "alice" {
		t.Errorf("requested_by = %v, want alice", got["requested_by"])
	}
}

// TestFR_WFL_001_ConcurrentApprovalsExecuteExactlyOnce is the race negative
// control. Two approvers hitting approve simultaneously must produce exactly
// one execution — if the claim were a plain read-then-write, both would pass
// the pending check and the subscriber would be credited twice.
func TestFR_WFL_001_ConcurrentApprovalsExecuteExactlyOnce(t *testing.T) {
	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionWalletCredit, SubscriberID: 9,
		Amount: decimalPtr(decimal.NewFromInt(500)), Reason: "goodwill", RequestedBy: "alice",
	})

	const approvers = 8
	var wg sync.WaitGroup
	codes := make([]int, approvers)
	start := make(chan struct{})
	for i := 0; i < approvers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together to maximise the overlap
			codes[i] = h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "approver").Code
		}(i)
	}
	close(start)
	wg.Wait()

	okCount, conflictCount := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			okCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Errorf("unexpected status %d from a concurrent approval", c)
		}
	}
	if okCount != 1 {
		t.Errorf("want exactly 1 successful approval out of %d, got %d", approvers, okCount)
	}
	if conflictCount != approvers-1 {
		t.Errorf("want %d conflicts, got %d", approvers-1, conflictCount)
	}
	if h.wallet.rechargeIDCounter != 1 {
		t.Errorf("DOUBLE EXECUTION: want exactly 1 wallet posting, got %d", h.wallet.rechargeIDCounter)
	}
}

// TestFR_WFL_001_RejectNeverExecutes: a rejected request must leave the
// subscriber untouched. This is the whole point of rejecting.
func TestFR_WFL_001_RejectNeverExecutes(t *testing.T) {
	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 9,
		Reason: "relocated", RequestedBy: "alice",
	})

	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/reject", `{"reason":"customer disputed"}`, "billing_admin", "bob")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if h.lifecycle.terminateCalls != 0 {
		t.Error("a rejected termination must never reach the lifecycle store")
	}
	if len(h.tasks.snapshot()) != 0 {
		t.Error("a rejected termination must not enqueue a PoD")
	}
	req, _ := h.approvals.GetApprovalRequest(context.Background(), 1)
	if req.Status != workflow.StatusRejected {
		t.Errorf("status = %q, want rejected", req.Status)
	}
}

// TestFR_WFL_001_AlreadyDecidedRequestCannotBeReDecided guards the other
// replay: approving something already rejected (or vice versa) must not
// execute it after the fact.
func TestFR_WFL_001_AlreadyDecidedRequestCannotBeReDecided(t *testing.T) {
	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionWalletCredit, SubscriberID: 9,
		Amount: decimalPtr(decimal.NewFromInt(500)), Reason: "goodwill", RequestedBy: "alice",
	})

	if rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/reject", `{"reason":"no"}`, "billing_admin", "bob"); rec.Code != http.StatusOK {
		t.Fatalf("reject: want 200, got %d", rec.Code)
	}

	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "carol")
	if rec.Code != http.StatusConflict {
		t.Fatalf("approving an already-rejected request: want 409, got %d — %s", rec.Code, rec.Body.String())
	}
	if h.wallet.rechargeIDCounter != 0 {
		t.Error("a rejected request must never execute, even on a later approve")
	}
}

// TestFR_WFL_001_TerminationExecutesFullSideEffectsOnApproval verifies the
// approved path performs everything the old ungated endpoint did — status
// change, auth-cache invalidation and PoD — rather than only recording a
// decision.
func TestFR_WFL_001_TerminationExecutesFullSideEffectsOnApproval(t *testing.T) {
	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 9,
		Reason: "relocated", RequestedBy: "alice",
	})

	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	if h.lifecycle.terminateCalls != 1 {
		t.Errorf("want exactly 1 terminate call, got %d", h.lifecycle.terminateCalls)
	}
	tasks := h.tasks.snapshot()
	if len(tasks) != 1 || tasks[0].Type() != "network:pod_send" {
		t.Errorf("want exactly 1 network:pod_send task, got %+v", tasks)
	}
	if len(h.cache.invalidated) != 1 || h.cache.invalidated[0] != "terminated_sub" {
		t.Errorf("auth-cache invalidation = %v, want exactly [terminated_sub]", h.cache.invalidated)
	}
}

// TestFR_WFL_001_ExecutionFailureIsRecordedNotHidden: an approval whose
// action fails (here, a refund larger than the balance now available) must
// come back as execution_failed with the reason — the approval itself
// landed, so reporting a bare 500 would wrongly suggest the decision did
// not stick.
func TestFR_WFL_001_ExecutionFailureIsRecordedNotHidden(t *testing.T) {
	h := newWorkflowHarness(t)
	h.wallet.balance = decimal.NewFromInt(10) // less than the refund below
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionRefund, SubscriberID: 9,
		Amount: decimalPtr(decimal.NewFromInt(500)), Reason: "duplicate recharge", RequestedBy: "alice",
	})

	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (the approval landed even though the action failed), got %d", rec.Code)
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != string(workflow.StatusExecutionFailed) {
		t.Errorf("status = %v, want execution_failed", got["status"])
	}
	if got["execution_error"] == nil || got["execution_error"] == "" {
		t.Error("an execution failure must carry the reason it failed")
	}
}

// TestFR_WFL_001_UnknownRequestIs404 — a decision on a request that does not
// exist must not be reported as success.
func TestFR_WFL_001_UnknownRequestIs404(t *testing.T) {
	h := newWorkflowHarness(t)
	if rec := h.do(t, http.MethodPost, "/api/v1/approvals/9999/approve", ``, "billing_admin", "bob"); rec.Code != http.StatusNotFound {
		t.Errorf("approve unknown: want 404, got %d", rec.Code)
	}
	if rec := h.do(t, http.MethodPost, "/api/v1/approvals/9999/reject", `{"reason":"x"}`, "billing_admin", "bob"); rec.Code != http.StatusNotFound {
		t.Errorf("reject unknown: want 404, got %d", rec.Code)
	}
}

func TestFR_WFL_001_RejectRequiresAReason(t *testing.T) {
	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 9, Reason: "x", RequestedBy: "alice",
	})

	rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/reject", `{}`, "billing_admin", "bob")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 rejecting without a reason, got %d", rec.Code)
	}
}

// TestFR_WFL_001_ApprovalRoutesRefusedToLesserRoles: the approval queue
// exposes what money is about to move and lets a caller release it, so it
// stays on the same billing_admin/isp_owner tier as the actions themselves.
func TestFR_WFL_001_ApprovalRoutesRefusedToLesserRoles(t *testing.T) {
	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 9, Reason: "x", RequestedBy: "alice",
	})

	for _, role := range []string{"csr", "technician", "noc_engineer"} {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/v1/approvals", ``},
			{http.MethodPost, "/api/v1/approvals/1/approve", ``},
			{http.MethodPost, "/api/v1/approvals/1/reject", `{"reason":"x"}`},
		} {
			rec := h.do(t, tc.method, tc.path, tc.body, role, "someone")
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s on %s: want 403, got %d", role, tc.path, rec.Code)
			}
		}
	}
}

// TestFR_WFL_001_ListFiltersByStatus covers the approval queue's primary
// use: "what is waiting on me."
func TestFR_WFL_001_ListFiltersByStatus(t *testing.T) {
	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 9, Reason: "a", RequestedBy: "alice",
	})
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 10, Reason: "b",
		RequestedBy: "alice", Status: workflow.StatusRejected,
	})

	rec := h.do(t, http.MethodGet, "/api/v1/approvals?status=pending", ``, "billing_admin", "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0]["status"] != "pending" {
		t.Errorf("status=pending filter returned %+v, want exactly the one pending request", got)
	}
}

// ── FR-WFL-002: Field tasks ─────────────────────────────────────────────────

func TestFR_WFL_002_FieldTaskCreateAndComplete(t *testing.T) {
	h := newWorkflowHarness(t)

	created := h.do(t, http.MethodPost, "/api/v1/field-tasks",
		`{"title":"Replace CPE at flat 3B","assigned_to":"tech1","due_date":"2026-09-01"}`, "technician", "csr1")
	if created.Code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d — %s", created.Code, created.Body.String())
	}
	var task map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// snake_case, like every other endpoint's response — asserted rather
	// than accepted either way, since the tags are what make that true.
	if task["status"] != workflow.TaskOpen {
		t.Errorf("a new task must start open (and serialize as snake_case), got %+v", task)
	}
	if task["assigned_to"] != "tech1" {
		t.Errorf("assigned_to = %v, want tech1", task["assigned_to"])
	}

	done := h.do(t, http.MethodPatch, "/api/v1/field-tasks/1", `{"status":"completed"}`, "technician", "tech1")
	if done.Code != http.StatusOK {
		t.Fatalf("complete: want 200, got %d — %s", done.Code, done.Body.String())
	}
}

func TestFR_WFL_002_FieldTaskValidation(t *testing.T) {
	h := newWorkflowHarness(t)

	cases := []struct {
		name, body string
	}{
		{"missing title", `{"assigned_to":"tech1"}`},
		{"missing assignee", `{"title":"Something"}`},
		{"malformed due date", `{"title":"X","assigned_to":"tech1","due_date":"01-09-2026"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, http.MethodPost, "/api/v1/field-tasks", tc.body, "technician", "csr1")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestFR_WFL_002_FieldTaskInvalidStatusRejected: an unknown status must be
// refused with a readable message rather than reaching the CHECK constraint
// and surfacing as a raw 500.
func TestFR_WFL_002_FieldTaskInvalidStatusRejected(t *testing.T) {
	h := newWorkflowHarness(t)
	h.do(t, http.MethodPost, "/api/v1/field-tasks", `{"title":"X","assigned_to":"tech1"}`, "technician", "csr1")

	rec := h.do(t, http.MethodPatch, "/api/v1/field-tasks/1", `{"status":"finished"}`, "technician", "tech1")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an unknown status, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestFR_WFL_002_FieldTaskUnknownIs404(t *testing.T) {
	h := newWorkflowHarness(t)
	rec := h.do(t, http.MethodPatch, "/api/v1/field-tasks/9999", `{"status":"completed"}`, "technician", "tech1")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

func decimalPtr(d decimal.Decimal) *decimal.Decimal { return &d }

// ── FR-LC-003: lifecycle actions are auditable against the subscriber ────────
//
// FR-LC-003 (BO-007, CRD-REG-001) requires every lifecycle-affecting action
// to be audit-logged with staff attribution. Plan change and adjustment
// execute inline and have always emitted subscriber.* entries; termination,
// refund and the *gated* half of an adjustment execute inside the approval
// flow instead, which audited only approval.request/approval.approve
// against the approval's own id. The question an auditor actually asks -
// "what was done to subscriber 42, and by whom" - was therefore answerable
// for two of the four actions and silently not for the others.
//
// These assert on the emitted audit line rather than on a call count,
// because the line *is* the deliverable: a regulator reads the log, not the
// code path that produced it.

// captureAuditLog redirects the package-level logger for the duration of a
// test, mirroring internal/middleware's own withCapturedLog.
func captureAuditLog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	orig := log.Logger
	log.Logger = zerolog.New(buf)
	t.Cleanup(func() { log.Logger = orig })
}

// auditEntries returns the audit lines in buf, decoded.
func auditEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue // not every logged line is JSON we care about
		}
		if v, ok := m["audit"].(bool); ok && v {
			out = append(out, m)
		}
	}
	return out
}

// findAudit returns the first audit entry with the given action, or nil.
func findAudit(entries []map[string]any, action string) map[string]any {
	for _, e := range entries {
		if e["action"] == action {
			return e
		}
	}
	return nil
}

func TestFR_LC_003_ApprovedTerminationIsAuditedAgainstTheSubscriber(t *testing.T) {
	var buf bytes.Buffer
	captureAuditLog(t, &buf)

	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionTerminate, SubscriberID: 9,
		Reason: "relocated", RequestedBy: "alice",
	})

	if rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "bob"); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	entry := findAudit(auditEntries(t, &buf), "subscriber.terminate")
	if entry == nil {
		t.Fatal("no subscriber.terminate audit entry: a termination is not traceable against the subscriber it terminated")
	}
	if entry["target"] != "9" {
		t.Errorf("target = %v, want the subscriber id \"9\"", entry["target"])
	}
	// Both parties: the approver from the request context, the requester
	// from the approval record. Either alone leaves a two-person control
	// looking like a one-person action.
	if entry["actor_id"] != "bob" {
		t.Errorf("actor_id = %v, want the approver \"bob\"", entry["actor_id"])
	}
	detail, _ := entry["detail"].(map[string]any)
	if detail == nil || detail["requested_by"] != "alice" {
		t.Errorf("detail.requested_by = %v, want the requester \"alice\"", detail["requested_by"])
	}
	if detail["reason"] != "relocated" {
		t.Errorf("detail.reason = %v, want \"relocated\"", detail["reason"])
	}
}

func TestFR_LC_003_ApprovedRefundIsAuditedAgainstTheSubscriber(t *testing.T) {
	var buf bytes.Buffer
	captureAuditLog(t, &buf)

	h := newWorkflowHarness(t)
	h.wallet.balance = decimal.NewFromInt(500)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionRefund, SubscriberID: 9,
		Amount: decimalPtr(decimal.NewFromInt(200)),
		Reason: "service outage", RequestedBy: "alice",
	})

	if rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "bob"); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	entry := findAudit(auditEntries(t, &buf), "subscriber.refund")
	if entry == nil {
		t.Fatal("no subscriber.refund audit entry: money left the system with no subscriber-level trace")
	}
	if entry["target"] != "9" {
		t.Errorf("target = %v, want the subscriber id \"9\"", entry["target"])
	}
	detail, _ := entry["detail"].(map[string]any)
	if detail == nil {
		t.Fatal("refund audit entry carries no detail")
	}
	// The amount is the whole point of auditing a refund.
	if detail["amount"] != "200" {
		t.Errorf("detail.amount = %v, want \"200\"", detail["amount"])
	}
	if detail["direction"] != "debit" {
		t.Errorf("detail.direction = %v, want \"debit\"", detail["direction"])
	}
}

// The gated half of an adjustment: CreateAdjustment audits the immediate
// debit itself, but a credit is routed through approval and so took the
// same unaudited path terminations did.
func TestFR_LC_003_ApprovedWalletCreditIsAuditedAgainstTheSubscriber(t *testing.T) {
	var buf bytes.Buffer
	captureAuditLog(t, &buf)

	h := newWorkflowHarness(t)
	h.approvals.seed(workflow.ApprovalRequest{
		ActionType: workflow.ActionWalletCredit, SubscriberID: 9,
		Amount: decimalPtr(decimal.NewFromInt(150)),
		Reason: "goodwill", RequestedBy: "alice",
	})

	if rec := h.do(t, http.MethodPost, "/api/v1/approvals/1/approve", ``, "billing_admin", "bob"); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	entry := findAudit(auditEntries(t, &buf), "subscriber.wallet_credit")
	if entry == nil {
		t.Fatal("no subscriber.wallet_credit audit entry for an approved credit")
	}
	if entry["target"] != "9" {
		t.Errorf("target = %v, want the subscriber id \"9\"", entry["target"])
	}
	detail, _ := entry["detail"].(map[string]any)
	if detail == nil || detail["direction"] != "credit" {
		t.Errorf("detail.direction = %v, want \"credit\"", detail["direction"])
	}
}
