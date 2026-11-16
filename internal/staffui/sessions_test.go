package staffui

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

type fakeDisconnector struct {
	err      error
	gotID    string
	gotActor string
}

func (f *fakeDisconnector) RequestDisconnect(ctx context.Context, sessionID string) (int, error) {
	f.gotID = sessionID
	f.gotActor = middleware.SubjectFromContext(ctx)
	return 1, f.err
}

func runSessions(h *Handler, role, method, target string, form url.Values) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{StaffID: 1, Username: role + "_user", Role: role}))
	w := httptest.NewRecorder()
	if method == "POST" {
		// Go 1.22 mux would fill this from the route pattern.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		r.SetPathValue("session_id", parts[2])
		h.DisconnectSessionForm(w, r)
	} else {
		h.Sessions(w, r)
	}
	return w
}

func TestParseSessionRange(t *testing.T) {
	now := time.Date(2026, 11, 16, 15, 30, 0, 0, time.Local)
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.Local) }

	cases := []struct {
		name           string
		from, to       string
		wantFrom, want time.Time
		wantMsg        bool
	}{
		{"blank means yesterday and today", "", "", day(2026, 11, 15), day(2026, 11, 17), false},
		{"one day is inclusive of that day", "2026-11-10", "2026-11-10", day(2026, 11, 10), day(2026, 11, 11), false},
		{"reversed is swapped, not refused", "2026-11-12", "2026-11-10", day(2026, 11, 10), day(2026, 11, 13), true},
		{"over 31 days is clipped from the start", "2026-01-01", "2026-11-16", day(2026, 11, 17).Add(-maxCompletedRange), day(2026, 11, 17), true},
		{"garbage falls back to defaults with a message", "yesterday", "", day(2026, 11, 15), day(2026, 11, 17), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, msg := parseSessionRange(tc.from, tc.to, now)
			if !from.Equal(tc.wantFrom) || !to.Equal(tc.want) {
				t.Errorf("want [%v, %v), got [%v, %v)", tc.wantFrom, tc.want, from, to)
			}
			if (msg != "") != tc.wantMsg {
				t.Errorf("message: want present=%v, got %q", tc.wantMsg, msg)
			}
			if to.Sub(from) > maxCompletedRange {
				t.Errorf("range %v exceeds the cap", to.Sub(from))
			}
		})
	}
}

