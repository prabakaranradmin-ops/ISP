//go:build integration

// Approval decisions — FR-WFL-001, FR-LC-003.
//
// internal/api/workflow.go sat at 65.5%, and the uncovered part was the half
// that matters: the decision path. A wallet credit, a refund and a
// termination all run through here, and three distinct failure modes have to
// stay distinguishable because a client and an operator act differently on
// each:
//
//   - 403 ERR_SELF_APPROVAL — an authorisation boundary, not bad input.
//   - 409 ERR_ALREADY_DECIDED — somebody else got there first.
//   - 200 with status execution_failed — the approval landed, the action did
//     not. Reporting this as 500 would suggest the decision itself had not
//     stuck, and the operator would try again.
//
// The third is the subtle one, and it is why the claim is atomic: two
// approvers deciding at once must produce one execution and one 409, not two
// credits.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/shopspring/decimal"
)

// decisionApprovals is a fuller approvals stub than the bulk one: it can lose
// the claim race, and it records the finalised outcome.
type decisionApprovals struct {
	existing *workflow.ApprovalRequest
	// claimLost makes ClaimApprovalRequest return (nil, nil), which is how
	// the store reports that the conditional update matched no pending row.
	claimLost bool
	getErr    error
	claimErr  error
	rejectErr error
	listErr   error
	list      []workflow.ApprovalRequest

	claimedBy       string
	finalStatus     workflow.Status
	finalErr        *string
	finalLedger     *int
	rejectedBy      string
	rejectReason    string
	gotStatusFilter *workflow.Status
	gotSubFilter    *int
}

func (a *decisionApprovals) CreateApprovalRequest(_ context.Context, req workflow.ApprovalRequest) (*workflow.ApprovalRequest, error) {
	req.ID = 4
	req.Status = workflow.StatusPending
	return &req, nil
}

func (a *decisionApprovals) GetApprovalRequest(context.Context, int) (*workflow.ApprovalRequest, error) {
	return a.existing, a.getErr
}

func (a *decisionApprovals) ListApprovalRequests(_ context.Context, status *workflow.Status, subID *int) ([]workflow.ApprovalRequest, error) {
	a.gotStatusFilter, a.gotSubFilter = status, subID
	return a.list, a.listErr
}

func (a *decisionApprovals) ClaimApprovalRequest(_ context.Context, _ int, decidedBy string) (*workflow.ApprovalRequest, error) {
	if a.claimErr != nil {
		return nil, a.claimErr
	}
	if a.claimLost {
		return nil, nil
	}
	a.claimedBy = decidedBy
	claimed := *a.existing
	claimed.Status = workflow.StatusApproved
	claimed.DecidedBy = &decidedBy
	return &claimed, nil
}

func (a *decisionApprovals) RejectApprovalRequest(_ context.Context, id int, decidedBy, reason string) (*workflow.ApprovalRequest, error) {
	a.rejectedBy, a.rejectReason = decidedBy, reason
	if a.rejectErr != nil {
		return nil, a.rejectErr
	}
	rejected := *a.existing
	rejected.ID = id
	rejected.Status = workflow.StatusRejected
	rejected.DecidedBy = &decidedBy
	rejected.DecisionReason = &reason
	return &rejected, nil
}

func (a *decisionApprovals) FinalizeApprovalExecution(_ context.Context, _ int,
	status workflow.Status, execErr *string, ledgerID *int) error {
	a.finalStatus, a.finalErr, a.finalLedger = status, execErr, ledgerID
	return nil
}

// walletRecorder captures the posting a gated action makes, which is where
// the ledger attribution can be checked.
//
// The attribution lives on the wallet legs rather than on PostRequest: Post
// builds a WalletEntry per leg and carries AdjustedBy and Description there.
// The "Credit" field is the subscriber's own leg whichever way the money
// moved, with EntryType saying the real direction.
type walletRecorder struct {
	err  error
	got  billing.RechargePosting
	seen bool
}

func (w *walletRecorder) GetTransactionByToken(context.Context, string) (*billing.Transaction, error) {
	return nil, nil
}

