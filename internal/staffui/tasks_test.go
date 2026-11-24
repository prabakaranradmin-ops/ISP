package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/workflow"
	"github.com/shopspring/decimal"
)

// The Tasks screen — field-task dispatch (FR-WFL-002, CRD-EXP-002) and the
// approvals queue (FR-WFL-001).
//
// tasks.go was at 0%: 143 statements. The approvals half is the most
// sensitive thing in the console. These requests are wallet credits, refunds
// and terminations, and approving one executes it. Two rules carry that:
// only billing admin or owner may decide, and nobody may decide their own
// request. The second is segregation of duties — the control that stops one
// operator crediting their own wallet by requesting it and signing it off a
// moment later. It was enforced and untested.

var errTaskStore = errors.New("store: workflow tables unreachable")

type fakeFieldTasks struct {
	tasks     []workflow.FieldTask
	created   *workflow.FieldTask
	updatedID int
	status    *string
	assignee  *string
	returnNil bool
	listErr   error
	createErr error
	updateErr error
}

func (f *fakeFieldTasks) CreateFieldTask(_ context.Context, t workflow.FieldTask) (*workflow.FieldTask, error) {
	f.created = &t
	if f.createErr != nil {
		return nil, f.createErr
	}
	t.ID = 9
	t.Status = workflow.TaskOpen
	return &t, nil
}

func (f *fakeFieldTasks) ListFieldTasks(_ context.Context, _, _ *string) ([]workflow.FieldTask, error) {
	return f.tasks, f.listErr
}

func (f *fakeFieldTasks) UpdateFieldTask(_ context.Context, id int, status, assignedTo *string, _ *time.Time) (*workflow.FieldTask, error) {
	f.updatedID, f.status, f.assignee = id, status, assignedTo
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	if f.returnNil {
		return nil, nil
	}
	return &workflow.FieldTask{ID: id, Title: "Install at 12 Anna Nagar"}, nil
}

type fakeApprovals struct {
	pending  []workflow.ApprovalRequest
	one      *workflow.ApprovalRequest
	rejected *workflow.ApprovalRequest
	// decidedBy and reason record what the store was told, which is what
	// ends up on the row somebody later reads as evidence.
	decidedBy string
	reason    string
	listErr   error
	getErr    error
	rejectErr error
}

func (f *fakeApprovals) ListApprovalRequests(_ context.Context, _ *workflow.Status, _ *int) ([]workflow.ApprovalRequest, error) {
	return f.pending, f.listErr
}

func (f *fakeApprovals) GetApprovalRequest(_ context.Context, _ int) (*workflow.ApprovalRequest, error) {
	return f.one, f.getErr
}

func (f *fakeApprovals) RejectApprovalRequest(_ context.Context, id int, decidedBy, reason string) (*workflow.ApprovalRequest, error) {
	f.decidedBy, f.reason = decidedBy, reason
	if f.rejectErr != nil {
		return nil, f.rejectErr
	}
	if f.rejected != nil {
		return f.rejected, nil
	}
	return &workflow.ApprovalRequest{ID: id, Status: workflow.StatusRejected}, nil
}

type fakeExecutor struct {
	result   *workflow.ApprovalRequest
	err      error
	actor    string
	executed int
}

func (f *fakeExecutor) ExecuteApprovedRequest(_ context.Context, id int, actor string) (*workflow.ApprovalRequest, error) {
	f.executed, f.actor = id, actor
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &workflow.ApprovalRequest{ID: id, Status: workflow.StatusExecuted}, nil
}

func serveTasks(h *Handler, s Session, method, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i, p := range parts {
		if p == "approvals" || p == "tasks" {
			if len(parts) > i+1 {
				r.SetPathValue("id", parts[i+1])
			}
		}
	}

	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(r.URL.Path, "/approve"):
		h.ApproveTaskRequest(w, r)
	case strings.HasSuffix(r.URL.Path, "/reject"):
		h.RejectTaskRequest(w, r)
	case method == http.MethodPost && strings.Contains(r.URL.Path, "/tasks/"):
		h.UpdateFieldTaskForm(w, r)
	case method == http.MethodPost:
		h.CreateFieldTaskForm(w, r)
	default:
		h.Tasks(w, r)
	}
	return w
}

