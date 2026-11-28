//go:build integration

// The owner's speed override over the API — the console's speed-override
// form has tests of its own; this endpoint had none.
//
// The defect found writing them: any rate_limit_string was accepted. The
// override is stored and used on every Access-Accept until cleared, and a
// rate the vendor builders cannot render does not refuse the login —
// radiusd sends the Accept with no bandwidth attribute, so the subscriber
// is unshaped. "fast", "100Mbps/100Mbps" or a burst string meant to slow
// somebody down gave them the full line instead.
package api_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
)

func speedMux(t *testing.T, ctl *stubSessionCtl, tasks *stubTaskEnqueuer) *http.ServeMux {
	t.Helper()
	deps := api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{})}
	if ctl != nil {
		deps.SessionCtl = ctl
	}
	if tasks != nil {
		deps.Tasks = tasks
	}
	h := api.NewHandler(deps)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func TestASpeedOverrideIsStoredAndPushed(t *testing.T) {
	ctl, tasks := &stubSessionCtl{}, &stubTaskEnqueuer{}
	before := time.Now()
	rec := httptestJSON(t, speedMux(t, ctl, tasks), http.MethodPost, "/api/v1/subscribers/5/speed-override",
		"isp_owner", `{"rate_limit_string":" 2M/1M ","duration_minutes":90}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(ctl.overrides) != 1 {
		t.Fatalf("stored %d overrides", len(ctl.overrides))
	}
	o := ctl.overrides[0]
	if o.SubscriberID != 5 || o.RateLimit != "2M/1M" {
		t.Errorf("stored %+v; the rate should arrive trimmed", o)
	}
	if o.ExpiresAt == nil || o.ExpiresAt.Sub(before) < 89*time.Minute || o.ExpiresAt.Sub(before) > 91*time.Minute {
		t.Errorf("expiry %v is not ninety minutes out", o.ExpiresAt)
	}
	if len(tasks.snapshot()) != 1 {
		t.Errorf("enqueued %d CoA tasks, want 1", len(tasks.snapshot()))
	}

	// No duration is "until cleared".
	ctl = &stubSessionCtl{}
	httptestJSON(t, speedMux(t, ctl, &stubTaskEnqueuer{}), http.MethodPost, "/api/v1/subscribers/5/speed-override",
		"isp_owner", `{"rate_limit_string":"512k/256k"}`)
	if len(ctl.overrides) != 1 || ctl.overrides[0].ExpiresAt != nil {
		t.Errorf("an override with no duration got an expiry: %+v", ctl.overrides)
	}
}

// A rate the vendor builders cannot render is refused, and nothing is stored
// or pushed.
func TestASpeedOverrideTheRouterCannotTakeIsRefused(t *testing.T) {
	for _, bad := range []string{
		"fast", "fast/slow", "100Mbps/100Mbps", "10M/10M 20M/20M", "10M/", "/10M", "10M", "99999G/1M",
	} {
		t.Run(bad, func(t *testing.T) {
			ctl, tasks := &stubSessionCtl{}, &stubTaskEnqueuer{}
			rec := httptestJSON(t, speedMux(t, ctl, tasks), http.MethodPost, "/api/v1/subscribers/5/speed-override",
				"isp_owner", `{"rate_limit_string":"`+bad+`"}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%q: got %d %s", bad, rec.Code, rec.Body.String())
			}
			if len(ctl.overrides) != 0 || len(tasks.snapshot()) != 0 {
				t.Errorf("%q reached the store (%d) or the queue (%d)", bad, len(ctl.overrides), len(tasks.snapshot()))
			}
		})
	}
}

func TestSpeedOverrideRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		path, body string
		ctl        *stubSessionCtl
		tasks      *stubTaskEnqueuer
		code       int
	}{
		"bad id":           {"/api/v1/subscribers/x/speed-override", `{}`, &stubSessionCtl{}, &stubTaskEnqueuer{}, http.StatusBadRequest},
		"malformed":        {"/api/v1/subscribers/5/speed-override", `{`, &stubSessionCtl{}, &stubTaskEnqueuer{}, http.StatusBadRequest},
		"no rate":          {"/api/v1/subscribers/5/speed-override", `{"rate_limit_string":"  "}`, &stubSessionCtl{}, &stubTaskEnqueuer{}, http.StatusUnprocessableEntity},
		"negative minutes": {"/api/v1/subscribers/5/speed-override", `{"rate_limit_string":"1M/1M","duration_minutes":-5}`, &stubSessionCtl{}, &stubTaskEnqueuer{}, http.StatusUnprocessableEntity},
		"store failed":     {"/api/v1/subscribers/5/speed-override", `{"rate_limit_string":"1M/1M"}`, &stubSessionCtl{setErr: errors.New("db down")}, &stubTaskEnqueuer{}, http.StatusInternalServerError},
		"unconfigured":     {"/api/v1/subscribers/5/speed-override", `{"rate_limit_string":"1M/1M"}`, nil, nil, http.StatusServiceUnavailable},
		"clear, bad id":    {"/api/v1/subscribers/x/speed-override/clear", ``, &stubSessionCtl{}, &stubTaskEnqueuer{}, http.StatusBadRequest},
		"clear, failed":    {"/api/v1/subscribers/5/speed-override/clear", ``, &stubSessionCtl{setErr: errors.New("db down")}, &stubTaskEnqueuer{}, http.StatusInternalServerError},
		"clear, unwired":   {"/api/v1/subscribers/5/speed-override/clear", ``, nil, nil, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptestJSON(t, speedMux(t, tc.ctl, tc.tasks), http.MethodPost, tc.path, "isp_owner", tc.body)
			if rec.Code != tc.code {
				t.Errorf("got %d %s, want %d", rec.Code, rec.Body.String(), tc.code)
			}
			if strings.Contains(rec.Body.String(), "db down") {
				t.Error("the store's error reached the client")
			}
			if tc.tasks != nil && len(tc.tasks.snapshot()) != 0 {
				t.Error("a refused or failed override sent a CoA")
			}
		})
	}
}

// Clearing is stored and pushed too; without the CoA the router keeps the
// old rate until the next login.
func TestClearingASpeedOverrideIsPushed(t *testing.T) {
	ctl, tasks := &stubSessionCtl{}, &stubTaskEnqueuer{}
	rec := httptestJSON(t, speedMux(t, ctl, tasks), http.MethodPost, "/api/v1/subscribers/5/speed-override/clear", "isp_owner", ``)
	if rec.Code != http.StatusAccepted || len(ctl.cleared) != 1 || ctl.cleared[0] != 5 || len(tasks.snapshot()) != 1 {
		t.Errorf("clear: %d cleared=%v tasks=%d", rec.Code, ctl.cleared, len(tasks.snapshot()))
	}

	// A queue that is down does not undo the stored change: it takes effect
	// on the next login regardless.
	ctl, tasks = &stubSessionCtl{}, &stubTaskEnqueuer{err: errors.New("queue down")}
	rec = httptestJSON(t, speedMux(t, ctl, tasks), http.MethodPost, "/api/v1/subscribers/5/speed-override",
		"isp_owner", `{"rate_limit_string":"1M/1M"}`)
	if rec.Code != http.StatusAccepted || len(ctl.overrides) != 1 {
		t.Errorf("queue down: %d stored=%d", rec.Code, len(ctl.overrides))
	}
}

// The FUP override's remaining failure paths.
func TestFUPOverrideFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		ctl   *stubSessionCtl
		tasks *stubTaskEnqueuer
		body  string
		code  int
	}{
		"malformed":       {&stubSessionCtl{subscriberID: 7}, &stubTaskEnqueuer{}, `{`, http.StatusBadRequest},
		"no session":      {&stubSessionCtl{resolveErr: errors.New("no rows")}, &stubTaskEnqueuer{}, `{"action":"apply"}`, http.StatusNotFound},
		"state not saved": {&stubSessionCtl{subscriberID: 7, setErr: errors.New("db down")}, &stubTaskEnqueuer{}, `{"action":"apply"}`, http.StatusInternalServerError},
		"queue down":      {&stubSessionCtl{subscriberID: 7}, &stubTaskEnqueuer{err: errors.New("queue down")}, `{"action":"remove"}`, http.StatusInternalServerError},
		"unconfigured":    {nil, nil, `{"action":"apply"}`, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptestJSON(t, speedMux(t, tc.ctl, tc.tasks), http.MethodPost, "/api/v1/sessions/sess-7/fup-override", "noc_engineer", tc.body)
			if rec.Code != tc.code {
				t.Errorf("got %d %s, want %d", rec.Code, rec.Body.String(), tc.code)
			}
		})
	}
}
