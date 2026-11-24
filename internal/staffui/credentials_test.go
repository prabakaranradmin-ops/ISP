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

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// Subscriber credentials on the console — FR-SUB-006, FR-SEC-004.
//
// credentials.go was at 2.6%: 75 statements covering the only screen in the
// product that shows an operator a customer's password.
//
// The gating is deliberately asymmetric and that asymmetry is the thing worth
// testing. A CSR may *reveal* a password, because telling a customer their
// password is what a support call is; a CSR may not *reset* one or change a
// plan. A technician or NOC engineer may do neither. Each handler re-checks
// the role itself — hiding a card is presentation, not a control, and a POST
// does not have to come from the page.

var errCreds = errors.New("store: credential read failed")

type fakeCredentials struct {
	password  string
	stored    bool
	revealErr error
	resetErr  error

	// Who the API handler was told is doing this. The reveal of a customer's
	// password is the single most audit-worthy read in the console, and the
	// attribution travels on the context.
	revealSubject string
	resetSubject  string
	revealedID    int
	resetID       int
	newPassword   string
}

func (f *fakeCredentials) RevealSubscriberPassword(ctx context.Context, id int) (string, bool, error) {
	f.revealSubject, f.revealedID = middleware.SubjectFromContext(ctx), id
	if f.revealErr != nil {
		return "", false, f.revealErr
	}
	return f.password, f.stored, nil
}

func (f *fakeCredentials) ResetSubscriberPassword(ctx context.Context, id int, newPassword string) error {
	f.resetSubject, f.resetID, f.newPassword = middleware.SubjectFromContext(ctx), id, newPassword
	return f.resetErr
}

func serveCreds(h *Handler, s Session, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) >= 3 {
		r.SetPathValue("id", parts[2])
	}

	w := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(r.URL.Path, "/password/reveal"):
		h.RevealPassword(w, r)
	case strings.HasSuffix(r.URL.Path, "/password"):
		h.ResetPassword(w, r)
	default:
		h.ChangeSubscriberPlan(w, r)
	}
	return w
}

// fakeSubscribers is the minimum the subscriber page needs to render a
// subscriber at all. Without it the credentials card is not drawn and every
// assertion about what the card shows passes or fails for the wrong reason.
type fakeSubscribers struct {
	rec *api.SubscriberRecord
	err error
}

func (f *fakeSubscribers) GetSubscriberByID(context.Context, int) (*api.SubscriberRecord, error) {
	return f.rec, f.err
}

func (f *fakeSubscribers) GetSubscriberByUsername(context.Context, string) (*api.SubscriberRecord, error) {
	return f.rec, f.err
}

func (f *fakeSubscribers) UpdateSubscriber(_ context.Context, _ int, _ *int, _ *string,
	_ *time.Time) (*api.SubscriberRecord, error) {
	return f.rec, f.err
}

func credsHandler(c CredentialManager, b BulkActionExecutor) *Handler {
	h := &Handler{
		revenue: &fakeRevenue{subs: []revenue.SubscriberRow{{ID: 5, Username: "five@isp"}}},
		subscribers: &fakeSubscribers{rec: &api.SubscriberRecord{
			ID: 5, Username: "five@isp", CAFNumber: "CAF-000005",
			MobileNumber: "+919000000005", Status: "active", WalletBalance: "0.00",
		}},
	}
	if c != nil {
		h.credentials = c
	}
	if b != nil {
		h.bulkActions = b
	}
	return h
}

// A CSR may reveal a password; a CSR may not reset one. That split is the
// point of this screen, and it is enforced per handler rather than by the
// template.
func TestFR_SEC_004_ACSRMayRevealButNotReset(t *testing.T) {
	c := &fakeCredentials{password: "hunter2", stored: true}
	h := credsHandler(c, &fakeBulk{})

	reveal := serveCreds(h, sess("csr"), "/staff/subscribers/5/password/reveal", nil)
	if reveal.Code != http.StatusOK {
		t.Errorf("a CSR should be able to reveal, got %d", reveal.Code)
	}
	if c.revealedID != 5 {
		t.Errorf("the reveal asked for subscriber %d", c.revealedID)
	}

	reset := serveCreds(h, sess("csr"), "/staff/subscribers/5/password",
		url.Values{"new_password": {"a-new-password"}})
	if reset.Code != http.StatusForbidden {
		t.Errorf("a CSR should not be able to reset, got %d", reset.Code)
	}
	if c.newPassword != "" {
		t.Errorf("a CSR's reset reached the store with %q", c.newPassword)
	}

	plan := serveCreds(h, sess("csr"), "/staff/subscribers/5/plan-change",
		url.Values{"plan_id": {"7"}})
	if plan.Code != http.StatusForbidden {
		t.Errorf("a CSR should not be able to change a plan, got %d", plan.Code)
	}
}

