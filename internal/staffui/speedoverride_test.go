package staffui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// Per-subscriber speed override — FR-NET-009.
//
// speedoverride.go was at 0%: 54 statements that change what one customer's
// router is told to deliver. Two things make it worth more than its size:
//
//   - It is owner-only, and the check is in the handler rather than in the
//     template that hides the form. A POST does not have to come from the
//     page, and this is the one console action that silently changes what a
//     customer is paying for.
//   - Setting the row is only half the job. Until a CoA goes out the router
//     keeps enforcing the old rate, so the override appears applied on the
//     screen and has no effect on the wire. The enqueue is asserted on both
//     the apply and the clear paths.

var errSpeedStore = errors.New("store: speed override write failed")

type fakeSpeedOverride struct {
	setErr   error
	clearErr error

	setFor     int
	setRate    string
	setExpiry  *time.Time
	clearedFor int
}

func (f *fakeSpeedOverride) SetSpeedOverride(_ context.Context, id int, rate string, expiresAt *time.Time) error {
	f.setFor, f.setRate, f.setExpiry = id, rate, expiresAt
	return f.setErr
}

func (f *fakeSpeedOverride) ClearSpeedOverride(_ context.Context, id int) error {
	f.clearedFor = id
	return f.clearErr
}

// recordingQueue captures the tasks a handler enqueued, so the CoA can be
// inspected rather than merely counted.
type recordingQueue struct {
	err   error
	tasks []*jobqueue.Task
}

func (q *recordingQueue) Enqueue(t *jobqueue.Task, _ ...jobqueue.Option) (*jobqueue.TaskInfo, error) {
	q.tasks = append(q.tasks, t)
	if q.err != nil {
		return nil, q.err
	}
	return &jobqueue.TaskInfo{}, nil
}

func serveSpeed(h *Handler, s Session, target string, form url.Values) *httptest.ResponseRecorder {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) >= 3 {
		r.SetPathValue("id", parts[2])
	}

	w := httptest.NewRecorder()
	if strings.HasSuffix(r.URL.Path, "/clear") {
		h.ClearSpeedOverride(w, r)
	} else {
		h.ApplySpeedOverride(w, r)
	}
	return w
}

func speedHandler(so SpeedOverrideController, q TaskEnqueuer) *Handler {
	h := &Handler{
		revenue: &fakeRevenue{subs: []revenue.SubscriberRow{{ID: 5, Username: "five@isp"}}},
		subscribers: &fakeSubscribers{rec: &api.SubscriberRecord{
			ID: 5, Username: "five@isp", Status: "active", WalletBalance: "0.00",
		}},
	}
	if so != nil {
		h.speedOverride = so
	}
	if q != nil {
		h.tasks = q
	}
	return h
}

// coaFor returns the subscriber id in the one CoA task the handler enqueued.
func coaFor(t *testing.T, q *recordingQueue) int {
	t.Helper()
	if len(q.tasks) != 1 {
		t.Fatalf("want exactly one task enqueued, got %d", len(q.tasks))
	}
	task := q.tasks[0]
	if task.Type() != fup.TaskTypeCoA {
		t.Fatalf("enqueued a %q task, want %q", task.Type(), fup.TaskTypeCoA)
	}
	var payload fup.CoAPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		t.Fatalf("the CoA payload does not decode: %v", err)
	}
	return payload.SubscriberID
}

// Only the owner may change a subscriber's speed. Checked in the handler, not
// by the template that hides the form.
func TestFR_NET_009_OnlyTheOwnerChangesASubscribersSpeed(t *testing.T) {
	for _, role := range []string{"billing_admin", "csr", "technician", "noc_engineer"} {
		so := &fakeSpeedOverride{}
		q := &recordingQueue{}
		h := speedHandler(so, q)
		s := Session{Username: role + "_user", Role: role}

		for _, target := range []string{
			"/staff/subscribers/5/speed-override",
			"/staff/subscribers/5/speed-override/clear",
		} {
			rec := serveSpeed(h, s, target, url.Values{"rate_limit_string": {"10M/10M"}})
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s POST %s: want 403, got %d", role, target, rec.Code)
			}
		}
		if so.setFor != 0 || so.clearedFor != 0 {
			t.Errorf("%s reached the store", role)
		}
		if len(q.tasks) != 0 {
			t.Errorf("%s caused %d CoA task(s)", role, len(q.tasks))
		}
	}
}

