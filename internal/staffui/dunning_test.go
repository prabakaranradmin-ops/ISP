package staffui

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

type fakeDunning struct {
	err       error
	extended  int
	gotID     int
	gotUntil  time.Time
	gotReason string
	gotActor  string
	ended     int
	policy    billing.DunningPolicy
	saves     int
}

func (f *fakeDunning) ExtendGrace(ctx context.Context, id int, until time.Time, reason string) error {
	f.extended++
	f.gotID, f.gotUntil, f.gotReason, f.gotActor = id, until, reason, middleware.SubjectFromContext(ctx)
	return f.err
}

func (f *fakeDunning) EndGrace(ctx context.Context, id int) error {
	f.ended++
	f.gotID, f.gotActor = id, middleware.SubjectFromContext(ctx)
	return f.err
}

func (f *fakeDunning) CurrentDunningPolicy(context.Context) (billing.DunningPolicy, error) {
	return f.policy, nil
}

func (f *fakeDunning) SaveDunningPolicy(ctx context.Context, p billing.DunningPolicy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	f.saves++
	f.policy, f.gotActor = p, middleware.SubjectFromContext(ctx)
	return f.err
}

func serveForm(h *Handler, role, target string, form url.Values, fn func(*Handler, *httptest.ResponseRecorder, *http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(context.WithValue(r.Context(), sessionKey, Session{Username: role + "_user", Role: role}))
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) > 2 {
		r.SetPathValue("id", parts[2])
	}
	w := httptest.NewRecorder()
	fn(h, w, r)
	return w
}

func extendGrace(h *Handler, w *httptest.ResponseRecorder, r *http.Request) { h.ExtendGraceForm(w, r) }
func endGrace(h *Handler, w *httptest.ResponseRecorder, r *http.Request)    { h.EndGraceForm(w, r) }
func saveDunning(h *Handler, w *httptest.ResponseRecorder, r *http.Request) { h.SaveDunningForm(w, r) }

func TestFR_BIL_016_GraceForms(t *testing.T) {
	friday := time.Now().AddDate(0, 0, 4).Format("2006-01-02")

	t.Run("billing extends grace to the end of the chosen day, attributed", func(t *testing.T) {
		fd := &fakeDunning{}
		w := serveForm(&Handler{dunning: fd}, "billing_admin", "/staff/subscribers/5/grace",
			url.Values{"until": {friday}, "reason": {"salary on Friday"}}, extendGrace)
		if w.Code != 303 || fd.gotID != 5 || fd.gotReason != "salary on Friday" || fd.gotActor != "billing_admin_user" {
			t.Fatalf("got %d id=%d reason=%q actor=%q", w.Code, fd.gotID, fd.gotReason, fd.gotActor)
		}
		if fd.gotUntil.Format("2006-01-02") != friday || fd.gotUntil.Hour() != 23 {
			t.Errorf("until: %s", fd.gotUntil)
		}
		if loc := w.Header().Get("Location"); loc != "/staff/subscribers/5?grace=set" {
			t.Errorf("redirect: %q", loc)
		}
	})

	t.Run("a CSR can neither extend nor end", func(t *testing.T) {
		fd := &fakeDunning{}
		if code := serveForm(&Handler{dunning: fd}, "csr", "/staff/subscribers/5/grace", url.Values{"until": {friday}, "reason": {"x"}}, extendGrace).Code; code != 403 {
			t.Errorf("extend: %d", code)
		}
		if code := serveForm(&Handler{dunning: fd}, "csr", "/staff/subscribers/5/grace/end", nil, endGrace).Code; code != 403 {
			t.Errorf("end: %d", code)
		}
		if fd.extended+fd.ended != 0 {
			t.Error("a CSR changed grace")
		}
	})

	t.Run("no date never reaches the API", func(t *testing.T) {
		fd := &fakeDunning{}
		serveForm(&Handler{dunning: fd}, "isp_owner", "/staff/subscribers/5/grace", url.Values{"reason": {"x"}}, extendGrace)
		if fd.extended != 0 {
			t.Error("a missing date must be refused in the console")
		}
	})

	for name, err := range map[string]error{
		"refused by the API": fmt.Errorf("%w: give a reason; it is kept with the extension", billing.ErrGraceInvalid),
		"not past expiry":    billing.ErrGraceNotApplicable,
		"database went away": fmt.Errorf("boom"),
	} {
		t.Run("stays on the page when "+name, func(t *testing.T) {
			fd := &fakeDunning{err: err}
			w := serveForm(&Handler{dunning: fd}, "isp_owner", "/staff/subscribers/5/grace",
				url.Values{"until": {friday}, "reason": {"x"}}, extendGrace)
			if w.Code == 303 {
				t.Error("a failure must not redirect as if it worked")
			}
			if !strings.Contains(w.Body.String(), "Grace not extended") {
				t.Errorf("no explanation shown: %.300s", w.Body.String())
			}
		})
	}

	t.Run("ending redirects with a code", func(t *testing.T) {
		fd := &fakeDunning{}
		w := serveForm(&Handler{dunning: fd}, "isp_owner", "/staff/subscribers/5/grace/end", nil, endGrace)
		if fd.ended != 1 || w.Header().Get("Location") != "/staff/subscribers/5?grace=ended" {
			t.Errorf("ended=%d loc=%q", fd.ended, w.Header().Get("Location"))
		}
	})
}

