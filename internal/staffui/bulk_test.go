package staffui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// Bulk actions on the subscriber directory — FR-SUB-010, FR-NOTIF-010.
//
// bulk.go was at 0%: 92 statements that act on every selected subscriber at
// once, which is what makes a mistake here expensive. The behaviour worth
// pinning hardest is that a bulk wallet credit does *not* credit anything: it
// files one pending approval per subscriber, each still needing a second
// member of staff. A bulk path that applied credits directly would be a way
// round the segregation of duties the single-subscriber path enforces, and the
// only thing standing between the two is which method this handler calls.

var errBulk = errors.New("api: bulk executor unavailable")

type fakeBulk struct {
	planResults   []api.BulkResult
	statusResults []api.BulkResult
	creditResults []api.BulkCreditResult
	enqueued      int
	notifyErr     error

	// What each path was given.
	planIDs   []int
	planID    int
	statusIDs []int
	status    string
	creditIDs []int
	creditBy  string
	creditAmt decimal.Decimal
	creditWhy string
	notifyIDs []int
	notifyBy  string
	title     string
	body      string
	channels  []string
	inPortal  bool

	// Whether a credit was ever applied rather than requested. There is no
	// method for that on this interface, which is the design — this records
	// that the test went looking.
	creditRequested bool
}

func (f *fakeBulk) ChangePlanForMany(_ context.Context, ids []int, planID int) []api.BulkResult {
	f.planIDs, f.planID = ids, planID
	return f.planResults
}

func (f *fakeBulk) UpdateStatusForMany(_ context.Context, ids []int, status string) []api.BulkResult {
	f.statusIDs, f.status = ids, status
	return f.statusResults
}

func (f *fakeBulk) RequestCreditForMany(_ context.Context, requestedBy string, ids []int,
	amount decimal.Decimal, reason string) []api.BulkCreditResult {
	f.creditRequested = true
	f.creditIDs, f.creditBy, f.creditAmt, f.creditWhy = ids, requestedBy, amount, reason
	return f.creditResults
}

func (f *fakeBulk) SendBulkNotification(_ context.Context, requestedBy string, ids []int,
	title, body string, channels []string, showInPortal bool) (int, error) {
	f.notifyIDs, f.notifyBy = ids, requestedBy
	f.title, f.body, f.channels, f.inPortal = title, body, channels, showInPortal
	if f.notifyErr != nil {
		return 0, f.notifyErr
	}
	return f.enqueued, nil
}

func serveBulk(h *Handler, s Session, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/staff/subscribers/bulk",
		strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	w := httptest.NewRecorder()
	h.BulkAction(w, r)
	return w
}

func bulkHandler(b BulkActionExecutor) *Handler {
	// The directory itself has to be renderable: every one of these paths
	// comes back on the subscribers page, and with no directory store the
	// page replaces the message under test with "not configured".
	h := &Handler{revenue: &fakeRevenue{subs: []revenue.SubscriberRow{
		{ID: 1, Username: "one@isp"}, {ID: 2, Username: "two@isp"},
		{ID: 3, Username: "three@isp"},
	}}}
	if b != nil {
		h.bulkActions = b
	}
	return h
}

func ids(vs ...string) url.Values { return url.Values{"ids": vs} }

func withAction(action string, extra url.Values, idList ...string) url.Values {
	v := ids(idList...)
	v.Set("action", action)
	for k, vals := range extra {
		for _, x := range vals {
			v.Add(k, x)
		}
	}
	return v
}

