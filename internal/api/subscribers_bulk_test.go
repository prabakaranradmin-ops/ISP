//go:build integration

// Bulk subscriber operations — FR-WFL-001 for the credit path, FR-LC-002 for
// what bulk must not do to a subscriber's status.
//
// Acting on up to 500 subscribers at once has no requirement of its own in the
// SRS. The two rules it has to respect do.
//
// internal/api/subscribers_bulk.go was at 0%: 80 statements that act on up to
// 500 subscribers in one request. Two of its design decisions are controls
// rather than conveniences, and both were untested:
//
//   - "terminated" is not a bulk status. Termination is approval-gated as a
//     single action, and bulk must not be a route around that second
//     approver. The exclusion lives in one exported map, which the console's
//     form also reads, so a single edit would open both at once.
//   - A bulk wallet credit does not credit anybody. It files one pending
//     approval per subscriber and answers 202 with a note saying so, because
//     a wallet credit needs two people at any amount.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/shopspring/decimal"
)

// bulkSubs records the status writes so a test can check what moved and,
// more importantly, what did not.
type bulkSubs struct {
	err      error
	notFound bool

	gotIDs    []int
	gotStatus []string
	gotPlanID []*int
	gotExpiry []*time.Time
}

func (b *bulkSubs) CreateSubscriber(_ context.Context, sub api.SubscriberRecord, _ string) (*api.SubscriberRecord, error) {
	return &sub, b.err
}

func (b *bulkSubs) GetSubscriberByID(_ context.Context, id int) (*api.SubscriberRecord, error) {
	if b.notFound {
		return nil, nil
	}
	return &api.SubscriberRecord{ID: id, Username: fmt.Sprintf("sub%d@isp", id)}, b.err
}

func (b *bulkSubs) GetSubscriberByUsername(context.Context, string) (*api.SubscriberRecord, error) {
	return nil, b.err
}

func (b *bulkSubs) UpdateSubscriber(_ context.Context, id int, planID *int, status *string,
	expiry *time.Time) (*api.SubscriberRecord, error) {
	b.gotIDs = append(b.gotIDs, id)
	if status != nil {
		b.gotStatus = append(b.gotStatus, *status)
	}
	b.gotPlanID = append(b.gotPlanID, planID)
	b.gotExpiry = append(b.gotExpiry, expiry)
	if b.err != nil {
		return nil, b.err
	}
	if b.notFound {
		return nil, nil
	}
	return &api.SubscriberRecord{ID: id}, nil
}

// bulkApprovals records the approval requests a bulk credit files.
type bulkApprovals struct {
	err     error
	created []workflow.ApprovalRequest
	nextID  int
}

func (a *bulkApprovals) CreateApprovalRequest(_ context.Context, req workflow.ApprovalRequest) (*workflow.ApprovalRequest, error) {
	if a.err != nil {
		return nil, a.err
	}
	a.nextID++
	req.ID = a.nextID
	req.Status = workflow.StatusPending
	a.created = append(a.created, req)
	return &req, nil
}

func (a *bulkApprovals) GetApprovalRequest(context.Context, int) (*workflow.ApprovalRequest, error) {
	return nil, nil
}
func (a *bulkApprovals) ListApprovalRequests(context.Context, *workflow.Status, *int) ([]workflow.ApprovalRequest, error) {
	return nil, nil
}
func (a *bulkApprovals) ClaimApprovalRequest(context.Context, int, string) (*workflow.ApprovalRequest, error) {
	return nil, nil
}
func (a *bulkApprovals) RejectApprovalRequest(context.Context, int, string, string) (*workflow.ApprovalRequest, error) {
	return nil, nil
}
func (a *bulkApprovals) FinalizeApprovalExecution(context.Context, int, workflow.Status, *string, *int) error {
	return nil
}

func bulkCall(h *api.Handler, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	r = r.WithContext(middleware.WithSubject(r.Context(), "meena"))
	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(target, "/plan-change"):
		h.BulkChangeSubscriberPlan(w, r)
	case strings.HasSuffix(target, "/status"):
		h.BulkUpdateStatus(w, r)
	default:
		h.BulkWalletCredit(w, r)
	}
	return w
}