// Applying an override writes the rate and sends the CoA. Without the CoA the
// router keeps enforcing the old rate and the screen says otherwise.
func TestFR_NET_009_ApplyingAnOverrideAlsoSendsTheCoA(t *testing.T) {
	so := &fakeSpeedOverride{}
	q := &recordingQueue{}

	rec := serveSpeed(speedHandler(so, q), owner(), "/staff/subscribers/5/speed-override",
		url.Values{"rate_limit_string": {"  200M/200M  "}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect, got %d — %s", rec.Code, firstLines(rec.Body.String()))
	}
	if so.setFor != 5 {
		t.Errorf("the store was told subscriber %d", so.setFor)
	}
	if so.setRate != "200M/200M" {
		t.Errorf("rate reached the store as %q; it should be trimmed", so.setRate)
	}
	if so.setExpiry != nil {
		t.Errorf("a blank duration became an expiry of %v; blank means until cleared",
			so.setExpiry)
	}
	if id := coaFor(t, q); id != 5 {
		t.Errorf("the CoA names subscriber %d", id)
	}
	if loc := rec.Header().Get("Location"); loc != "/staff/subscribers/5" {
		t.Errorf("redirected to %q", loc)
	}
}

// A duration in minutes becomes an expiry; blank means until cleared.
func TestFR_NET_009_ADurationInMinutesBecomesAnExpiry(t *testing.T) {
	so := &fakeSpeedOverride{}
	serveSpeed(speedHandler(so, &recordingQueue{}), owner(),
		"/staff/subscribers/5/speed-override",
		url.Values{"rate_limit_string": {"200M/200M"}, "duration_minutes": {"90"}})

	if so.setExpiry == nil {
		t.Fatal("a duration did not become an expiry")
	}
	if d := time.Until(*so.setExpiry); d > 91*time.Minute || d < 89*time.Minute {
		t.Errorf("the override lasts %v, want about 90 minutes", d)
	}
}

// A malformed rate or duration is refused, and nothing is written or sent —
// a half-applied override is worse than none, because the screen would show
// a change the router never received.
func TestFR_NET_009_MalformedInputIsRefusedWithoutWritingOrSending(t *testing.T) {
	for name, tc := range map[string]struct {
		form url.Values
		want string
	}{
		"no rate": {url.Values{"rate_limit_string": {""}}, "MikroTik rate-limit form"},
		"rate with no slash": {url.Values{"rate_limit_string": {"200M"}},
			"MikroTik rate-limit form"},
		"duration not a number": {url.Values{
			"rate_limit_string": {"200M/200M"}, "duration_minutes": {"an hour"},
		}, "positive number of minutes"},
		"duration zero": {url.Values{
			"rate_limit_string": {"200M/200M"}, "duration_minutes": {"0"},
		}, "positive number of minutes"},
		"duration negative": {url.Values{
			"rate_limit_string": {"200M/200M"}, "duration_minutes": {"-30"},
		}, "positive number of minutes"},
	} {
		t.Run(name, func(t *testing.T) {
			so := &fakeSpeedOverride{}
			q := &recordingQueue{}
			rec := serveSpeed(speedHandler(so, q), owner(),
				"/staff/subscribers/5/speed-override", tc.form)

			if !strings.Contains(pageText(rec.Body.String()), tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(rec.Body.String()))
			}
			if so.setFor != 0 {
				t.Error("a malformed override reached the store")
			}
			if len(q.tasks) != 0 {
				t.Error("a malformed override sent a CoA")
			}
		})
	}
}

// Clearing an override writes the clear and sends the CoA too. Without it the
// router keeps throttling a customer who is no longer overridden.
func TestFR_NET_009_ClearingAnOverrideAlsoSendsTheCoA(t *testing.T) {
	so := &fakeSpeedOverride{}
	q := &recordingQueue{}

	rec := serveSpeed(speedHandler(so, q), owner(),
		"/staff/subscribers/5/speed-override/clear", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want a redirect, got %d — %s", rec.Code, firstLines(rec.Body.String()))
	}
	if so.clearedFor != 5 {
		t.Errorf("the store was told to clear subscriber %d", so.clearedFor)
	}
	if id := coaFor(t, q); id != 5 {
		t.Errorf("the CoA names subscriber %d", id)
	}
}