func TestFR_BIL_016_GraceView(t *testing.T) {
	now := time.Now()
	past, future := now.AddDate(0, 0, -5), now.AddDate(0, 0, 3)
	if g := newGraceView(&future, "r", "meena", &past, "suspended", now); !g.Active || g.SetBy != "meena" {
		t.Errorf("an extension in force must show: %+v", g)
	}
	if g := newGraceView(&past, "r", "meena", &past, "suspended", now); g.Active || !g.PastExpiry {
		t.Errorf("a lapsed extension offers a new one: %+v", g)
	}
	if g := newGraceView(nil, "", "", &future, "active", now); g.PastExpiry {
		t.Error("someone paid up cannot be given grace")
	}
	if g := newGraceView(nil, "", "", &past, "terminated", now); g.PastExpiry {
		t.Error("a terminated subscriber cannot be given grace")
	}

	s := testSession()
	for name, g := range map[string]graceView{
		"active":  newGraceView(&future, "salary day", "meena", &past, "suspended", now),
		"offer":   newGraceView(nil, "", "", &past, "suspended", now),
		"not due": newGraceView(nil, "", "", &future, "active", now),
		"no plan": newGraceView(nil, "", "", nil, "pending_payment", now),
	} {
		t.Run("renders: "+name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			(&Handler{}).render(rec, "subscriber_detail", pageData{Session: s, Data: subscriberDetailData{
				Subscriber:  &api.SubscriberRecord{ID: 5, Username: "u", Status: "suspended"},
				ShowBilling: true, ShowGrace: true, Grace: g,
			}})
			body := rec.Body.String()
			if rec.Code != 200 || !strings.Contains(body, "Grace extension") {
				t.Fatalf("got %d without the card", rec.Code)
			}
			switch name {
			case "active":
				if !strings.Contains(body, "salary day") || !strings.Contains(body, "/grace/end") {
					t.Error("an active extension must show its reason and an end button")
				}
			case "offer":
				if !strings.Contains(body, `action="/staff/subscribers/5/grace"`) {
					t.Error("past expiry must offer the form")
				}
			default:
				if strings.Contains(body, `action="/staff/subscribers/5/grace"`) {
					t.Error("the form must not be offered before expiry")
				}
			}
		})
	}

	t.Run("an unknown grace code in the URL shows nothing", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/staff/subscribers/5?grace=You+have+been+hacked", nil)
		r = r.WithContext(context.WithValue(r.Context(), sessionKey, s))
		r.SetPathValue("id", "5")
		w := httptest.NewRecorder()
		(&Handler{}).SubscriberDetail(w, r)
		if strings.Contains(w.Body.String(), "hacked") {
			t.Error("text in the URL must never be shown")
		}
	})
}

func TestFR_BIL_017_DunningSettingsForm(t *testing.T) {
	valid := url.Values{"remind_first_days": {"10"}, "remind_second_days": {"5"}, "remind_last_days": {"2"},
		"grace_days": {"7"}, "soft_suspend_days": {"5"}}

	t.Run("billing saves a schedule, attributed", func(t *testing.T) {
		fd := &fakeDunning{policy: billing.DefaultDunningPolicy()}
		w := serveForm(&Handler{dunning: fd}, "billing_admin", "/staff/dunning", valid, saveDunning)
		if w.Code != 303 || w.Header().Get("Location") != "/staff/dunning?saved=1" {
			t.Fatalf("got %d to %q: %.300s", w.Code, w.Header().Get("Location"), w.Body.String())
		}
		if fd.policy.GraceDays != 7 || fd.policy.RemindFirstDays != 10 || fd.gotActor != "billing_admin_user" {
			t.Errorf("saved %+v by %q", fd.policy, fd.gotActor)
		}
	})

	t.Run("a schedule out of order is refused with the rule", func(t *testing.T) {
		fd := &fakeDunning{policy: billing.DefaultDunningPolicy()}
		bad := url.Values{"remind_first_days": {"2"}, "remind_second_days": {"5"}, "remind_last_days": {"1"},
			"grace_days": {"3"}, "soft_suspend_days": {"3"}}
		w := serveForm(&Handler{dunning: fd}, "isp_owner", "/staff/dunning", bad, saveDunning)
		if fd.saves != 0 || !strings.Contains(w.Body.String(), "count down") {
			t.Errorf("saves=%d body %.300s", fd.saves, w.Body.String())
		}
		// The operator's figures stay in the form so they can fix one.
		if !strings.Contains(w.Body.String(), `name="remind_second_days" type="number" min="0" max="30" required value="5"`) {
			t.Error("the submitted figures must be kept")
		}
	})

	t.Run("not a number never reaches the API", func(t *testing.T) {
		fd := &fakeDunning{}
		bad := url.Values{"remind_first_days": {"a week"}}
		serveForm(&Handler{dunning: fd}, "isp_owner", "/staff/dunning", bad, saveDunning)
		if fd.saves != 0 {
			t.Error("saved")
		}
	})

	t.Run("a CSR is kept out of the screen entirely", func(t *testing.T) {
		fd := &fakeDunning{}
		if code := serveForm(&Handler{dunning: fd}, "csr", "/staff/dunning", valid, saveDunning).Code; code != 403 || fd.saves != 0 {
			t.Errorf("got %d with %d saves", code, fd.saves)
		}
	})

	t.Run("the page spells the schedule out", func(t *testing.T) {
		rec := httptest.NewRecorder()
		p := billing.DunningPolicy{RemindFirstDays: 10, RemindSecondDays: 5, RemindLastDays: 1, GraceDays: 7, SoftSuspendDays: 5}
		(&Handler{}).render(rec, "dunning", pageData{Session: testSession(), Data: dunningData{Policy: p, Ladder: ladderFor(p), CanEdit: true}})
		body := rec.Body.String()
		for _, want := range []string{"10 days before expiry", "1 day before expiry", "7 days after expiry", "12 days after expiry"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
	})
}