// "terminated" is not a bulk status, and neither is anything else outside the
// three. Termination is approval-gated on its own, and bulk must not be the
// way round that.
func TestFR_LC_002_BulkCannotTerminate(t *testing.T) {
	for _, status := range []string{
		"terminated", "grace_period", "pending_payment", "TERMINATED", "",
	} {
		subs := &bulkSubs{}
		h := api.NewHandler(api.HandlerDeps{DB: subs})
		rec := bulkCall(h, "/api/v1/subscribers/bulk/status",
			fmt.Sprintf(`{"subscriber_ids":[1,2,3],"status":%q}`, status))

		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("status %q: want 422, got %d — %s", status, rec.Code, rec.Body.String())
		}
		if len(subs.gotIDs) != 0 {
			t.Errorf("status %q reached the store for %v", status, subs.gotIDs)
		}
	}

	// The allowed three are the allowed three. Read from the exported map so
	// this cannot drift from what the console offers.
	if _, ok := api.BulkAllowedStatuses["terminated"]; ok {
		t.Error("terminated is in BulkAllowedStatuses; bulk termination would bypass the " +
			"second approver that TerminateSubscriber requires")
	}
	for status := range api.BulkAllowedStatuses {
		subs := &bulkSubs{}
		h := api.NewHandler(api.HandlerDeps{DB: subs})
		if rec := bulkCall(h, "/api/v1/subscribers/bulk/status",
			fmt.Sprintf(`{"subscriber_ids":[1],"status":%q}`, status)); rec.Code != http.StatusOK {
			t.Errorf("allowed status %q was refused: %d — %s", status, rec.Code, rec.Body.String())
		}
		if len(subs.gotStatus) != 1 || subs.gotStatus[0] != status {
			t.Errorf("allowed status %q reached the store as %v", status, subs.gotStatus)
		}
	}
}

// A bulk suspend or resume moves nobody's billed period. Passing a plan
// expiry here would quietly extend or shorten everyone in the batch.
func TestFR_LC_001_ABulkStatusChangeNeverMovesTheBilledPeriod(t *testing.T) {
	subs := &bulkSubs{}
	h := api.NewHandler(api.HandlerDeps{DB: subs})

	bulkCall(h, "/api/v1/subscribers/bulk/status",
		`{"subscriber_ids":[1,2,3],"status":"hard_suspended"}`)

	if len(subs.gotIDs) != 3 {
		t.Fatalf("the store saw %d subscribers, want 3", len(subs.gotIDs))
	}
	for i := range subs.gotIDs {
		if subs.gotExpiry[i] != nil {
			t.Errorf("subscriber %d had its plan expiry set to %v by a status change",
				subs.gotIDs[i], subs.gotExpiry[i])
		}
		if subs.gotPlanID[i] != nil {
			t.Errorf("subscriber %d had its plan changed by a status change", subs.gotIDs[i])
		}
	}
}

// A bulk credit files approvals and says so. 202, a note, and an approval id
// per subscriber — not a credit.
func TestFR_WFL_001_ABulkCreditFilesApprovalsAndSaysSo(t *testing.T) {
	ap := &bulkApprovals{}
	h := api.NewHandler(api.HandlerDeps{Approvals: ap})

	rec := bulkCall(h, "/api/v1/subscribers/bulk/credit",
		`{"subscriber_ids":[1,2],"amount":"250.00","reason":"goodwill after the outage"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 Accepted — not 200, which reads as done — got %d: %s",
			rec.Code, rec.Body.String())
	}

	var out struct {
		Note    string `json:"note"`
		Results []struct {
			SubscriberID int  `json:"subscriber_id"`
			OK           bool `json:"ok"`
			ApprovalID   int  `json:"approval_id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if !strings.Contains(out.Note, "not an applied credit") {
		t.Errorf("the response must say these are not credits, got note %q", out.Note)
	}
	if len(out.Results) != 2 {
		t.Fatalf("got %d results, want 2", len(out.Results))
	}
	for _, r := range out.Results {
		if !r.OK || r.ApprovalID == 0 {
			t.Errorf("subscriber %d: ok=%v approval_id=%d — a success here means an "+
				"approval exists, so it must carry its id", r.SubscriberID, r.OK, r.ApprovalID)
		}
	}

	if len(ap.created) != 2 {
		t.Fatalf("%d approval requests were filed, want 2", len(ap.created))
	}
	for _, req := range ap.created {
		if req.ActionType != workflow.ActionWalletCredit {
			t.Errorf("filed a %q request", req.ActionType)
		}
		if req.Status != workflow.StatusPending {
			t.Errorf("the request was filed as %q, not pending", req.Status)
		}
		if req.RequestedBy != "meena" {
			t.Errorf("requested_by = %q; it must come from the caller's own auth context",
				req.RequestedBy)
		}
		if req.Amount == nil || !req.Amount.Equal(decimal.RequireFromString("250.00")) {
			t.Errorf("amount = %v", req.Amount)
		}
		// The batch marker lets a second approver recognise these as one
		// action rather than deciding twenty unrelated-looking requests.
		if !strings.Contains(req.Reason, "(bulk)") {
			t.Errorf("reason = %q; it should mark the request as part of a batch", req.Reason)
		}
		if !strings.Contains(req.Reason, "goodwill after the outage") {
			t.Errorf("reason = %q; the operator's own reason is missing", req.Reason)
		}
	}
}