func (w *walletRecorder) RecordRecharge(_ context.Context, p billing.RechargePosting) (*billing.Transaction, error) {
	w.got, w.seen = p, true
	if w.err != nil {
		return nil, w.err
	}
	return &billing.Transaction{
		ID: 77, SubscriberID: p.Credit.SubscriberID, EntryType: p.Credit.EntryType,
		Amount: p.Credit.Amount, BalanceAfter: p.NewBalance,
	}, nil
}

// A funded balance, because a debit against zero is refused before the
// posting is ever built — which made the refund test assert on a posting
// that had not happened.
func (w *walletRecorder) GetSubscriberBalance(context.Context, int) (decimal.Decimal, error) {
	return decimal.RequireFromString("10000.00"), nil
}

func decide(h *api.Handler, id, method, target, body, actor string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if actor != "" {
		r = r.WithContext(middleware.WithSubject(r.Context(), actor))
	}
	r.SetPathValue("id", id)

	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(target, "/approve"):
		h.ApproveRequest(w, r)
	case strings.HasSuffix(target, "/reject"):
		h.RejectRequest(w, r)
	case strings.Contains(target, "/approvals/"):
		h.GetApproval(w, r)
	default:
		h.ListApprovals(w, r)
	}
	return w
}

func creditRequest(requestedBy string) *workflow.ApprovalRequest {
	amount := decimal.RequireFromString("5000.00")
	return &workflow.ApprovalRequest{
		ID: 4, ActionType: workflow.ActionWalletCredit, SubscriberID: 12,
		Amount: &amount, Reason: "goodwill after an outage",
		Status: workflow.StatusPending, RequestedBy: requestedBy,
	}
}

// Self-approval is 403 with its own code: an authorisation boundary, not bad
// input, and a client has to be able to tell it from "somebody beat you".
func TestFR_WFL_001_SelfApprovalIs403AndDistinctFromAlreadyDecided(t *testing.T) {
	ap := &decisionApprovals{existing: creditRequest("ravi")}
	h := api.NewHandler(api.HandlerDeps{Approvals: ap})

	self := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/approve", "", "ravi")
	if self.Code != http.StatusForbidden {
		t.Errorf("self-approval: want 403, got %d — %s", self.Code, self.Body.String())
	}
	if !strings.Contains(self.Body.String(), "ERR_SELF_APPROVAL") {
		t.Errorf("want ERR_SELF_APPROVAL, got %s", self.Body.String())
	}
	if ap.claimedBy != "" {
		t.Errorf("the request was claimed by its own requester (%q)", ap.claimedBy)
	}

	decided := creditRequest("ravi")
	decided.Status = workflow.StatusExecuted
	already := api.NewHandler(api.HandlerDeps{Approvals: &decisionApprovals{existing: decided}})
	rec := decide(already, "4", http.MethodPost, "/api/v1/approvals/4/approve", "", "meena")
	if rec.Code != http.StatusConflict {
		t.Errorf("already decided: want 409, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_ALREADY_DECIDED") {
		t.Errorf("want ERR_ALREADY_DECIDED, got %s", rec.Body.String())
	}
}

// Rejection carries the same boundary.
func TestFR_WFL_001_SelfRejectionIsAlso403(t *testing.T) {
	ap := &decisionApprovals{existing: creditRequest("ravi")}
	h := api.NewHandler(api.HandlerDeps{Approvals: ap})

	rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/reject",
		`{"reason":"changed my mind"}`, "ravi")
	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403, got %d — %s", rec.Code, rec.Body.String())
	}
	if ap.rejectedBy != "" {
		t.Errorf("the rejection reached the store as %q", ap.rejectedBy)
	}
}