// A bulk credit files approvals. It does not credit anybody, and the page says
// so — an operator who reads "credited" and moves on is the failure this
// wording exists to prevent.
func TestFR_SUB_010_ABulkCreditFilesApprovalsRatherThanCrediting(t *testing.T) {
	f := &fakeBulk{creditResults: []api.BulkCreditResult{
		{SubscriberID: 1, OK: true, ApprovalID: 11},
		{SubscriberID: 2, OK: true, ApprovalID: 12},
	}}
	h := bulkHandler(f)

	rec := serveBulk(h, sess("billing_admin"), withAction("credit",
		url.Values{"amount": {"100.00"}, "reason": {"goodwill after the outage"}}, "1", "2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	if !f.creditRequested {
		t.Fatal("no credit request was filed")
	}
	if f.creditBy != "billing_admin_user" {
		t.Errorf("the approvals were filed as %q; the requester must come from the session",
			f.creditBy)
	}
	if !f.creditAmt.Equal(decimal.RequireFromString("100.00")) {
		t.Errorf("amount = %s", f.creditAmt)
	}
	if f.creditWhy != "goodwill after the outage" {
		t.Errorf("reason = %q", f.creditWhy)
	}

	body := pageText(rec.Body.String())
	if !strings.Contains(body, "2 pending wallet-credit approval(s) filed") {
		t.Errorf("the page should say approvals were filed, got: %s", firstLines(body))
	}
	if !strings.Contains(body, "not applied yet") {
		t.Errorf("the page must say the credits are not applied; an operator who reads "+
			"this as done is the whole risk. Got: %s", firstLines(body))
	}
	if !strings.Contains(body, "second staff member") {
		t.Errorf("the page should say a second person is still needed, got: %s", firstLines(body))
	}
}

// A bulk credit needs a reason and a positive amount, and neither reaches the
// executor when wrong.
func TestFR_SUB_010_ABulkCreditNeedsAReasonAndAPositiveAmount(t *testing.T) {
	for name, tc := range map[string]struct {
		extra url.Values
		want  string
	}{
		"no reason": {url.Values{"amount": {"100.00"}}, "reason is required"},
		"blank reason": {url.Values{"amount": {"100.00"}, "reason": {"   "}},
			"reason is required"},
		"no amount": {url.Values{"reason": {"goodwill"}}, "positive decimal"},
		"zero":      {url.Values{"amount": {"0"}, "reason": {"goodwill"}}, "positive decimal"},
		"negative":  {url.Values{"amount": {"-50"}, "reason": {"goodwill"}}, "positive decimal"},
		"not a number": {url.Values{"amount": {"a lot"}, "reason": {"goodwill"}},
			"positive decimal"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeBulk{}
			rec := serveBulk(bulkHandler(f), sess("billing_admin"),
				withAction("credit", tc.extra, "1"))
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if f.creditRequested {
				t.Error("an invalid credit reached the executor")
			}
		})
	}
}

// Only the owner and billing admin may act in bulk. Checked in the handler
// and not only by the template hiding the form, because a POST does not have
// to come from the page.
func TestFR_SUB_010_OnlyOwnerAndBillingAdminActInBulk(t *testing.T) {
	for _, role := range []string{"csr", "technician", "noc_engineer", "lco",
		"franchise_admin", "franchise_staff"} {
		f := &fakeBulk{}
		rec := serveBulk(bulkHandler(f), sess(role), withAction("status",
			url.Values{"status": {"hard_suspended"}}, "1", "2", "3"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403, got %d", role, rec.Code)
		}
		if f.statusIDs != nil {
			t.Errorf("%s reached the executor", role)
		}
	}

	for _, role := range []string{"isp_owner", "billing_admin"} {
		f := &fakeBulk{statusResults: []api.BulkResult{{SubscriberID: 1, OK: true}}}
		if rec := serveBulk(bulkHandler(f), sess(role), withAction("status",
			url.Values{"status": {"active"}}, "1")); rec.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d", role, rec.Code)
		}
		if f.status != "active" {
			t.Errorf("%s: the executor got status %q", role, f.status)
		}
	}
}

// Only the three statuses the API allows in bulk are accepted. "terminated"
// in particular is not one of them: ending service for a selection in one
// click is not an undo-able mistake.
func TestFR_SUB_010_OnlyTheAllowedStatusesCanBeSetInBulk(t *testing.T) {
	for _, status := range []string{"terminated", "grace_period", "pending_payment", ""} {
		f := &fakeBulk{}
		rec := serveBulk(bulkHandler(f), sess("isp_owner"),
			withAction("status", url.Values{"status": {status}}, "1"))
		if !strings.Contains(pageText(rec.Body.String()), "valid status") {
			t.Errorf("status %q: want a refusal, got: %s", status, firstLines(rec.Body.String()))
		}
		if f.statusIDs != nil {
			t.Errorf("status %q reached the executor", status)
		}
	}

	for status := range api.BulkAllowedStatuses {
		f := &fakeBulk{statusResults: []api.BulkResult{{SubscriberID: 1, OK: true}}}
		serveBulk(bulkHandler(f), sess("isp_owner"),
			withAction("status", url.Values{"status": {status}}, "1"))
		if f.status != status {
			t.Errorf("the allowed status %q did not reach the executor", status)
		}
	}
}

