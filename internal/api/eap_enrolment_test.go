//go:build integration

// EAP enrolment — FR-AAA-006.
//
// internal/api/eap_enrolment.go was at 0%: 59 statements, and one of them is
// the only thing standing between this endpoint and a credential backdoor.
//
// Enrolling derives an NT hash from a password. If the endpoint accepted any
// password, an operator could enrol one of their own choosing and then
// authenticate as that subscriber over EAP while the subscriber's own PAP
// password kept working — a second, divergent credential with nothing to show
// it had been created. The supplied password is therefore verified against the
// stored bcrypt hash before anything is written, and that check was untested.
package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"golang.org/x/crypto/bcrypt"
)

type fakeEAP struct {
	enrolled  bool
	setErr    error
	statusErr error

	// gotHash is what was written. nil means un-enrolled, which is the
	// distinction the remediation path depends on.
	gotHash  []byte
	gotID    int
	setCalls int
}

func (f *fakeEAP) SetNTHash(_ context.Context, id int, hash []byte) error {
	f.setCalls++
	f.gotID, f.gotHash = id, hash
	return f.setErr
}

func (f *fakeEAP) IsEAPEnrolled(_ context.Context, _ int) (bool, error) {
	return f.enrolled, f.statusErr
}

// fakeCreds returns a bcrypt hash of whatever password it was built with, so
// a test can present the right one and the wrong one.
type fakeCreds struct {
	hash  string
	err   error
	asked string
}

func newFakeCreds(t *testing.T, password string) *fakeCreds {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash the fixture password: %v", err)
	}
	return &fakeCreds{hash: string(h)}
}

func (f *fakeCreds) GetPasswordHash(_ context.Context, username string) (string, error) {
	f.asked = username
	return f.hash, f.err
}

func (f *fakeCreds) GetSubscriberPasswordEnc(context.Context, int) (string, error) {
	return "", nil
}
func (f *fakeCreds) SetSubscriberPasswordEnc(context.Context, int, string) error { return nil }
func (f *fakeCreds) SetSubscriberCredential(context.Context, int, string, string) error {
	return nil
}

// eapSubs is the subscriber lookup EnrolEAP needs. Only GetSubscriberByID is
// used; the rest satisfy the interface.
type eapSubs struct {
	rec *api.SubscriberRecord
	err error
}

func (s *eapSubs) GetSubscriberByID(context.Context, int) (*api.SubscriberRecord, error) {
	return s.rec, s.err
}

func (s *eapSubs) GetSubscriberByUsername(context.Context, string) (*api.SubscriberRecord, error) {
	return s.rec, s.err
}

func (s *eapSubs) CreateSubscriber(_ context.Context, sub api.SubscriberRecord, _ string) (*api.SubscriberRecord, error) {
	return &sub, s.err
}

func (s *eapSubs) UpdateSubscriber(_ context.Context, _ int, _ *int, _ *string,
	_ *time.Time) (*api.SubscriberRecord, error) {
	return s.rec, s.err
}

func eapCall(h *api.Handler, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r = r.WithContext(middleware.WithSubject(r.Context(), "meena"))
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) >= 4 {
		r.SetPathValue("id", parts[3])
	}

	w := httptest.NewRecorder()
	switch method {
	case http.MethodPost:
		h.EnrolEAP(w, r)
	case http.MethodDelete:
		h.UnenrolEAP(w, r)
	default:
		h.GetEAPStatus(w, r)
	}
	return w
}

// The password presented for enrolment must be the subscriber's own. This is
// the test that stops this endpoint becoming a way to mint a second
// credential.
func TestFR_AAA_006_EnrolmentRefusesAPasswordThatIsNotTheSubscribersOwn(t *testing.T) {
	eap := &fakeEAP{}
	creds := newFakeCreds(t, "the-subscribers-real-password")
	h := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: eap,
		Credentials:  creds,
		DB:           &eapSubs{rec: &api.SubscriberRecord{ID: 12, Username: "twelve@isp"}},
	})

	rec := eapCall(h, http.MethodPost, "/api/v1/subscribers/12/eap",
		`{"password":"an-operators-choice"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "does not match") {
		t.Errorf("the refusal should say the password is wrong, got %s", rec.Body.String())
	}
	if eap.setCalls != 0 {
		t.Error("an unverified password was enrolled; this endpoint would be a way to " +
			"create a second credential that authenticates as the subscriber")
	}
	if creds.asked != "twelve@isp" {
		t.Errorf("the hash was fetched for %q, want the subscriber's own username", creds.asked)
	}
}

// The subscriber's real password enrols, and what is stored is a hash rather
// than the password.
func TestFR_AAA_006_TheSubscribersOwnPasswordEnrols(t *testing.T) {
	const password = "the-subscribers-real-password"
	eap := &fakeEAP{}
	h := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: eap,
		Credentials:  newFakeCreds(t, password),
		DB:           &eapSubs{rec: &api.SubscriberRecord{ID: 12, Username: "twelve@isp"}},
	})

	rec := eapCall(h, http.MethodPost, "/api/v1/subscribers/12/eap",
		`{"password":"`+password+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if eap.gotID != 12 {
		t.Errorf("enrolled subscriber %d", eap.gotID)
	}
	if len(eap.gotHash) == 0 {
		t.Fatal("no NT hash was written")
	}
	if string(eap.gotHash) == password {
		t.Error("the password itself was stored instead of its NT hash")
	}
	if strings.Contains(string(eap.gotHash), password) {
		t.Error("the stored hash contains the password")
	}
	// The response must not carry credential material.
	if strings.Contains(rec.Body.String(), password) {
		t.Errorf("the password was echoed back: %s", rec.Body.String())
	}
}