// A failing write reports it and sends no CoA — telling a router to change to
// a rate that was never stored would leave the two disagreeing.
func TestFR_NET_009_AFailedWriteSendsNoCoA(t *testing.T) {
	for name, tc := range map[string]struct {
		so     *fakeSpeedOverride
		target string
		want   string
	}{
		"apply": {&fakeSpeedOverride{setErr: errSpeedStore},
			"/staff/subscribers/5/speed-override", "Could not apply"},
		"clear": {&fakeSpeedOverride{clearErr: errSpeedStore},
			"/staff/subscribers/5/speed-override/clear", "Could not clear"},
	} {
		t.Run(name, func(t *testing.T) {
			q := &recordingQueue{}
			rec := serveSpeed(speedHandler(tc.so, q), owner(), tc.target,
				url.Values{"rate_limit_string": {"200M/200M"}})

			body := pageText(rec.Body.String())
			if !strings.Contains(body, tc.want) {
				t.Errorf("want %q, got: %s", tc.want, firstLines(body))
			}
			if strings.Contains(body, errSpeedStore.Error()) {
				t.Error("the store's own error reached the page")
			}
			if len(q.tasks) != 0 {
				t.Error("a CoA was sent for a write that failed")
			}
		})
	}
}

// A queue failure does not undo the override or fail the request: the row is
// written and the operator is told nothing, which is the existing design.
// Asserted so a change to it is deliberate rather than accidental.
func TestFR_NET_009_AQueueFailureStillLeavesTheOverrideApplied(t *testing.T) {
	so := &fakeSpeedOverride{}
	q := &recordingQueue{err: errors.New("jobqueue: unreachable")}

	rec := serveSpeed(speedHandler(so, q), owner(), "/staff/subscribers/5/speed-override",
		url.Values{"rate_limit_string": {"200M/200M"}})
	if rec.Code != http.StatusSeeOther {
		t.Errorf("want a redirect, got %d", rec.Code)
	}
	if so.setFor != 5 {
		t.Error("the override was not written")
	}
	if len(q.tasks) != 1 {
		t.Errorf("the handler did not attempt the CoA: %d tasks", len(q.tasks))
	}
}

// An unconfigured deployment, and a malformed subscriber id, are both refused
// before anything is written.
func TestFR_NET_009_UnconfiguredAndMalformedAreRefused(t *testing.T) {
	for name, h := range map[string]*Handler{
		"no store": speedHandler(nil, &recordingQueue{}),
		"no queue": speedHandler(&fakeSpeedOverride{}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			for _, target := range []string{
				"/staff/subscribers/5/speed-override",
				"/staff/subscribers/5/speed-override/clear",
			} {
				rec := serveSpeed(h, owner(), target,
					url.Values{"rate_limit_string": {"200M/200M"}})
				if rec.Code != http.StatusServiceUnavailable {
					t.Errorf("POST %s: want 503, got %d", target, rec.Code)
				}
			}
		})
	}

	so := &fakeSpeedOverride{}
	q := &recordingQueue{}
	h := speedHandler(so, q)
	for _, target := range []string{
		"/staff/subscribers/not-a-number/speed-override",
		"/staff/subscribers/not-a-number/speed-override/clear",
	} {
		rec := serveSpeed(h, owner(), target, url.Values{"rate_limit_string": {"200M/200M"}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s: want 400, got %d", target, rec.Code)
		}
	}
	if so.setFor != 0 || so.clearedFor != 0 || len(q.tasks) != 0 {
		t.Error("a malformed id reached the store or the queue")
	}
}

// The CoA carries no NAS address. The task handler resolves the live session
// at execution time, and a snapshot taken here would be stale by then — a
// disconnect sent to the router the subscriber used to be on.
func TestFR_NET_009_TheCoACarriesNoStaleNASAddress(t *testing.T) {
	q := &recordingQueue{}
	serveSpeed(speedHandler(&fakeSpeedOverride{}, q), owner(),
		"/staff/subscribers/5/speed-override",
		url.Values{"rate_limit_string": {"200M/200M"}})

	if len(q.tasks) != 1 {
		t.Fatalf("want one task, got %d", len(q.tasks))
	}
	var payload fup.CoAPayload
	if err := json.Unmarshal(q.tasks[0].Payload(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.NasIP != "" {
		t.Errorf("the CoA carries NAS %q; it must be resolved fresh at execution time, "+
			"or a reconnect between here and there sends the change to the wrong router",
			payload.NasIP)
	}
}
