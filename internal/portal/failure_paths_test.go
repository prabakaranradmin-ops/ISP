//go:build integration

package portal_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// What the portal does when the store is broken, and when a token is valid
// but names nobody.
//
// Every handler here opens the same two ways — an unusable subscriber id, and
// a store that returns an error — and neither had a test. The package sat at
// 64.9% with the uncovered portion being almost entirely these branches: the
// ones that only run when something has already gone wrong, which is when a
// subscriber is least able to tolerate a stack trace or a blank page.

// brokenStore fails every read. It satisfies the same four interfaces
// NewHandler takes, so the handler cannot tell it apart from a real store
// that is simply having a bad day.
type brokenStore struct{ err error }

func (b *brokenStore) GetSubscriberByUsername(context.Context, string) (*portal.SubscriberAuth, error) {
	return nil, b.err
}
func (b *brokenStore) GetSubscriberByID(context.Context, int) (*portal.SubscriberProfile, error) {
	return nil, b.err
}
func (b *brokenStore) GetActiveSession(context.Context, int) (*portal.ActiveSession, error) {
	return nil, b.err
}
func (b *brokenStore) ListSessionHistory(context.Context, int, int) ([]portal.SessionHistoryEntry, error) {
	return nil, b.err
}
func (b *brokenStore) ListNotifications(context.Context, int, int) ([]portal.NotificationEntry, error) {
	return nil, b.err
}
func (b *brokenStore) ListTickets(context.Context, int) ([]portal.TicketEntry, error) {
	return nil, b.err
}
func (b *brokenStore) CreateTicket(context.Context, portal.TicketCreateRequest) (*portal.TicketEntry, error) {
	return nil, b.err
}
func (b *brokenStore) RateTicket(context.Context, int, int, tickets.Rating) error { return b.err }

func brokenPortal(t *testing.T) *http.ServeMux {
	t.Helper()
	s := &brokenStore{err: errors.New("database is unreachable")}
	h := portal.NewHandler(s, s, s, s, nil, itPortalSecret)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

// tokenFor mints a subscriber token directly, so a test can present one the
// middleware accepts but whose subscriber id is unusable. A login cannot
// produce that, which is exactly why these branches were unreachable.
func tokenFor(t *testing.T, subscriberID int) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             "subscriber",
		SubscriberID:     subscriberID,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itPortalSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

func call(t *testing.T, mux *http.ServeMux, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// A token carrying no subscriber id is refused rather than treated as
// subscriber zero. The difference matters: subscriber 0 matches no row, so
// the alternative is a confident-looking empty dashboard.
func TestPortal_ATokenNamingNoSubscriberIsRefused(t *testing.T) {
	mux := brokenPortal(t)
	token := tokenFor(t, 0)

	for _, path := range []string{
		"/portal/me", "/portal/dashboard", "/portal/notifications", "/portal/tickets",
	} {
		t.Run(path, func(t *testing.T) {
			rec := call(t, mux, http.MethodGet, path, token)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("want 401 for a token naming no subscriber, got %d — %s",
					rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "ERR_UNAUTHORIZED") {
				t.Errorf("the refusal should name the reason, got %s", rec.Body.String())
			}
		})
	}
}

// A broken store is a 500 with a JSON body, not a panic and not HTML. A
// mobile client parses the body either way, which is the reason the whole
// package answers JSON on every path.
func TestPortal_AnUnreachableStoreIsReportedAsJSON(t *testing.T) {
	mux := brokenPortal(t)
	token := tokenFor(t, 42)

	for _, tc := range []struct {
		path string
		want int
	}{
		// Me and Dashboard used to report 404 here: each collapsed
		// `err != nil || profile == nil` into one answer, so during a
		// database outage every subscriber was told their account did not
		// exist, with a status no alert treats as an incident. This test
		// asserted that, with a note that it was wrong; it is now a 500, as
		// the browser portal and the console answer the same failure.
		{"/portal/me", http.StatusInternalServerError},
		{"/portal/dashboard", http.StatusInternalServerError},
		{"/portal/notifications", http.StatusInternalServerError},
		{"/portal/tickets", http.StatusInternalServerError},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := call(t, mux, http.MethodGet, tc.path, token)
			if rec.Code != tc.want {
				t.Errorf("want %d when the store is unreachable, got %d — %s",
					tc.want, rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content type %q: a client that can only parse JSON must be able to "+
					"read the failure too", ct)
			}
			// The store's own error must not reach the subscriber.
			if strings.Contains(rec.Body.String(), "database is unreachable") {
				t.Errorf("the internal error leaked to the subscriber: %s", rec.Body.String())
			}
		})
	}
}

// sessionlessStore reads the subscriber fine and fails only the live session,
// the case where the dashboard can still answer.
type sessionlessStore struct{ brokenStore }

func (sessionlessStore) GetSubscriberByID(_ context.Context, id int) (*portal.SubscriberProfile, error) {
	return &portal.SubscriberProfile{ID: id, Username: "ravi", PlanName: "TN_Super_100M", Status: "active"}, nil
}

// A live session that could not be read is reported as unavailable, not left
// out: a missing active_session is how a client renders "offline", and a
// customer told their line is down because a read failed calls support about
// a line that may be fine.
func TestPortal_ASessionReadThatFailedIsNotOffline(t *testing.T) {
	s := &sessionlessStore{brokenStore{err: errors.New("redis: connection refused")}}
	h := portal.NewHandler(s, s, s, s, nil, itPortalSecret)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := call(t, mux, http.MethodGet, "/portal/dashboard", tokenFor(t, 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("a failed session read should not fail the dashboard: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"session_unavailable":true`) {
		t.Errorf("the dashboard does not say the session is unavailable: %s", body)
	}
	if strings.Contains(body, "active_session") || strings.Contains(body, "redis") {
		t.Errorf("want no session and no store error: %s", body)
	}
}

// A sign-in against a broken store fails as a sign-in failure, not a 500 that
// tells an attacker the database is down.
func TestPortal_LoginAgainstABrokenStoreDoesNotLeakWhy(t *testing.T) {
	mux := brokenPortal(t)
	req := httptest.NewRequest(http.MethodPost, "/portal/login",
		strings.NewReader(`{"username":"anyone@isp","password":"whatever"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatal("a broken store must not produce a successful login")
	}
	if strings.Contains(rec.Body.String(), "database is unreachable") {
		t.Errorf("the store's error reached the sign-in page: %s", rec.Body.String())
	}
}

// Missing or malformed credentials are refused before the store is consulted.
func TestPortal_MalformedLoginIsRefusedBeforeTheStore(t *testing.T) {
	s := &brokenStore{err: errors.New("should never be called")}
	h := portal.NewHandler(s, s, s, s, nil, itPortalSecret)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	for _, body := range []string{`{`, `{}`, `{"username":""}`, `{"password":"x"}`} {
		req := httptest.NewRequest(http.MethodPost, "/portal/login", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Errorf("%s: a malformed login must not succeed", body)
		}
	}
}