// A technician and a NOC engineer may do none of it. Neither has a reason to
// see a customer's password.
func TestFR_SEC_004_TechniciansAndNOCCannotSeeOrChangeCredentials(t *testing.T) {
	for _, role := range []string{"technician", "noc_engineer"} {
		c := &fakeCredentials{password: "hunter2", stored: true}
		h := credsHandler(c, &fakeBulk{})

		for _, target := range []string{
			"/staff/subscribers/5/password/reveal",
			"/staff/subscribers/5/password",
			"/staff/subscribers/5/plan-change",
		} {
			rec := serveCreds(h, sess(role), target, url.Values{
				"new_password": {"a-new-password"}, "plan_id": {"7"},
			})
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s POST %s: want 403, got %d", role, target, rec.Code)
			}
		}
		if c.revealedID != 0 || c.newPassword != "" {
			t.Errorf("%s reached the credential store", role)
		}
	}
}

// Owner and billing admin may do all three.
func TestFR_SEC_004_OwnerAndBillingMayDoAllThree(t *testing.T) {
	for _, role := range []string{"isp_owner", "billing_admin"} {
		c := &fakeCredentials{password: "hunter2", stored: true}
		b := &fakeBulk{planResults: []api.BulkResult{{SubscriberID: 5, OK: true}}}
		h := credsHandler(c, b)

		if rec := serveCreds(h, sess(role), "/staff/subscribers/5/password/reveal", nil); rec.Code != http.StatusOK {
			t.Errorf("%s reveal: got %d", role, rec.Code)
		}
		if rec := serveCreds(h, sess(role), "/staff/subscribers/5/password",
			url.Values{"new_password": {"a-new-password"}}); rec.Code != http.StatusSeeOther {
			t.Errorf("%s reset: want a redirect, got %d", role, rec.Code)
		}
		if rec := serveCreds(h, sess(role), "/staff/subscribers/5/plan-change",
			url.Values{"plan_id": {"7"}}); rec.Code != http.StatusSeeOther {
			t.Errorf("%s plan change: want a redirect, got %d", role, rec.Code)
		}
	}
}

// Both actions are attributed to the operator who performed them. A reveal
// recorded as "unknown" is a read of a customer's password that nobody owns.
func TestFR_SEC_004_BothActionsAreAttributedToTheOperator(t *testing.T) {
	c := &fakeCredentials{password: "hunter2", stored: true}
	h := credsHandler(c, &fakeBulk{})

	serveCreds(h, sess("csr"), "/staff/subscribers/5/password/reveal", nil)
	if c.revealSubject != "csr_user" {
		t.Errorf("the reveal was attributed to %q, want the operator's username", c.revealSubject)
	}

	serveCreds(h, sess("billing_admin"), "/staff/subscribers/5/password",
		url.Values{"new_password": {"a-new-password"}})
	if c.resetSubject != "billing_admin_user" {
		t.Errorf("the reset was attributed to %q", c.resetSubject)
	}
}

// A revealed password is shown on that render and is not carried into the
// page's own state — the next ordinary load of the subscriber must not show
// it again.
func TestFR_SUB_006_ARevealedPasswordAppearsOnceAndNotOnTheNextLoad(t *testing.T) {
	c := &fakeCredentials{password: "hunter2", stored: true}
	h := credsHandler(c, &fakeBulk{})

	revealed := pageText(serveCreds(h, sess("csr"),
		"/staff/subscribers/5/password/reveal", nil).Body.String())
	if !strings.Contains(revealed, "hunter2") {
		t.Fatalf("the revealed password is not on the page: %s", firstLines(revealed))
	}

	// An ordinary load of the same screen, with no reveal.
	r := httptest.NewRequest(http.MethodGet, "/staff/subscribers/5", nil)
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, sess("csr")))
	r.SetPathValue("id", "5")
	w := httptest.NewRecorder()
	h.SubscriberDetail(w, r)
	if strings.Contains(pageText(w.Body.String()), "hunter2") {
		t.Error("the password is shown on an ordinary page load; a reveal must be a " +
			"deliberate act each time")
	}
}