func sess(role string) Session {
	return Session{StaffID: 9, Username: role + "_user", Role: role}
}

func pendingRequest(requestedBy string) *workflow.ApprovalRequest {
	amount := decimal.RequireFromString("5000.00")
	return &workflow.ApprovalRequest{
		ID: 4, ActionType: workflow.ActionWalletCredit, SubscriberID: 12,
		Amount: &amount, Reason: "goodwill after an outage",
		Status: workflow.StatusPending, RequestedBy: requestedBy,
	}
}

func tasksHandler(ft *fakeFieldTasks, ap *fakeApprovals, ex *fakeExecutor) *Handler {
	h := &Handler{}
	if ft != nil {
		h.fieldTasks = ft
	}
	if ap != nil {
		h.approvals = ap
	}
	if ex != nil {
		h.approvalExecutor = ex
	}
	return h
}

// Nobody decides their own request. This is the control that stops one
// operator crediting their own wallet by requesting it and approving it a
// moment later, and approving executes the credit — so the test asserts the
// executor was never reached, not merely that the page complained.
func TestFR_WFL_001_NobodyApprovesTheirOwnRequest(t *testing.T) {
	ap := &fakeApprovals{one: pendingRequest("billing_admin_user")}
	ex := &fakeExecutor{}
	h := tasksHandler(nil, ap, ex)

	rec := serveTasks(h, sess("billing_admin"), http.MethodPost, "/staff/approvals/4/approve", nil)
	if ex.executed != 0 {
		t.Errorf("the request was executed by its own requester (id %d, actor %q)",
			ex.executed, ex.actor)
	}
	if !strings.Contains(pageText(rec.Body.String()), "Could not approve") {
		t.Errorf("want a refusal on the page, got: %s", firstLines(rec.Body.String()))
	}
}

// Nor rejects it. Less obviously dangerous than self-approval and just as
// much a hole in the control: a requester who can reject can also withdraw
// the evidence that they asked.
func TestFR_WFL_001_NobodyRejectsTheirOwnRequestEither(t *testing.T) {
	ap := &fakeApprovals{one: pendingRequest("billing_admin_user")}
	h := tasksHandler(nil, ap, nil)

	rec := serveTasks(h, sess("billing_admin"), http.MethodPost, "/staff/approvals/4/reject",
		url.Values{"reason": {"changed my mind"}})
	if ap.decidedBy != "" {
		t.Errorf("the rejection reached the store, decided by %q", ap.decidedBy)
	}
	if !strings.Contains(pageText(rec.Body.String()), "Could not reject") {
		t.Errorf("want a refusal on the page, got: %s", firstLines(rec.Body.String()))
	}
}

// An already-decided request cannot be decided again, on either path.
func TestFR_WFL_001_AnAlreadyDecidedRequestCannotBeDecidedAgain(t *testing.T) {
	decided := pendingRequest("csr_user")
	decided.Status = workflow.StatusExecuted

	for name, target := range map[string]string{
		"approve": "/staff/approvals/4/approve",
		"reject":  "/staff/approvals/4/reject",
	} {
		t.Run(name, func(t *testing.T) {
			ap := &fakeApprovals{one: decided}
			ex := &fakeExecutor{}
			rec := serveTasks(tasksHandler(nil, ap, ex), sess("isp_owner"),
				http.MethodPost, target, url.Values{"reason": {"no"}})
			if ex.executed != 0 || ap.decidedBy != "" {
				t.Error("a decided request was decided again")
			}
			if body := pageText(rec.Body.String()); !strings.Contains(body, "Could not") {
				t.Errorf("want a refusal, got: %s", firstLines(body))
			}
		})
	}
}