// Losing the claim race is 409, not a second execution. This is the test that
// says two approvers pressing at once produce one credit.
func TestFR_WFL_001_LosingTheClaimRaceIsAConflictNotASecondExecution(t *testing.T) {
	wallet := &walletRecorder{}
	ap := &decisionApprovals{existing: creditRequest("ravi"), claimLost: true}
	h := api.NewHandler(api.HandlerDeps{
		Approvals: ap,
		Wallet:    billing.NewWalletService(wallet),
	})

	rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/approve", "", "meena")
	if rec.Code != http.StatusConflict {
		t.Errorf("want 409, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ERR_ALREADY_DECIDED") {
		t.Errorf("want ERR_ALREADY_DECIDED, got %s", rec.Body.String())
	}
	if wallet.seen {
		t.Errorf("a lost claim still moved money: %+v", wallet.got)
	}
}

// An approval that lands while its action fails answers 200 with the failure
// recorded on the request — not 500, which would read as "your decision did
// not stick" and invite a second attempt.
func TestFR_WFL_001_AFailedActionIsRecordedNotReportedAsAFailedDecision(t *testing.T) {
	ap := &decisionApprovals{existing: creditRequest("ravi")}
	h := api.NewHandler(api.HandlerDeps{
		Approvals: ap,
		Wallet:    billing.NewWalletService(&walletRecorder{err: errors.New("ledger write rejected")}),
	})

	rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/approve", "", "meena")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 — the approval itself succeeded — got %d: %s",
			rec.Code, rec.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if out["status"] != string(workflow.StatusExecutionFailed) {
		t.Errorf("status = %v, want %s", out["status"], workflow.StatusExecutionFailed)
	}
	if ap.finalStatus != workflow.StatusExecutionFailed {
		t.Errorf("the store recorded %q", ap.finalStatus)
	}
	if ap.finalErr == nil || !strings.Contains(*ap.finalErr, "rejected") {
		t.Errorf("the failure reason was not recorded: %v", ap.finalErr)
	}
}

// With no wallet service, an approved credit records the failure rather than
// reporting success — an approval marked executed with no money moved is the
// worst of the available outcomes.
func TestFR_WFL_001_AnApprovedCreditWithNoWalletRecordsTheFailure(t *testing.T) {
	ap := &decisionApprovals{existing: creditRequest("ravi")}
	h := api.NewHandler(api.HandlerDeps{Approvals: ap})

	rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/approve", "", "meena")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if ap.finalStatus != workflow.StatusExecutionFailed {
		t.Errorf("the request was finalised as %q with no wallet service configured",
			ap.finalStatus)
	}
}

// The ledger movement is attributed to the requester, with the approver in
// the description. Both parties are on approval_requests; the ledger records
// whose judgment call it was.
func TestFR_LC_003_TheLedgerAttributesTheMovementToTheRequester(t *testing.T) {
	wallet := &walletRecorder{}
	ap := &decisionApprovals{existing: creditRequest("ravi")}
	h := api.NewHandler(api.HandlerDeps{
		Approvals: ap,
		Wallet:    billing.NewWalletService(wallet),
	})

	rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/approve", "", "meena")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	leg := wallet.got.Credit
	if leg.AdjustedBy != "ravi" {
		t.Errorf("the posting is attributed to %q, want the requester", leg.AdjustedBy)
	}
	if !strings.Contains(leg.Description, "meena") {
		t.Errorf("the approver is not in the description: %q", leg.Description)
	}
	if !strings.Contains(leg.Description, "goodwill after an outage") {
		t.Errorf("the reason is not in the description: %q", leg.Description)
	}
	if leg.EntryType != "credit" {
		t.Errorf("the subscriber's leg is a %q", leg.EntryType)
	}
	if !leg.Amount.Equal(decimal.RequireFromString("5000.00")) {
		t.Errorf("amount = %s", leg.Amount)
	}
	if ap.finalStatus != workflow.StatusExecuted {
		t.Errorf("the request was finalised as %q", ap.finalStatus)
	}
	if ap.finalLedger == nil || *ap.finalLedger != 77 {
		t.Errorf("the ledger id was not recorded on the request: %v", ap.finalLedger)
	}
}

// A refund moves the other way, and its reason reads as a refund rather than
// an adjustment.
func TestFR_LC_003_ARefundIsADebitDescribedAsARefund(t *testing.T) {
	amount := decimal.RequireFromString("300.00")
	req := &workflow.ApprovalRequest{
		ID: 5, ActionType: workflow.ActionRefund, SubscriberID: 12, Amount: &amount,
		Reason: "double charge in November", Status: workflow.StatusPending, RequestedBy: "ravi",
	}
	wallet := &walletRecorder{}
	h := api.NewHandler(api.HandlerDeps{
		Approvals: &decisionApprovals{existing: req},
		Wallet:    billing.NewWalletService(wallet),
	})

	decide(h, "5", http.MethodPost, "/api/v1/approvals/5/approve", "", "meena")
	leg := wallet.got.Credit
	if leg.EntryType != "debit" {
		t.Errorf("a refund posted the subscriber's leg as %q", leg.EntryType)
	}
	if !strings.Contains(leg.Description, "refund") {
		t.Errorf("description = %q; a refund should not read as an adjustment",
			leg.Description)
	}
}