// The batch is capped. Without it one request could hold a transaction over
// every subscriber on the deployment.
func TestTheBulkBatchIsCapped(t *testing.T) {
	tooMany := make([]string, 501)
	for i := range tooMany {
		tooMany[i] = fmt.Sprint(i + 1)
	}
	body := `{"subscriber_ids":[` + strings.Join(tooMany, ",") + `],"status":"active"}`

	subs := &bulkSubs{}
	h := api.NewHandler(api.HandlerDeps{DB: subs, Approvals: &bulkApprovals{}})
	rec := bulkCall(h, "/api/v1/subscribers/bulk/status", body)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an oversized batch, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "500") {
		t.Errorf("the refusal should name the limit, got %s", rec.Body.String())
	}
	if len(subs.gotIDs) != 0 {
		t.Errorf("an oversized batch reached the store for %d subscribers", len(subs.gotIDs))
	}

	// Exactly at the limit is allowed: an off-by-one here would be a cap of
	// 499 that nobody could explain.
	atLimit := make([]string, 500)
	for i := range atLimit {
		atLimit[i] = fmt.Sprint(i + 1)
	}
	ok := bulkCall(api.NewHandler(api.HandlerDeps{DB: &bulkSubs{}}),
		"/api/v1/subscribers/bulk/status",
		`{"subscriber_ids":[`+strings.Join(atLimit, ",")+`],"status":"active"}`)
	if ok.Code != http.StatusOK {
		t.Errorf("a batch of exactly 500 was refused: %d — %s", ok.Code, ok.Body.String())
	}
}

