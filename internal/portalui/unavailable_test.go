package portalui_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/portal"
)

// What a subscriber is told when the portal could not read something —
// FR-POR-001 (dashboard: plan, balance, live usage).
//
// The dashboard discarded the session store's error, so a failed read told
// the customer "No active session — you appear to be offline", on the page
// and again on every 15-second poll. A customer told their line is down
// calls support about a line that may be fine. And a failed account lookup
// answered "subscriber not found" to somebody who had just signed in.

type stubSessionsFailing struct{}

func (stubSessionsFailing) GetActiveSession(context.Context, int) (*portal.ActiveSession, error) {
	return nil, errors.New("redis: connection refused")
}

type stubSubscribersFailing struct{ *stubSubscribers }

func (stubSubscribersFailing) GetSubscriberByID(context.Context, int) (*portal.SubscriberProfile, error) {
	return nil, errors.New("db: connection refused")
}

func signedInGet(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	got := loginAndGetCookie(t, mux, "testuser", "testpass")
	req := httptest.NewRequest("GET", path, nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestFR_POR_001_ASessionReadThatFailedIsNotOffline(t *testing.T) {
	mux := newTestMux(t, stubSessionsFailing{}, &stubSessionHistory{})
	for _, path := range []string{"/ui/dashboard", "/ui/dashboard/session"} {
		rec := signedInGet(t, mux, path)
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d — a failed usage read should not fail the page", path, rec.Code)
		}
		if strings.Contains(body, "you appear to be offline") {
			t.Errorf("%s told the customer they are offline because a read failed", path)
		}
		if !strings.Contains(body, "could not load your live usage") {
			t.Errorf("%s does not say the usage is unavailable: %.300s", path, body)
		}
		if strings.Contains(body, "redis") {
			t.Errorf("%s showed the store's error to a customer", path)
		}
	}
}

func TestFR_POR_001_AnAccountLookupThatFailedIsNotNotFound(t *testing.T) {
	deps := baseTestDeps()
	mux := newTestMuxWithDeps(t, deps)
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	deps.Subscribers = stubSubscribersFailing{&stubSubscribers{}}
	failing := newTestMuxWithDeps(t, deps)
	req := httptest.NewRequest("GET", "/ui/dashboard", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	failing.ServeHTTP(rec, req)

	if rec.Code == http.StatusNotFound || strings.Contains(rec.Body.String(), "not found") {
		t.Errorf("a failed lookup told a signed-in customer their account was not found: %d %s",
			rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("got %d, want 500", rec.Code)
	}
}