// Only billing admin and owner decide. A CSR who can file the request must
// not be able to sign it off.
func TestFR_WFL_001_OnlyBillingAdminOrOwnerDecides(t *testing.T) {
	for _, role := range []string{"csr", "technician", "noc_engineer", "lco"} {
		ap := &fakeApprovals{one: pendingRequest("someone_else")}
		ex := &fakeExecutor{}
		h := tasksHandler(nil, ap, ex)

		for _, target := range []string{"/staff/approvals/4/approve", "/staff/approvals/4/reject"} {
			rec := serveTasks(h, sess(role), http.MethodPost, target, url.Values{"reason": {"x"}})
			if rec.Code == http.StatusOK {
				t.Errorf("%s was allowed to decide via %s", role, target)
			}
			if ex.executed != 0 || ap.decidedBy != "" {
				t.Errorf("%s reached the execution path via %s", role, target)
			}
		}
	}

	// And the two who may, do.
	for _, role := range []string{"billing_admin", "isp_owner"} {
		ex := &fakeExecutor{}
		h := tasksHandler(nil, &fakeApprovals{one: pendingRequest("csr_user")}, ex)
		rec := serveTasks(h, sess(role), http.MethodPost, "/staff/approvals/4/approve", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s got %d approving", role, rec.Code)
		}
		if ex.actor != role+"_user" {
			t.Errorf("the executor was told the actor is %q, want %q", ex.actor, role+"_user")
		}
	}
}

// An approval that is recorded but whose action fails must say both things.
// "Approved" alone would leave an operator believing a refund went out.
func TestFR_WFL_001_ARecordedApprovalWhoseActionFailedSaysBoth(t *testing.T) {
	failure := "wallet: ledger write rejected"
	result := pendingRequest("csr_user")
	result.Status = workflow.StatusExecutionFailed
	result.ExecutionError = &failure

	h := tasksHandler(nil, &fakeApprovals{one: pendingRequest("csr_user")},
		&fakeExecutor{result: result})

	body := pageText(serveTasks(h, sess("isp_owner"), http.MethodPost,
		"/staff/approvals/4/approve", nil).Body.String())
	if !strings.Contains(body, "was recorded") || !strings.Contains(body, failure) {
		t.Errorf("the page should say the approval stuck and the action did not, got: %s",
			firstLines(body))
	}
}

// A successful approval names what happened.
func TestFR_WFL_001_ASuccessfulApprovalIsConfirmed(t *testing.T) {
	ex := &fakeExecutor{}
	h := tasksHandler(nil, &fakeApprovals{one: pendingRequest("csr_user")}, ex)

	body := pageText(serveTasks(h, sess("isp_owner"), http.MethodPost,
		"/staff/approvals/4/approve", nil).Body.String())
	if !strings.Contains(body, "#4 approved and executed") {
		t.Errorf("want a confirmation naming the request, got: %s", firstLines(body))
	}
	if ex.executed != 4 {
		t.Errorf("the executor was given id %d", ex.executed)
	}
}

// A rejection needs a reason, and the reason reaches the row. The row is the
// evidence of why somebody's refund was declined.
func TestFR_WFL_001_ARejectionNeedsAReasonAndRecordsIt(t *testing.T) {
	ap := &fakeApprovals{one: pendingRequest("csr_user")}
	h := tasksHandler(nil, ap, nil)

	blank := serveTasks(h, sess("billing_admin"), http.MethodPost,
		"/staff/approvals/4/reject", url.Values{"reason": {"   "}})
	if !strings.Contains(pageText(blank.Body.String()), "reason is required") {
		t.Errorf("want a reason-required message, got: %s", firstLines(blank.Body.String()))
	}
	if ap.decidedBy != "" {
		t.Error("a reasonless rejection reached the store")
	}

	ok := serveTasks(h, sess("billing_admin"), http.MethodPost,
		"/staff/approvals/4/reject", url.Values{"reason": {"no outage on that date"}})
	if !strings.Contains(pageText(ok.Body.String()), "#4 rejected") {
		t.Errorf("want a confirmation, got: %s", firstLines(ok.Body.String()))
	}
	if ap.reason != "no outage on that date" {
		t.Errorf("the stored reason is %q", ap.reason)
	}
	if ap.decidedBy != "billing_admin_user" {
		t.Errorf("the row records %q as the decider", ap.decidedBy)
	}
}