// Nothing selected does nothing, and says so. An empty selection reaching the
// executor would be an action over zero subscribers reported as a success.
func TestFR_SUB_010_AnEmptySelectionIsRefused(t *testing.T) {
	for name, form := range map[string]url.Values{
		"no ids at all":   {"action": {"status"}, "status": {"active"}},
		"unparseable ids": {"action": {"status"}, "status": {"active"}, "ids": {"all", "x"}},
		"blank id":        {"action": {"status"}, "status": {"active"}, "ids": {""}},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeBulk{}
			rec := serveBulk(bulkHandler(f), sess("isp_owner"), form)
			if !strings.Contains(pageText(rec.Body.String()), "Select at least one subscriber") {
				t.Errorf("want a selection message, got: %s", firstLines(rec.Body.String()))
			}
			if f.statusIDs != nil {
				t.Error("an empty selection reached the executor")
			}
		})
	}
}

// Unparseable ids are skipped rather than failing the whole action, but the
// ones that parse still go through — a selection of 50 must not be lost
// because one checkbox sent something odd.
func TestFR_SUB_010_UnparseableIdsAreSkippedNotFatal(t *testing.T) {
	f := &fakeBulk{statusResults: []api.BulkResult{{SubscriberID: 1, OK: true}}}
	serveBulk(bulkHandler(f), sess("isp_owner"), url.Values{
		"action": {"status"}, "status": {"active"},
		"ids": {"1", "not-an-id", "3"},
	})
	if len(f.statusIDs) != 2 || f.statusIDs[0] != 1 || f.statusIDs[1] != 3 {
		t.Errorf("the executor got ids %v, want [1 3]", f.statusIDs)
	}
}

// A plan change needs a plan.
func TestFR_SUB_010_ABulkPlanChangeNeedsAPlan(t *testing.T) {
	for _, planID := range []string{"", "0", "-1", "none"} {
		f := &fakeBulk{}
		rec := serveBulk(bulkHandler(f), sess("isp_owner"),
			withAction("plan_change", url.Values{"plan_id": {planID}}, "1"))
		if !strings.Contains(pageText(rec.Body.String()), "Choose a plan") {
			t.Errorf("plan_id %q: want a refusal, got: %s", planID, firstLines(rec.Body.String()))
		}
		if f.planIDs != nil {
			t.Errorf("plan_id %q reached the executor", planID)
		}
	}

	f := &fakeBulk{planResults: []api.BulkResult{{SubscriberID: 1, OK: true}}}
	serveBulk(bulkHandler(f), sess("isp_owner"),
		withAction("plan_change", url.Values{"plan_id": {"7"}}, "1", "2"))
	if f.planID != 7 || len(f.planIDs) != 2 {
		t.Errorf("the executor got plan %d for ids %v", f.planID, f.planIDs)
	}
}

// Partial failures are reported per subscriber rather than as one number. A
// bulk action over 50 subscribers where 3 failed must name the 3, or nobody
// can go back and fix them.
func TestFR_SUB_010_PartialFailuresNameTheSubscribersThatFailed(t *testing.T) {
	f := &fakeBulk{planResults: []api.BulkResult{
		{SubscriberID: 1, OK: true},
		{SubscriberID: 2, OK: false, Error: "no such plan for this subscriber's franchise"},
		{SubscriberID: 3, OK: true},
	}}

	body := pageText(serveBulk(bulkHandler(f), sess("isp_owner"),
		withAction("plan_change", url.Values{"plan_id": {"7"}}, "1", "2", "3")).Body.String())

	if !strings.Contains(body, "for 2 subscriber(s)") {
		t.Errorf("the success count is missing or wrong: %s", firstLines(body))
	}
	if !strings.Contains(body, "id 2") {
		t.Errorf("the failed subscriber is not named: %s", firstLines(body))
	}
	if !strings.Contains(body, "no such plan") {
		t.Errorf("the reason for the failure is missing: %s", firstLines(body))
	}
}