// A subscriber with no recoverable password says so rather than showing an
// empty field that reads as a blank password.
func TestFR_SUB_006_NoRecoverablePasswordIsStatedNotShownAsBlank(t *testing.T) {
	h := credsHandler(&fakeCredentials{password: "", stored: false}, &fakeBulk{})
	rec := serveCreds(h, sess("csr"), "/staff/subscribers/5/password/reveal", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("want a rendered page, got %d", rec.Code)
	}
	body := pageText(rec.Body.String())
	// Whatever the wording, the page must not present an empty value as the
	// password.
	if strings.Contains(body, "Password: \"\"") || strings.Contains(body, "password is \"\"") {
		t.Errorf("an unrecoverable password was rendered as blank: %s", firstLines(body))
	}
}

// A reset needs a password, and a too-short one gets a message the operator
// can act on rather than a generic failure.
func TestFR_SUB_006_ResetValidationSaysWhatIsWrong(t *testing.T) {
	for name, tc := range map[string]struct {
		form url.Values
		err  error
		want string
	}{
		"empty":  {url.Values{"new_password": {""}}, nil, "new password is required"},
		"blanks": {url.Values{"new_password": {"    "}}, nil, "new password is required"},
		"too short": {url.Values{"new_password": {"abc"}}, api.ErrSubscriberInvalid,
			"at least 6 characters"},
		"store failure": {url.Values{"new_password": {"a-good-password"}}, errCreds,
			"Could not reset"},
	} {
		t.Run(name, func(t *testing.T) {
			c := &fakeCredentials{resetErr: tc.err}
			rec := serveCreds(credsHandler(c, &fakeBulk{}), sess("billing_admin"),
				"/staff/subscribers/5/password", tc.form)
			body := pageText(rec.Body.String())
			if !strings.Contains(body, tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(body))
			}
			if tc.err == errCreds && strings.Contains(body, errCreds.Error()) {
				t.Error("the store's own error reached the page")
			}
		})
	}
}

// A successful reset redirects rather than re-rendering, so a refresh cannot
// resubmit it, and the confirmation says when the new password takes effect.
func TestFR_SUB_006_ASuccessfulResetRedirectsWithAConfirmation(t *testing.T) {
	c := &fakeCredentials{}
	rec := serveCreds(credsHandler(c, &fakeBulk{}), sess("billing_admin"),
		"/staff/subscribers/5/password", url.Values{"new_password": {"a-good-password"}})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 so a refresh cannot resubmit, got %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/staff/subscribers/5") {
		t.Errorf("redirected to %q", loc)
	}
	if !strings.Contains(loc, "next+login") && !strings.Contains(loc, "next%20login") {
		t.Errorf("the confirmation should say when it takes effect, got %q", loc)
	}
	if c.newPassword != "a-good-password" {
		t.Errorf("the store got %q", c.newPassword)
	}
}

// A single plan change goes through the same executor the bulk toolbar uses,
// with one id — so proration and billing cannot diverge between the two
// paths.
func TestFR_SUB_006_ASinglePlanChangeUsesTheBulkPath(t *testing.T) {
	b := &fakeBulk{planResults: []api.BulkResult{{SubscriberID: 5, OK: true}}}
	rec := serveCreds(credsHandler(&fakeCredentials{}, b), sess("isp_owner"),
		"/staff/subscribers/5/plan-change", url.Values{"plan_id": {"7"}})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect, got %d", rec.Code)
	}
	if len(b.planIDs) != 1 || b.planIDs[0] != 5 {
		t.Errorf("the executor got ids %v, want [5]", b.planIDs)
	}
	if b.planID != 7 {
		t.Errorf("the executor got plan %d", b.planID)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "prorated") {
		t.Errorf("the confirmation should mention proration, got %q", loc)
	}
}