// A rejection needs a reason, and the reason lands on the row — the only
// record of why somebody's refund was refused.
func TestFR_WFL_001_ARejectionNeedsAReasonAndRecordsIt(t *testing.T) {
	ap := &decisionApprovals{existing: creditRequest("ravi")}
	h := api.NewHandler(api.HandlerDeps{Approvals: ap})

	for _, body := range []string{`{}`, `{"reason":""}`} {
		rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/reject", body, "meena")
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: want 422, got %d — %s", body, rec.Code, rec.Body.String())
		}
	}
	if ap.rejectedBy != "" {
		t.Error("a reasonless rejection reached the store")
	}

	if rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/reject", `{`, "meena"); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: want 400, got %d", rec.Code)
	}

	rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/reject",
		`{"reason":"no outage on that date"}`, "meena")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if ap.rejectReason != "no outage on that date" {
		t.Errorf("the stored reason is %q", ap.rejectReason)
	}
	if ap.rejectedBy != "meena" {
		t.Errorf("the row records %q as the decider", ap.rejectedBy)
	}
}

// A request that does not exist, or cannot be read, is not decided.
func TestFR_WFL_001_AMissingOrUnreadableRequestIsNotDecided(t *testing.T) {
	for name, ap := range map[string]*decisionApprovals{
		"missing":    {existing: nil},
		"read error": {getErr: errors.New("store down")},
	} {
		for _, target := range []string{
			"/api/v1/approvals/4/approve", "/api/v1/approvals/4/reject",
		} {
			t.Run(name+target, func(t *testing.T) {
				h := api.NewHandler(api.HandlerDeps{Approvals: ap})
				rec := decide(h, "4", http.MethodPost, target, `{"reason":"x"}`, "meena")
				if rec.Code == http.StatusOK {
					t.Errorf("decided anyway: %s", rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), "store down") {
					t.Errorf("the store's own error reached the caller: %s", rec.Body.String())
				}
				if ap.claimedBy != "" || ap.rejectedBy != "" {
					t.Error("the decision path was reached")
				}
			})
		}
	}
}

// A claim that errors is 500 and distinguishable from a lost race.
func TestFR_WFL_001_AFailedClaimIsNotAConflict(t *testing.T) {
	ap := &decisionApprovals{
		existing: creditRequest("ravi"),
		claimErr: errors.New("store down"),
	}
	h := api.NewHandler(api.HandlerDeps{Approvals: ap})

	rec := decide(h, "4", http.MethodPost, "/api/v1/approvals/4/approve", "", "meena")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d — %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ERR_ALREADY_DECIDED") {
		t.Error("a store failure was reported as somebody else having decided, which would " +
			"stop the approver retrying")
	}
}

// One request reads back, and a missing one is 404 rather than an empty
// approval that reads as a request with no detail.
func TestFR_WFL_001_OneRequestReadsBack(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		Approvals: &decisionApprovals{existing: creditRequest("ravi")},
	})
	rec := decide(h, "4", http.MethodGet, "/api/v1/approvals/4", "", "meena")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "goodwill after an outage") {
		t.Errorf("the request's detail is missing: %s", rec.Body.String())
	}

	missing := api.NewHandler(api.HandlerDeps{Approvals: &decisionApprovals{existing: nil}})
	if rec := decide(missing, "4", http.MethodGet, "/api/v1/approvals/4", "", "meena"); rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}

	bad := api.NewHandler(api.HandlerDeps{Approvals: &decisionApprovals{existing: creditRequest("ravi")}})
	if rec := decide(bad, "not-a-number", http.MethodGet, "/api/v1/approvals/not-a-number", "", "meena"); rec.Code != http.StatusBadRequest {
		t.Errorf("want 400 for a non-numeric id, got %d", rec.Code)
	}
}