// A notification needs a title, a message, and somewhere to send it.
func TestFR_NOTIF_010_ABulkNotificationNeedsContentAndAChannel(t *testing.T) {
	for name, tc := range map[string]struct {
		extra url.Values
		want  string
	}{
		"no title": {url.Values{"body": {"we are working on it"}, "channel_sms": {"on"}},
			"title and message are required"},
		"no body": {url.Values{"title": {"Outage"}, "channel_sms": {"on"}},
			"title and message are required"},
		"no channel at all": {url.Values{"title": {"Outage"}, "body": {"we are working on it"}},
			"at least one channel"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeBulk{}
			rec := serveBulk(bulkHandler(f), sess("isp_owner"),
				withAction("notify", tc.extra, "1"))
			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if f.notifyIDs != nil {
				t.Error("an incomplete notification reached the executor")
			}
		})
	}
}

// The portal banner alone is enough — a notice nobody is paged about is still
// a notice.
func TestFR_NOTIF_010_ThePortalBannerAloneIsAValidDestination(t *testing.T) {
	f := &fakeBulk{enqueued: 1}
	rec := serveBulk(bulkHandler(f), sess("isp_owner"), withAction("notify", url.Values{
		"title": {"Outage"}, "body": {"we are working on it"}, "show_in_portal": {"on"},
	}, "1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !f.inPortal {
		t.Error("show_in_portal did not reach the executor")
	}
	if len(f.channels) != 0 {
		t.Errorf("channels = %v, want none", f.channels)
	}
}

// Every ticked channel reaches the executor, and only the ticked ones.
func TestFR_NOTIF_010_OnlyTheTickedChannelsAreUsed(t *testing.T) {
	f := &fakeBulk{enqueued: 3}
	serveBulk(bulkHandler(f), sess("isp_owner"), withAction("notify", url.Values{
		"title": {"Outage"}, "body": {"we are working on it"},
		"channel_sms": {"on"}, "channel_whatsapp": {"on"},
	}, "1"))

	got := map[string]bool{}
	for _, c := range f.channels {
		got[c] = true
	}
	if !got["sms"] || !got["whatsapp"] {
		t.Errorf("the ticked channels did not reach the executor: %v", f.channels)
	}
	if got["email"] {
		t.Errorf("an unticked channel was used: %v", f.channels)
	}
	if f.notifyBy != "isp_owner_user" {
		t.Errorf("the sender was recorded as %q", f.notifyBy)
	}
}

// A failed send says so rather than reporting a delivery that did not happen.
func TestFR_NOTIF_010_AFailedSendIsReported(t *testing.T) {
	h := bulkHandler(&fakeBulk{notifyErr: errBulk})
	body := pageText(serveBulk(h, sess("isp_owner"), withAction("notify", url.Values{
		"title": {"Outage"}, "body": {"we are working on it"}, "channel_sms": {"on"},
	}, "1")).Body.String())

	if !strings.Contains(body, "Could not send") {
		t.Errorf("want a failure message, got: %s", firstLines(body))
	}
	if strings.Contains(body, errBulk.Error()) {
		t.Error("the executor's own error reached the page")
	}
}

// An unknown action does nothing at all.
func TestFR_SUB_010_AnUnknownActionDoesNothing(t *testing.T) {
	f := &fakeBulk{}
	rec := serveBulk(bulkHandler(f), sess("isp_owner"),
		withAction("delete_everything", nil, "1", "2"))
	if !strings.Contains(pageText(rec.Body.String()), "Unknown bulk action") {
		t.Errorf("want a refusal, got: %s", firstLines(rec.Body.String()))
	}
	if f.planIDs != nil || f.statusIDs != nil || f.creditRequested || f.notifyIDs != nil {
		t.Error("an unknown action reached one of the executors")
	}
}

// An unconfigured deployment refuses rather than panicking.
func TestFR_SUB_010_AnUnconfiguredDeploymentRefuses(t *testing.T) {
	rec := serveBulk(bulkHandler(nil), sess("isp_owner"),
		withAction("status", url.Values{"status": {"active"}}, "1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503, got %d", rec.Code)
	}
}