// A missing request is reported rather than treated as pending.
func TestFR_WFL_001_AMissingRequestIsReported(t *testing.T) {
	for name, ap := range map[string]*fakeApprovals{
		"no row": {one: nil},
		"error":  {getErr: errTaskStore},
	} {
		t.Run(name, func(t *testing.T) {
			ex := &fakeExecutor{}
			rec := serveTasks(tasksHandler(nil, ap, ex), sess("isp_owner"),
				http.MethodPost, "/staff/approvals/4/approve", nil)
			if ex.executed != 0 {
				t.Error("a request that could not be read was executed anyway")
			}
			if !strings.Contains(pageText(rec.Body.String()), "not found") {
				t.Errorf("want a not-found message, got: %s", firstLines(rec.Body.String()))
			}
		})
	}
}

// The approvals panel is only rendered for the two roles that can act on it.
// A CSR seeing a queue of pending wallet credits they cannot decide is both
// noise and a disclosure.
func TestFR_WFL_001_TheApprovalsPanelIsOnlyRenderedForDeciders(t *testing.T) {
	ap := &fakeApprovals{pending: []workflow.ApprovalRequest{*pendingRequest("csr_user")}}
	h := tasksHandler(&fakeFieldTasks{}, ap, nil)

	for _, role := range []string{"csr", "technician", "noc_engineer"} {
		body := pageText(serveTasks(h, sess(role), http.MethodGet, "/staff/tasks", nil).Body.String())
		if strings.Contains(body, "goodwill after an outage") {
			t.Errorf("%s was shown a pending approval's detail", role)
		}
	}
	for _, role := range []string{"billing_admin", "isp_owner"} {
		body := pageText(serveTasks(h, sess(role), http.MethodGet, "/staff/tasks", nil).Body.String())
		if !strings.Contains(body, "goodwill after an outage") {
			t.Errorf("%s was not shown the pending approval", role)
		}
	}
}

// Dispatching field tasks is csr/technician/owner. Everyone who reaches the
// screen sees the queue; not everyone may change it.
func TestFR_WFL_002_OnlyCSRTechnicianOrOwnerDispatchesFieldTasks(t *testing.T) {
	for _, role := range []string{"billing_admin", "noc_engineer", "lco"} {
		ft := &fakeFieldTasks{}
		h := tasksHandler(ft, nil, nil)
		for _, target := range []string{"/staff/tasks", "/staff/tasks/9"} {
			rec := serveTasks(h, sess(role), http.MethodPost, target, url.Values{
				"title": {"Install"}, "assigned_to": {"ravi"},
			})
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s POST %s: want 403, got %d", role, target, rec.Code)
			}
		}
		if ft.created != nil || ft.updatedID != 0 {
			t.Errorf("%s reached the task store", role)
		}
	}
}