// The listing passes its filters through and answers a readable shape.
func TestFR_WFL_001_TheListingFiltersByStatusAndSubscriber(t *testing.T) {
	ap := &decisionApprovals{list: []workflow.ApprovalRequest{*creditRequest("ravi")}}
	h := api.NewHandler(api.HandlerDeps{Approvals: ap})

	r := httptest.NewRequest(http.MethodGet, "/api/v1/approvals?status=pending&subscriber_id=12", nil)
	r = r.WithContext(middleware.WithSubject(r.Context(), "meena"))
	w := httptest.NewRecorder()
	h.ListApprovals(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", w.Code, w.Body.String())
	}
	if ap.gotStatusFilter == nil || *ap.gotStatusFilter != workflow.StatusPending {
		t.Errorf("the status filter reached the store as %v", ap.gotStatusFilter)
	}
	if ap.gotSubFilter == nil || *ap.gotSubFilter != 12 {
		t.Errorf("the subscriber filter reached the store as %v", ap.gotSubFilter)
	}

	// No filters means no predicates, not a default that silently hides rows.
	plain := &decisionApprovals{}
	h2 := api.NewHandler(api.HandlerDeps{Approvals: plain})
	r2 := httptest.NewRequest(http.MethodGet, "/api/v1/approvals", nil)
	w2 := httptest.NewRecorder()
	h2.ListApprovals(w2, r2)
	if plain.gotStatusFilter != nil || plain.gotSubFilter != nil {
		t.Errorf("an unfiltered listing passed filters: status=%v sub=%v",
			plain.gotStatusFilter, plain.gotSubFilter)
	}

	broken := api.NewHandler(api.HandlerDeps{
		Approvals: &decisionApprovals{listErr: errors.New("store down")},
	})
	w3 := httptest.NewRecorder()
	broken.ListApprovals(w3, httptest.NewRequest(http.MethodGet, "/api/v1/approvals", nil))
	if w3.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d", w3.Code)
	}
}

// With no approval store, every path reports it rather than panicking.
func TestFR_WFL_001_AnUnconfiguredApprovalStoreIsReported(t *testing.T) {
	bare := api.NewHandler(api.HandlerDeps{})
	for name, tc := range map[string]struct{ method, target, body string }{
		"approve": {http.MethodPost, "/api/v1/approvals/4/approve", ""},
		"reject":  {http.MethodPost, "/api/v1/approvals/4/reject", `{"reason":"x"}`},
	} {
		t.Run(name, func(t *testing.T) {
			rec := decide(bare, "4", tc.method, tc.target, tc.body, "meena")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}

	// And the exported method the console calls refuses too, rather than
	// nil-dereferencing inside a money path.
	if _, err := bare.ExecuteApprovedRequest(context.Background(), 4, "meena"); err == nil {
		t.Error("ExecuteApprovedRequest succeeded with no approval store")
	}
}

// The decision is audited with both parties and the outcome, which is what
// makes the trail answer "who approved this, who asked, and did it work".
func TestFR_WFL_001_TheDecisionIsAuditedWithBothParties(t *testing.T) {
	sink := &recordingAuditSink{}
	middleware.SetAuditSink(sink)
	t.Cleanup(func() { middleware.SetAuditSink(nil) })

	h := api.NewHandler(api.HandlerDeps{
		Approvals: &decisionApprovals{existing: creditRequest("ravi")},
		Wallet:    billing.NewWalletService(&walletRecorder{}),
	})
	decide(h, "4", http.MethodPost, "/api/v1/approvals/4/approve", "", "meena")

	var found bool
	for _, e := range sink.entries() {
		if e.Action != "approval.approve" {
			continue
		}
		found = true
		if e.Detail["requested_by"] != "ravi" {
			t.Errorf("requested_by = %v", e.Detail["requested_by"])
		}
		if e.Detail["status"] != string(workflow.StatusExecuted) {
			t.Errorf("status = %v", e.Detail["status"])
		}
		if e.Detail["action_type"] != string(workflow.ActionWalletCredit) {
			t.Errorf("action_type = %v", e.Detail["action_type"])
		}
	}
	if !found {
		t.Error("the approval was not audited")
	}
}