func TestHumanBytesAndElapsed(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.00 KB", 301961216: "287.97 MB", 3758096384: "3.50 GB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for d, want := range map[time.Duration]string{
		10*time.Minute + 20*time.Second: "0:10:20",
		25*time.Hour + 61*time.Second:   "1d 1:01:01",
		-time.Minute:                    "0:00:00",
	} {
		if got := elapsed(start, start.Add(d)); got != want {
			t.Errorf("elapsed(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestSessionsScreen(t *testing.T) {
	started := time.Now().Add(-90 * time.Minute)
	live := []LiveSession{{SubscriberID: 1, Username: "test_user", SessionID: "s-1", AssignedIP: "100.64.0.7",
		NasIP: "10.10.0.1", BytesIn: 2048, BytesOut: 301961216, SpeedProfile: "100M/100M", FUPThrottled: true,
		StartedAt: started, UpdatedAt: time.Now()}}

	t.Run("every ISP-wide role can view; only NOC and owner see Disconnect", func(t *testing.T) {
		for role, wantButton := range map[string]bool{
			"isp_owner": true, "noc_engineer": true, "csr": false, "technician": false, "billing_admin": false,
		} {
			h := &Handler{console: &fakeConsole{live: live}}
			w := runSessions(h, role, "GET", "/staff/sessions", nil)
			if w.Code != 200 {
				t.Fatalf("%s: want 200, got %d", role, w.Code)
			}
			body := w.Body.String()
			if !strings.Contains(body, "100.64.0.7") || !strings.Contains(body, "287.97 MB") {
				t.Errorf("%s: live row not rendered", role)
			}
			if got := strings.Contains(body, ">Disconnect<"); got != wantButton {
				t.Errorf("%s: Disconnect button shown=%v, want %v", role, got, wantButton)
			}
		}
	})

	t.Run("franchise roles are refused", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{}}
		if code := runSessions(h, "lco", "GET", "/staff/sessions", nil).Code; code != 403 {
			t.Errorf("lco: want 403, got %d", code)
		}
	})

	t.Run("query and FUP filter reach the store", func(t *testing.T) {
		fc := &fakeConsole{}
		h := &Handler{console: fc}
		runSessions(h, "csr", "GET", "/staff/sessions?q=+100.64.0.7+&fup=1", nil)
		if fc.gotSess == nil || fc.gotSess.Query != "100.64.0.7" || !fc.gotSess.FUPOnly {
			t.Fatalf("want trimmed query and FUPOnly, got %+v", fc.gotSess)
		}
	})

	t.Run("completed tab sends a bounded range", func(t *testing.T) {
		fc := &fakeConsole{completed: []CompletedSession{{SubscriberID: 1, Username: "test_user", StartTime: started, StopTime: time.Now(), TerminateCause: "User-Request"}}}
		h := &Handler{console: fc}
		body := runSessions(h, "csr", "GET", "/staff/sessions?tab=completed&from=2026-01-01&to=2026-11-16", nil).Body.String()
		if fc.gotSess == nil || fc.gotSess.To.Sub(fc.gotSess.From) > maxCompletedRange {
			t.Fatalf("range must be capped before the query, got %+v", fc.gotSess)
		}
		if !strings.Contains(body, "at most 31 days") || !strings.Contains(body, "User-Request") {
			t.Error("the clipped range should be explained and the row shown")
		}
	})

	t.Run("a store failure is reported", func(t *testing.T) {
		h := &Handler{console: &fakeConsole{err: errors.New("boom")}}
		if body := runSessions(h, "csr", "GET", "/staff/sessions", nil).Body.String(); !strings.Contains(body, "Could not load sessions.") {
			t.Error("failure must be reported")
		}
	})
}

func TestDisconnectSessionForm(t *testing.T) {
	form := url.Values{"username": {"test_user"}}

	t.Run("CSR and technician are refused before anything is sent", func(t *testing.T) {
		for _, role := range []string{"csr", "technician", "billing_admin"} {
			fd := &fakeDisconnector{}
			h := &Handler{console: &fakeConsole{}, disconnector: fd}
			if code := runSessions(h, role, "POST", "/staff/sessions/s-1/disconnect", form).Code; code != 403 {
				t.Errorf("%s: want 403, got %d", role, code)
			}
			if fd.gotID != "" {
				t.Errorf("%s: a refused request must not reach the disconnector", role)
			}
		}
	})

	t.Run("NOC disconnect is sent, attributed, and confirmed", func(t *testing.T) {
		fd := &fakeDisconnector{}
		h := &Handler{console: &fakeConsole{}, disconnector: fd}
		w := runSessions(h, "noc_engineer", "POST", "/staff/sessions/s-1/disconnect", form)
		if w.Code != 200 || fd.gotID != "s-1" {
			t.Fatalf("want 200 and session s-1 sent, got %d %q", w.Code, fd.gotID)
		}
		if fd.gotActor != "noc_engineer_user" {
			t.Errorf("the audit entry must name the operator, got actor %q", fd.gotActor)
		}
		if !strings.Contains(w.Body.String(), "Disconnect sent for test_user") {
			t.Error("the operator should be told it was sent")
		}
	})

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"already ended": {fmt.Errorf("%w: gone", api.ErrSessionNotFound), "already ended"},
		"not wired":     {api.ErrSessionControlUnavailable, "not configured"},
		"queue down":    {errors.New("enqueue failed"), "Could not send the disconnect"},
	} {
		t.Run("error: "+name, func(t *testing.T) {
			h := &Handler{console: &fakeConsole{}, disconnector: &fakeDisconnector{err: tc.err}}
			body := runSessions(h, "isp_owner", "POST", "/staff/sessions/s-1/disconnect", form).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("want message containing %q", tc.want)
			}
			if strings.Contains(body, "Disconnect sent") {
				t.Error("a failure must not also claim success")
			}
		})
	}
}

func TestRenderSessions(t *testing.T) {
	h := &Handler{}
	now := time.Now()
	for name, d := range map[string]sessionsData{
		"live, empty":      {Tab: "live", Limit: 500},
		"live, with rows":  {Tab: "live", Limit: 500, CanDisconnect: true, Live: []LiveSession{{SubscriberID: 1, Username: "u", SessionID: "a/b", StartedAt: now, UpdatedAt: now}}},
		"completed, empty": {Tab: "completed", From: "2026-11-15", To: "2026-11-16", Limit: 500},
		"completed, a row": {Tab: "completed", Limit: 500, Completed: []CompletedSession{{SubscriberID: 1, Username: "u", StartTime: now.Add(-time.Hour), StopTime: now}}},
	} {
		t.Run(name, func(t *testing.T) {
			assertRendersOK(t, h, "sessions", pageData{Session: testSession(), Data: d})
		})
	}
}