// Filing a task records who filed it, from the session rather than the form.
func TestFR_WFL_002_AFiledTaskRecordsWhoFiledItFromTheSession(t *testing.T) {
	ft := &fakeFieldTasks{}
	h := tasksHandler(ft, nil, nil)

	rec := serveTasks(h, sess("csr"), http.MethodPost, "/staff/tasks", url.Values{
		"title": {"  Install at 12 Anna Nagar  "}, "assigned_to": {"ravi"},
		"description": {"second floor"}, "subscriber_id": {"12"}, "due_date": {"2026-12-31"},
		"created_by": {"somebody_else"}, // must be ignored
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if ft.created == nil {
		t.Fatal("nothing reached the store")
	}
	if ft.created.CreatedBy != "csr_user" {
		t.Errorf("created_by = %q; it must come from the session, not the form",
			ft.created.CreatedBy)
	}
	if ft.created.Title != "Install at 12 Anna Nagar" {
		t.Errorf("title = %q; it should be trimmed", ft.created.Title)
	}
	if ft.created.SubscriberID == nil || *ft.created.SubscriberID != 12 {
		t.Errorf("subscriber id = %v", ft.created.SubscriberID)
	}
	if ft.created.DueDate == nil || ft.created.DueDate.Format("2006-01-02") != "2026-12-31" {
		t.Errorf("due date = %v", ft.created.DueDate)
	}
}

// The optional fields are optional, and malformed ones are refused with the
// reason rather than silently dropped.
func TestFR_WFL_002_TaskFormValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		form  url.Values
		want  string
		wrote bool
	}{
		"no title": {url.Values{"assigned_to": {"ravi"}},
			"Title and assignee are required", false},
		"no assignee": {url.Values{"title": {"Install"}},
			"Title and assignee are required", false},
		"subscriber id not a number": {url.Values{
			"title": {"Install"}, "assigned_to": {"ravi"}, "subscriber_id": {"twelve"},
		}, "must be a number", false},
		"bad due date": {url.Values{
			"title": {"Install"}, "assigned_to": {"ravi"}, "due_date": {"31/12/2026"},
		}, "must be a date like", false},
		"no optional fields at all": {url.Values{
			"title": {"Install"}, "assigned_to": {"ravi"},
		}, "assigned to ravi", true},
	} {
		t.Run(name, func(t *testing.T) {
			ft := &fakeFieldTasks{}
			rec := serveTasks(tasksHandler(ft, nil, nil), sess("technician"),
				http.MethodPost, "/staff/tasks", tc.form)
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if wrote := ft.created != nil; wrote != tc.wrote {
				t.Errorf("reached the store = %v, want %v", wrote, tc.wrote)
			}
		})
	}
}

// Updating a task passes only the fields that were supplied, so a form that
// changes the status does not also blank the assignee.
func TestFR_WFL_002_AnUpdateOnlySendsWhatWasSupplied(t *testing.T) {
	ft := &fakeFieldTasks{}
	h := tasksHandler(ft, nil, nil)

	serveTasks(h, sess("technician"), http.MethodPost, "/staff/tasks/9",
		url.Values{"status": {workflow.TaskCompleted}, "assigned_to": {""}})
	if ft.status == nil || *ft.status != workflow.TaskCompleted {
		t.Errorf("status reached the store as %v", ft.status)
	}
	if ft.assignee != nil {
		t.Errorf("a blank assignee was sent as %q; it would reassign the task to nobody",
			*ft.assignee)
	}
	if ft.updatedID != 9 {
		t.Errorf("the store was asked to update task %d", ft.updatedID)
	}
}

// A task id that does not exist is reported, not confirmed.
func TestFR_WFL_002_AnUnknownTaskIsReported(t *testing.T) {
	h := tasksHandler(&fakeFieldTasks{returnNil: true}, nil, nil)
	body := pageText(serveTasks(h, sess("csr"), http.MethodPost, "/staff/tasks/404",
		url.Values{"status": {workflow.TaskCompleted}}).Body.String())
	if !strings.Contains(body, "No task with that id") {
		t.Errorf("want a not-found message, got: %s", firstLines(body))
	}

	bad := tasksHandler(&fakeFieldTasks{}, nil, nil)
	body = pageText(serveTasks(bad, sess("csr"), http.MethodPost, "/staff/tasks/not-a-number",
		url.Values{"status": {workflow.TaskCompleted}}).Body.String())
	if !strings.Contains(body, "Invalid task id") {
		t.Errorf("want an invalid-id message, got: %s", firstLines(body))
	}
}