// A password is required: the NT hash cannot be derived from the stored
// bcrypt hash, and the message says so rather than failing obscurely.
func TestFR_AAA_006_EnrolmentWithoutAPasswordIsRefusedWithTheReason(t *testing.T) {
	eap := &fakeEAP{}
	h := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: eap,
		Credentials:  newFakeCreds(t, "x"),
		DB:           &eapSubs{rec: &api.SubscriberRecord{ID: 12, Username: "twelve@isp"}},
	})

	for _, body := range []string{`{}`, `{"password":""}`} {
		rec := eapCall(h, http.MethodPost, "/api/v1/subscribers/12/eap", body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: want 422, got %d", body, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "bcrypt") {
			t.Errorf("%s: the message should explain why a password is needed, got %s",
				body, rec.Body.String())
		}
	}
	if eap.setCalls != 0 {
		t.Error("an empty password reached the store")
	}

	if rec := eapCall(h, http.MethodPost, "/api/v1/subscribers/12/eap", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: want 400, got %d", rec.Code)
	}
}

// Un-enrolment clears the hash and needs no password. It is the remediation
// step for a lost device, and requiring the subscriber's password to perform
// it would make it unusable at exactly the moment it is needed.
func TestFR_AAA_006_UnenrolmentClearsTheHashAndNeedsNoPassword(t *testing.T) {
	eap := &fakeEAP{}
	// No credentials and no subscriber store wired at all: un-enrolment must
	// not depend on either.
	h := api.NewHandler(api.HandlerDeps{EAPEnrolment: eap})

	rec := eapCall(h, http.MethodDelete, "/api/v1/subscribers/12/eap", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if eap.setCalls != 1 {
		t.Fatalf("the store was called %d times", eap.setCalls)
	}
	if eap.gotHash != nil {
		t.Errorf("un-enrolment wrote %v; it must clear the hash", eap.gotHash)
	}

	var out struct {
		Enrolled bool `json:"eap_enrolled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v — %s", err, rec.Body.String())
	}
	if out.Enrolled {
		t.Error("un-enrolment reported the subscriber as still enrolled")
	}
}

// The status endpoint reports whether a hash exists and never the hash. An
// endpoint that returned credential material would defeat the point of
// keeping the enrolled population small.
func TestFR_AAA_006_StatusReportsEnrolmentAndNeverTheHash(t *testing.T) {
	for _, enrolled := range []bool{true, false} {
		h := api.NewHandler(api.HandlerDeps{EAPEnrolment: &fakeEAP{enrolled: enrolled}})
		rec := eapCall(h, http.MethodGet, "/api/v1/subscribers/12/eap", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
		}

		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out["eap_enrolled"] != enrolled {
			t.Errorf("eap_enrolled = %v, want %v", out["eap_enrolled"], enrolled)
		}
		for k := range out {
			if k != "subscriber_id" && k != "eap_enrolled" {
				t.Errorf("the status response carries an extra field %q; this endpoint "+
					"should report enrolment and nothing else", k)
			}
		}
	}
}

// A subscriber who does not exist cannot be enrolled, and the two failure
// modes are distinguishable.
func TestFR_AAA_006_AnUnknownSubscriberIsNotEnrolled(t *testing.T) {
	eap := &fakeEAP{}
	missing := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: eap,
		Credentials:  newFakeCreds(t, "x"),
		DB:           &eapSubs{rec: nil},
	})
	if rec := eapCall(missing, http.MethodPost, "/api/v1/subscribers/404/eap",
		`{"password":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}

	broken := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: eap,
		Credentials:  newFakeCreds(t, "x"),
		DB:           &eapSubs{err: errors.New("store down")},
	})
	rec := eapCall(broken, http.MethodPost, "/api/v1/subscribers/12/eap", `{"password":"x"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500 when the lookup fails, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "store down") {
		t.Errorf("the store's own error reached the caller: %s", rec.Body.String())
	}
	if eap.setCalls != 0 {
		t.Error("a subscriber that could not be read was enrolled anyway")
	}
}

// With no credential verification wired, enrolment is refused rather than
// proceeding unverified. This is the configuration in which the backdoor
// would otherwise exist.
func TestFR_AAA_006_WithoutCredentialVerificationEnrolmentIsRefused(t *testing.T) {
	eap := &fakeEAP{}
	h := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: eap,
		DB:           &eapSubs{rec: &api.SubscriberRecord{ID: 12, Username: "twelve@isp"}},
		// Credentials deliberately absent.
	})

	rec := eapCall(h, http.MethodPost, "/api/v1/subscribers/12/eap", `{"password":"anything"}`)
	if rec.Code == http.StatusOK {
		t.Fatal("enrolment succeeded with no way to verify the password")
	}
	if eap.setCalls != 0 {
		t.Error("an unverifiable password was enrolled")
	}
}

// A failing write is reported, not reported as enrolled.
func TestFR_AAA_006_AFailedWriteIsNotReportedAsEnrolled(t *testing.T) {
	const password = "the-subscribers-real-password"
	h := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: &fakeEAP{setErr: errors.New("store down")},
		Credentials:  newFakeCreds(t, password),
		DB:           &eapSubs{rec: &api.SubscriberRecord{ID: 12, Username: "twelve@isp"}},
	})

	rec := eapCall(h, http.MethodPost, "/api/v1/subscribers/12/eap",
		`{"password":"`+password+`"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("want 500, got %d — %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "eap_enrolled\":true") {
		t.Error("a failed write was reported as enrolled")
	}

	unenrol := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: &fakeEAP{setErr: errors.New("store down")},
	})
	if rec := eapCall(unenrol, http.MethodDelete, "/api/v1/subscribers/12/eap", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("un-enrolment: want 500, got %d", rec.Code)
	}
}