// An empty selection is refused on all three endpoints, so none of them can
// report a successful action over nobody.
func TestAnEmptyBulkSelectionIsRefusedEverywhere(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &bulkSubs{}, Approvals: &bulkApprovals{},
		Lifecycle: &bulkLifecycle{},
	})

	for name, tc := range map[string]struct{ target, body string }{
		"plan change": {"/api/v1/subscribers/bulk/plan-change",
			`{"subscriber_ids":[],"new_plan_id":7}`},
		"status": {"/api/v1/subscribers/bulk/status",
			`{"subscriber_ids":[],"status":"active"}`},
		"credit": {"/api/v1/subscribers/bulk/credit",
			`{"subscriber_ids":[],"amount":"100","reason":"x"}`},
		"plan change, no ids field": {"/api/v1/subscribers/bulk/plan-change",
			`{"new_plan_id":7}`},
	} {
		t.Run(name, func(t *testing.T) {
			rec := bulkCall(h, tc.target, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A bulk credit needs a reason and a positive amount.
func TestFR_WFL_001_BulkCreditValidation(t *testing.T) {
	for name, body := range map[string]string{
		"no reason":    `{"subscriber_ids":[1],"amount":"100.00"}`,
		"no amount":    `{"subscriber_ids":[1],"reason":"goodwill"}`,
		"zero":         `{"subscriber_ids":[1],"amount":"0","reason":"goodwill"}`,
		"negative":     `{"subscriber_ids":[1],"amount":"-100","reason":"goodwill"}`,
		"not a number": `{"subscriber_ids":[1],"amount":"lots","reason":"goodwill"}`,
		"not json":     `{`,
	} {
		t.Run(name, func(t *testing.T) {
			ap := &bulkApprovals{}
			rec := bulkCall(api.NewHandler(api.HandlerDeps{Approvals: ap}),
				"/api/v1/subscribers/bulk/credit", body)
			if rec.Code == http.StatusAccepted {
				t.Errorf("accepted: %s", rec.Body.String())
			}
			if len(ap.created) != 0 {
				t.Errorf("an invalid credit filed %d approval(s)", len(ap.created))
			}
		})
	}
}

// A plan change needs a plan.
func TestFR_LC_001_ABulkPlanChangeNeedsAPlanID(t *testing.T) {
	for name, body := range map[string]string{
		"no plan":  `{"subscriber_ids":[1]}`,
		"zero":     `{"subscriber_ids":[1],"new_plan_id":0}`,
		"negative": `{"subscriber_ids":[1],"new_plan_id":-3}`,
		"not json": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			lc := &bulkLifecycle{}
			rec := bulkCall(api.NewHandler(api.HandlerDeps{Lifecycle: lc}),
				"/api/v1/subscribers/bulk/plan-change", body)
			if rec.Code == http.StatusOK {
				t.Errorf("accepted: %s", rec.Body.String())
			}
			if lc.asked != 0 {
				t.Error("an invalid plan change reached the lifecycle store")
			}
		})
	}
}

// Per-subscriber outcomes are reported individually: one failure in a batch
// of three must not look like three failures, or like none.
func TestEachBulkSubscribersOutcomeIsReportedSeparately(t *testing.T) {
	subs := &bulkSubs{notFound: true}
	h := api.NewHandler(api.HandlerDeps{DB: subs})

	rec := bulkCall(h, "/api/v1/subscribers/bulk/status",
		`{"subscriber_ids":[1,2],"status":"active"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 with per-id results, got %d", rec.Code)
	}

	var out struct {
		Results []struct {
			SubscriberID int    `json:"subscriber_id"`
			OK           bool   `json:"ok"`
			Error        string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(out.Results) != 2 {
		t.Fatalf("got %d results, want one per id", len(out.Results))
	}
	for _, r := range out.Results {
		if r.OK {
			t.Errorf("subscriber %d reported OK although the store found nobody", r.SubscriberID)
		}
		if r.Error == "" {
			t.Errorf("subscriber %d failed with no reason", r.SubscriberID)
		}
	}

	// A store error is reported per id too, and does not abandon the rest.
	broken := &bulkSubs{err: errors.New("store down")}
	rec = bulkCall(api.NewHandler(api.HandlerDeps{DB: broken}),
		"/api/v1/subscribers/bulk/status", `{"subscriber_ids":[1,2,3],"status":"active"}`)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Results) != 3 {
		t.Errorf("a failure on the first id stopped the batch: %d results", len(out.Results))
	}
}

// A failure filing one approval does not stop the others being filed.
func TestFR_WFL_001_OneFailedApprovalDoesNotAbandonTheBatch(t *testing.T) {
	ap := &bulkApprovals{err: errors.New("approval store down")}
	rec := bulkCall(api.NewHandler(api.HandlerDeps{Approvals: ap}),
		"/api/v1/subscribers/bulk/credit",
		`{"subscriber_ids":[1,2,3],"amount":"100.00","reason":"goodwill"}`)

	var out struct {
		Results []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if len(out.Results) != 3 {
		t.Errorf("got %d results, want one per id even when every one failed", len(out.Results))
	}
	for i, r := range out.Results {
		if r.OK || r.Error == "" {
			t.Errorf("result %d: ok=%v error=%q", i, r.OK, r.Error)
		}
	}
}

// Each endpoint reports its own missing dependency, rather than one generic
// message that sends an operator looking in the wrong place.
func TestEachBulkEndpointNamesItsOwnMissingDependency(t *testing.T) {
	bare := api.NewHandler(api.HandlerDeps{})
	for name, tc := range map[string]struct{ target, body string }{
		"plan change": {"/api/v1/subscribers/bulk/plan-change",
			`{"subscriber_ids":[1],"new_plan_id":7}`},
		"status": {"/api/v1/subscribers/bulk/status",
			`{"subscriber_ids":[1],"status":"active"}`},
		"credit": {"/api/v1/subscribers/bulk/credit",
			`{"subscriber_ids":[1],"amount":"100","reason":"x"}`},
	} {
		t.Run(name, func(t *testing.T) {
			rec := bulkCall(bare, tc.target, tc.body)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503, got %d — %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "not configured") {
				t.Errorf("the message should name what is missing, got %s", rec.Body.String())
			}
		})
	}
}

// Every status change is audited and emits the partner event, once per
// subscriber — a bulk suspend of fifty that produced one audit row would be a
// trail that cannot answer who was suspended.
func TestFR_LC_003_EveryBulkStatusChangeIsAuditedIndividually(t *testing.T) {
	sink := &recordingAuditSink{}
	middleware.SetAuditSink(sink)
	t.Cleanup(func() { middleware.SetAuditSink(nil) })

	h := api.NewHandler(api.HandlerDeps{DB: &bulkSubs{}})
	bulkCall(h, "/api/v1/subscribers/bulk/status",
		`{"subscriber_ids":[1,2,3],"status":"soft_suspended"}`)

	targets := map[string]bool{}
	for _, e := range sink.entries() {
		if e.Action == "subscriber.update" {
			targets[e.Target] = true
			if e.Detail["status"] != "soft_suspended" {
				t.Errorf("the trail records status %v", e.Detail["status"])
			}
		}
	}
	for _, want := range []string{"1", "2", "3"} {
		if !targets[want] {
			t.Errorf("subscriber %s has no audit entry; recorded: %v", want, targets)
		}
	}
}

// bulkLifecycle is the minimum the plan-change path needs.
type bulkLifecycle struct {
	err   error
	asked int
}

func (l *bulkLifecycle) GetPlanChangeInfo(_ context.Context, subscriberID, _ int) (*api.PlanChangeInfo, error) {
	l.asked = subscriberID
	return nil, l.err
}

func (l *bulkLifecycle) SetSubscriberPlan(_ context.Context, id, _ int, _ time.Time) (*api.SubscriberRecord, error) {
	return &api.SubscriberRecord{ID: id}, l.err
}

func (l *bulkLifecycle) TerminateSubscriber(_ context.Context, id int) (*api.SubscriberRecord, error) {
	return &api.SubscriberRecord{ID: id}, l.err
}