// A failing field-task store leaves the rest of the screen usable, and an
// approvals failure does not hide a field-task failure that happened first.
func TestFR_WFL_002_StoreFailuresAreReportedWithoutLosingTheOtherPanel(t *testing.T) {
	both := tasksHandler(&fakeFieldTasks{listErr: errTaskStore},
		&fakeApprovals{listErr: errTaskStore}, nil)
	rec := serveTasks(both, sess("isp_owner"), http.MethodGet, "/staff/tasks", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("want a rendered page, got %d", rec.Code)
	}
	body := pageText(rec.Body.String())
	if !strings.Contains(body, "Could not load field tasks") {
		t.Errorf("the first failure should be the one reported, got: %s", firstLines(body))
	}
	if strings.Contains(body, errTaskStore.Error()) {
		t.Error("the store's own error reached the page")
	}

	onlyApprovals := tasksHandler(&fakeFieldTasks{}, &fakeApprovals{listErr: errTaskStore}, nil)
	body = pageText(serveTasks(onlyApprovals, sess("isp_owner"),
		http.MethodGet, "/staff/tasks", nil).Body.String())
	if !strings.Contains(body, "Could not load pending approvals") {
		t.Errorf("want the approvals message, got: %s", firstLines(body))
	}
}

// An unconfigured deployment refuses the actions rather than panicking.
func TestFR_WFL_001_AnUnconfiguredDeploymentRefusesTheActions(t *testing.T) {
	bare := &Handler{}

	if code := serveTasks(bare, sess("csr"), http.MethodGet, "/staff/tasks", nil).Code; code != http.StatusOK {
		t.Errorf("the screen itself should still render, got %d", code)
	}
	for name, tc := range map[string]struct {
		s      Session
		target string
	}{
		"create":  {sess("csr"), "/staff/tasks"},
		"update":  {sess("csr"), "/staff/tasks/9"},
		"approve": {sess("isp_owner"), "/staff/approvals/4/approve"},
		"reject":  {sess("isp_owner"), "/staff/approvals/4/reject"},
	} {
		t.Run(name, func(t *testing.T) {
			rec := serveTasks(bare, tc.s, http.MethodPost, tc.target,
				url.Values{"title": {"x"}, "assigned_to": {"y"}, "reason": {"z"}})
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503, got %d", rec.Code)
			}
		})
	}

	// Approvals configured but no executor is still unusable, and must say
	// so rather than record an approval nothing will carry out.
	noExec := tasksHandler(nil, &fakeApprovals{one: pendingRequest("csr_user")}, nil)
	if code := serveTasks(noExec, sess("isp_owner"), http.MethodPost,
		"/staff/approvals/4/approve", nil).Code; code != http.StatusServiceUnavailable {
		t.Errorf("want 503 with no executor wired, got %d", code)
	}
}

// A bad approval id is reported before anything is read or executed.
func TestFR_WFL_001_ABadApprovalIdIsRefused(t *testing.T) {
	ap := &fakeApprovals{one: pendingRequest("csr_user")}
	ex := &fakeExecutor{}
	h := tasksHandler(nil, ap, ex)

	for _, target := range []string{
		"/staff/approvals/not-a-number/approve",
		"/staff/approvals/not-a-number/reject",
	} {
		rec := serveTasks(h, sess("isp_owner"), http.MethodPost, target,
			url.Values{"reason": {"x"}})
		if !strings.Contains(pageText(rec.Body.String()), "Invalid approval id") {
			t.Errorf("%s: want an invalid-id message, got: %s", target,
				firstLines(rec.Body.String()))
		}
	}
	if ex.executed != 0 || ap.decidedBy != "" {
		t.Error("a malformed id reached the decision path")
	}
}

// The field-task queue renders for every role that reaches the screen.
func TestFR_WFL_002_TheQueueRendersForEveryRoleThatReachesTheScreen(t *testing.T) {
	due := time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	h := tasksHandler(&fakeFieldTasks{tasks: []workflow.FieldTask{
		{ID: 9, Title: "Install at 12 Anna Nagar", AssignedTo: "ravi",
			Status: workflow.TaskOpen, DueDate: &due},
	}}, nil, nil)

	for _, role := range []string{"isp_owner", "billing_admin", "csr", "technician", "noc_engineer"} {
		rec := serveTasks(h, sess(role), http.MethodGet, "/staff/tasks", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s got %d", role, rec.Code)
			continue
		}
		if body := pageText(rec.Body.String()); !strings.Contains(body, "Install at 12 Anna Nagar") {
			t.Errorf("%s was not shown the queue", role)
		}
	}
}