// An unconfigured deployment reports itself on all three, and a malformed id
// is refused first.
func TestFR_AAA_006_UnconfiguredAndMalformedAreBothRefused(t *testing.T) {
	bare := api.NewHandler(api.HandlerDeps{})
	for name, tc := range map[string]struct{ method, body string }{
		"enrol":   {http.MethodPost, `{"password":"x"}`},
		"unenrol": {http.MethodDelete, ""},
		"status":  {http.MethodGet, ""},
	} {
		t.Run(name, func(t *testing.T) {
			rec := eapCall(bare, tc.method, "/api/v1/subscribers/12/eap", tc.body)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}

	eap := &fakeEAP{}
	configured := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: eap,
		Credentials:  newFakeCreds(t, "x"),
		DB:           &eapSubs{rec: &api.SubscriberRecord{ID: 12}},
	})
	for name, method := range map[string]string{
		"enrol": http.MethodPost, "unenrol": http.MethodDelete, "status": http.MethodGet,
	} {
		t.Run("bad id/"+name, func(t *testing.T) {
			rec := eapCall(configured, method, "/api/v1/subscribers/not-a-number/eap",
				`{"password":"x"}`)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d", rec.Code)
			}
		})
	}
	if eap.setCalls != 0 {
		t.Error("a malformed id reached the store")
	}
}

// Both state changes are audited. An EAP credential appearing with no trail of
// who created it is the thing this endpoint's own doc comment warns about.
func TestFR_AAA_006_EnrolmentAndUnenrolmentAreAudited(t *testing.T) {
	sink := &recordingAuditSink{}
	middleware.SetAuditSink(sink)
	t.Cleanup(func() { middleware.SetAuditSink(nil) })

	const password = "the-subscribers-real-password"
	h := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: &fakeEAP{},
		Credentials:  newFakeCreds(t, password),
		DB:           &eapSubs{rec: &api.SubscriberRecord{ID: 12, Username: "twelve@isp"}},
	})

	eapCall(h, http.MethodPost, "/api/v1/subscribers/12/eap", `{"password":"`+password+`"}`)
	eapCall(h, http.MethodDelete, "/api/v1/subscribers/12/eap", "")

	seen := map[string]bool{}
	for _, e := range sink.entries() {
		seen[e.Action] = true
		if e.Target != "12" {
			t.Errorf("%s names target %q, want the subscriber id", e.Action, e.Target)
		}
		for _, v := range e.Detail {
			if s, ok := v.(string); ok && strings.Contains(s, password) {
				t.Errorf("the password reached the audit trail in %q", e.Action)
			}
		}
	}
	for _, want := range []string{"subscriber.eap_enrol", "subscriber.eap_unenrol"} {
		if !seen[want] {
			t.Errorf("%q was not audited; recorded: %v", want, seen)
		}
	}
}

// A status read that fails is not reported as "not enrolled", which would
// read as a definite answer.
func TestFR_AAA_006_AFailedStatusReadIsNotAnAnswer(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		EAPEnrolment: &fakeEAP{statusErr: errors.New("store down")},
	})
	rec := eapCall(h, http.MethodGet, "/api/v1/subscribers/12/eap", "")
	if rec.Code == http.StatusOK {
		t.Errorf("a failed read answered 200: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "store down") {
		t.Errorf("the store's own error reached the caller: %s", rec.Body.String())
	}
}
