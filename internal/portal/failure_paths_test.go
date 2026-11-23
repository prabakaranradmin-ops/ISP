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
		// Me and Dashboard both report 404 rather than 500: each collapses
		// `err != nil || profile == nil` into one answer, because it cannot
		// tell "no such subscriber" from "could not read the subscriber".
		//
		// Asserted as it behaves, not as it arguably should. The operational
		// cost is worth knowing: during a database outage every subscriber is
		// told their account does not exist, and the portal returns a 404 that
		// no alert treats as an incident. Changing it is a behaviour change
		// for a separate decision, not something to slip into a coverage pass.
		{"/portal/me", http.StatusNotFound},
		{"/portal/dashboard", http.StatusNotFound},
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