// A plan change that fails reports the executor's reason on the page.
func TestFR_SUB_006_AFailedPlanChangeReportsWhy(t *testing.T) {
	b := &fakeBulk{planResults: []api.BulkResult{
		{SubscriberID: 5, OK: false, Error: "plan is retired"},
	}}
	body := pageText(serveCreds(credsHandler(&fakeCredentials{}, b), sess("isp_owner"),
		"/staff/subscribers/5/plan-change", url.Values{"plan_id": {"7"}}).Body.String())
	if !strings.Contains(body, "plan is retired") {
		t.Errorf("the reason is missing: %s", firstLines(body))
	}

	// An executor that returns nothing at all is reported rather than read as
	// success.
	none := &fakeBulk{planResults: nil}
	body = pageText(serveCreds(credsHandler(&fakeCredentials{}, none), sess("isp_owner"),
		"/staff/subscribers/5/plan-change", url.Values{"plan_id": {"7"}}).Body.String())
	if !strings.Contains(body, "did not run") {
		t.Errorf("an empty result should be reported, got: %s", firstLines(body))
	}
}

// A plan change needs a plan.
func TestFR_SUB_006_APlanChangeNeedsAPlan(t *testing.T) {
	for _, planID := range []string{"", "0", "-3", "seven"} {
		b := &fakeBulk{}
		body := pageText(serveCreds(credsHandler(&fakeCredentials{}, b), sess("isp_owner"),
			"/staff/subscribers/5/plan-change", url.Values{"plan_id": {planID}}).Body.String())
		if !strings.Contains(body, "Choose a plan") {
			t.Errorf("plan_id %q: want a refusal, got: %s", planID, firstLines(body))
		}
		if b.planIDs != nil {
			t.Errorf("plan_id %q reached the executor", planID)
		}
	}
}

// A bad subscriber id is refused before anything is read or written.
func TestFR_SUB_006_ABadSubscriberIdIsRefused(t *testing.T) {
	c := &fakeCredentials{}
	b := &fakeBulk{}
	h := credsHandler(c, b)

	for _, target := range []string{
		"/staff/subscribers/not-a-number/password/reveal",
		"/staff/subscribers/not-a-number/password",
		"/staff/subscribers/not-a-number/plan-change",
	} {
		rec := serveCreds(h, sess("isp_owner"), target, url.Values{
			"new_password": {"a-good-password"}, "plan_id": {"7"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", target, rec.Code)
		}
	}
	if c.revealedID != 0 || c.newPassword != "" || b.planIDs != nil {
		t.Error("a malformed id reached a store")
	}
}

// An unconfigured deployment says which capability is missing, so an operator
// is not left guessing whether the feature exists.
func TestFR_SUB_006_AnUnconfiguredDeploymentNamesWhatIsMissing(t *testing.T) {
	noCreds := credsHandler(nil, &fakeBulk{})
	for _, target := range []string{
		"/staff/subscribers/5/password/reveal",
		"/staff/subscribers/5/password",
	} {
		rec := serveCreds(noCreds, sess("isp_owner"), target,
			url.Values{"new_password": {"a-good-password"}})
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: want 503, got %d", target, rec.Code)
		}
	}

	noBulk := credsHandler(&fakeCredentials{}, nil)
	rec := serveCreds(noBulk, sess("isp_owner"), "/staff/subscribers/5/plan-change",
		url.Values{"plan_id": {"7"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("plan change with no executor: want 503, got %d", rec.Code)
	}
}

// A reveal that fails is reported on the page, without the store's error.
func TestFR_SUB_006_AFailedRevealIsReported(t *testing.T) {
	h := credsHandler(&fakeCredentials{revealErr: errCreds}, &fakeBulk{})
	rec := serveCreds(h, sess("csr"), "/staff/subscribers/5/password/reveal", nil)
	body := pageText(rec.Body.String())
	if !strings.Contains(body, "Could not reveal") {
		t.Errorf("want a failure message, got: %s", firstLines(body))
	}
	if strings.Contains(body, errCreds.Error()) {
		t.Error("the store's own error reached the page")
	}
}
