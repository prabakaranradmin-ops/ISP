package portalui_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/portalui"
	"github.com/maaransoft/isp-bss-oss/internal/tickets"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/bcrypt"
)

// ── Stubs ────────────────────────────────────────────────────────────────

type stubSubscribers struct{}

func (s *stubSubscribers) GetSubscriberByUsername(_ context.Context, username string) (*portal.SubscriberAuth, error) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("testpass"), 12)
	return &portal.SubscriberAuth{ID: 1, Username: username, PasswordHash: string(hash)}, nil
}

func (s *stubSubscribers) GetSubscriberByID(_ context.Context, id int) (*portal.SubscriberProfile, error) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	return &portal.SubscriberProfile{
		ID:            id,
		Username:      "testuser",
		MobileNumber:  "+919876543210",
		PlanName:      "100 Mbps Unlimited",
		PlanExpiry:    &exp,
		WalletBalance: decimal.NewFromFloat(799),
		Status:        "active",
	}, nil
}

type stubSessionsOnline struct{}

func (s *stubSessionsOnline) GetActiveSession(_ context.Context, _ int) (*portal.ActiveSession, error) {
	return &portal.ActiveSession{
		SessionID:    "sess-001",
		GBUsed:       decimal.NewFromFloat(100),
		GBIncluded:   decimal.NewFromFloat(3300),
		PctUsed:      3.03,
		FUPThrottled: false,
	}, nil
}

type stubSessionsOffline struct{}

func (s *stubSessionsOffline) GetActiveSession(_ context.Context, _ int) (*portal.ActiveSession, error) {
	return nil, nil
}

type stubSessionHistory struct {
	entries []portal.SessionHistoryEntry
}

func (s *stubSessionHistory) ListSessionHistory(_ context.Context, _, _ int) ([]portal.SessionHistoryEntry, error) {
	return s.entries, nil
}

type stubInvoices struct {
	summaries []api.InvoiceSummary
	details   map[int]*api.InvoiceDetail
}

func (s *stubInvoices) ListInvoices(_ context.Context, _ int) ([]api.InvoiceSummary, error) {
	return s.summaries, nil
}

func (s *stubInvoices) GetInvoiceDetail(_ context.Context, invoiceID int) (*api.InvoiceDetail, error) {
	if s.details == nil {
		return nil, nil
	}
	return s.details[invoiceID], nil
}

type stubPDFGen struct {
	err error
}

func (s *stubPDFGen) GeneratePDF(_ context.Context, _ billing.InvoiceData) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []byte("%PDF-1.4 fake invoice pdf"), nil
}

type stubRazorpay struct {
	orderID     string
	paymentLink string
	err         error
}

func (s *stubRazorpay) CreateOrder(_ context.Context, _ int, _ decimal.Decimal) (string, string, error) {
	if s.err != nil {
		return "", "", s.err
	}
	orderID, link := s.orderID, s.paymentLink
	if orderID == "" {
		orderID = "plink_test123"
	}
	if link == "" {
		link = "https://rzp.io/i/test123"
	}
	return orderID, link, nil
}

type stubTickets struct {
	entries   []portal.TicketEntry
	created   []portal.TicketCreateRequest
	createErr error
	listErr   error
	// FR-SUP-004.
	ratedTicketID int
	ratedBy       int
	rating        tickets.Rating
	rateErr       error
}

func (s *stubTickets) ListTickets(_ context.Context, _ int) ([]portal.TicketEntry, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.entries, nil
}

func (s *stubTickets) CreateTicket(_ context.Context, req portal.TicketCreateRequest) (*portal.TicketEntry, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.created = append(s.created, req)
	entry := portal.TicketEntry{ID: len(s.created), Category: req.Category, Description: req.Description, Status: "open"}
	s.entries = append(s.entries, entry)
	return &entry, nil
}

type stubNotifications struct {
	entries []portal.NotificationEntry
	err     error
}

func (s *stubNotifications) ListNotifications(_ context.Context, _ int, _ int) ([]portal.NotificationEntry, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.entries, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────

// baseTestDeps gives every test a working default for every dependency;
// individual tests override just the fields their scenario cares about.
func baseTestDeps() portalui.Deps {
	return portalui.Deps{
		Subscribers:    &stubSubscribers{},
		Sessions:       &stubSessionsOnline{},
		SessionHistory: &stubSessionHistory{},
		Invoices:       &stubInvoices{},
		PDF:            &stubPDFGen{},
		Razorpay:       &stubRazorpay{},
		Tickets:        &stubTickets{},
		Notifications:  &stubNotifications{},
		JWTSecret:      "test-portal-secret",
	}
}

var csrfTokenRe = regexp.MustCompile(`name="csrf_token" value="([^"]*)"`)

func extractCSRFToken(t *testing.T, body string) string {
	t.Helper()
	m := csrfTokenRe.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("csrf_token not found in body: %s", body)
	}
	return m[1]
}

func getRenewPage(t *testing.T, mux *http.ServeMux, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/ui/renew", nil) //nolint:noctx
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/renew: want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func postRenew(t *testing.T, mux *http.ServeMux, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/ui/renew", strings.NewReader(form.Encode())) //nolint:noctx
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func newTestMuxWithDeps(t *testing.T, deps portalui.Deps) *http.ServeMux {
	t.Helper()
	h := portalui.NewHandler(deps)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

func newTestMux(t *testing.T, sessions portal.PortalSessionQuerier, history portal.PortalSessionHistoryQuerier) *http.ServeMux {
	t.Helper()
	deps := baseTestDeps()
	deps.Sessions = sessions
	deps.SessionHistory = history
	return newTestMuxWithDeps(t, deps)
}

// loginResult is what tests need from a login attempt. Deliberately not
// *http.Response: returning that out of this helper made golangci-lint's
// bodyclose check (correctly cautious, since it can't see the defer'd Close
// below across the function boundary) flag every call site.
type loginResult struct {
	Cookie   *http.Cookie
	Status   int
	Location string
}

//nolint:unparam // username is always "testuser" today; kept as a parameter for reuse by later-phase tests (e.g. a second subscriber for ownership checks)
func loginAndGetCookie(t *testing.T, mux *http.ServeMux, username, password string) loginResult {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest("POST", "/ui/login", strings.NewReader(form.Encode())) //nolint:noctx
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close() //nolint:errcheck

	result := loginResult{Status: res.StatusCode, Location: res.Header.Get("Location")}
	for _, c := range res.Cookies() {
		if c.Name == "portal_session" {
			result.Cookie = c
		}
	}
	return result
}

// ── Tests ────────────────────────────────────────────────────────────────

func TestLoginPage_RendersForm(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})

	req := httptest.NewRequest("GET", "/ui/login", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `action="/ui/login"`) {
		t.Fatalf("expected a login form in body, got: %s", rec.Body.String())
	}
}

func TestLogin_ValidCredentials_SetsCookieAndRedirects(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})

	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	if got.Status != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", got.Status)
	}
	if got.Location != "/ui/dashboard" {
		t.Fatalf("want redirect to /ui/dashboard, got %q", got.Location)
	}
	cookie := got.Cookie
	if cookie == nil {
		t.Fatal("expected a portal_session cookie to be set")
	}
	if !cookie.HttpOnly {
		t.Error("cookie must be HttpOnly")
	}
	if cookie.Path != "/ui" {
		t.Errorf("cookie Path = %q, want /ui", cookie.Path)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", cookie.SameSite)
	}
	if cookie.Value == "" {
		t.Error("expected a non-empty JWT in the cookie value")
	}
}

func TestLogin_InvalidPassword_RerendersFormWithError(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})

	form := url.Values{"username": {"testuser"}, "password": {"wrongpass"}}
	req := httptest.NewRequest("POST", "/ui/login", strings.NewReader(form.Encode())) //nolint:noctx
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Invalid username or password") {
		t.Fatalf("expected an inline error message, got: %s", rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "portal_session" {
			t.Fatal("must not set a session cookie on failed login")
		}
	}
}

func TestLogout_ClearsCookieAndRedirects(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})

	req := httptest.NewRequest("POST", "/ui/logout", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("want redirect to /ui/login, got %q", loc)
	}
	var cleared bool
	for _, c := range res.Cookies() {
		if c.Name == "portal_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("expected logout to clear the portal_session cookie")
	}
}

func TestDashboard_Unauthenticated_RedirectsToLogin(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})

	req := httptest.NewRequest("GET", "/ui/dashboard", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303 redirect to login, got %d — %s", res.StatusCode, rec.Body.String())
	}
	if loc := res.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("want redirect to /ui/login, got %q", loc)
	}
}

func TestFR_SUB_002_Dashboard_RendersPlanExpiryBalanceAndSession(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})
	got := loginAndGetCookie(t, mux, "testuser", "testpass")
	if got.Cookie == nil {
		t.Fatal("login did not return a session cookie")
	}

	req := httptest.NewRequest("GET", "/ui/dashboard", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"799.00", "100 Mbps Unlimited", "3300.00 GB"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected dashboard body to contain %q, got: %s", want, body)
		}
	}
}

func TestDashboard_OfflineSubscriber_ShowsEmptyState(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOffline{}, &stubSessionHistory{})
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/dashboard", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "you appear to be offline") {
		t.Errorf("expected the offline empty state, got: %s", rec.Body.String())
	}
}

func TestDashboardSessionFragment_ReturnsFragmentOnly(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/dashboard/session", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "100.00 GB") {
		t.Errorf("expected fragment to contain usage figures, got: %s", body)
	}
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("fragment response must not include the full page layout")
	}
}

func TestUsage_Unauthenticated_RedirectsToLogin(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})

	req := httptest.NewRequest("GET", "/ui/usage", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303 redirect to login, got %d — %s", res.StatusCode, rec.Body.String())
	}
	if loc := res.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("want redirect to /ui/login, got %q", loc)
	}
}

func TestUsage_Authenticated_ListsSessions(t *testing.T) {
	closedStart := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	closedStop := time.Date(2026, 1, 10, 11, 30, 0, 0, time.UTC)
	activeStart := time.Date(2026, 1, 12, 8, 0, 0, 0, time.UTC)

	history := &stubSessionHistory{entries: []portal.SessionHistoryEntry{
		{
			SessionID: "hist-active", NASIP: "10.0.0.1",
			StartTime: activeStart, StopTime: nil,
			GBUsed: decimal.NewFromFloat(1.5),
		},
		{
			SessionID: "hist-closed", NASIP: "10.0.0.1",
			StartTime: closedStart, StopTime: &closedStop,
			GBUsed: decimal.NewFromFloat(4.25), TerminateCause: "user-request",
		},
	}}
	mux := newTestMux(t, &stubSessionsOnline{}, history)
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/usage", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"1.50 GB", "4.25 GB", "user-request", "12 Jan 2026", "10 Jan 2026"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected usage page body to contain %q, got: %s", want, body)
		}
	}
	if !strings.Contains(body, `class="badge badge-active"`) {
		t.Error("expected the still-open session to render with the active badge")
	}
}

func TestUsage_NoHistory_ShowsEmptyState(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{entries: nil})
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/usage", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No session history yet") {
		t.Errorf("expected the empty state, got: %s", rec.Body.String())
	}
}

func TestInvoices_Unauthenticated_RedirectsToLogin(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())

	req := httptest.NewRequest("GET", "/ui/invoices", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303 redirect to login, got %d — %s", res.StatusCode, rec.Body.String())
	}
	if loc := res.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("want redirect to /ui/login, got %q", loc)
	}
}

func TestInvoices_Authenticated_ListsInvoices(t *testing.T) {
	deps := baseTestDeps()
	deps.Invoices = &stubInvoices{summaries: []api.InvoiceSummary{
		{ID: 7, SubscriberID: 1, TotalAmount: "942.82", CreatedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)},
	}}
	mux := newTestMuxWithDeps(t, deps)
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/invoices", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"INV-000007", "942.82", `href="/ui/invoices/7/pdf"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected invoices page body to contain %q, got: %s", want, body)
		}
	}
}

func TestInvoices_NoInvoices_ShowsEmptyState(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/invoices", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No invoices yet") {
		t.Errorf("expected the empty state, got: %s", rec.Body.String())
	}
}

func TestInvoicePDF_OwnInvoice_ReturnsPDF(t *testing.T) {
	deps := baseTestDeps()
	deps.Invoices = &stubInvoices{details: map[int]*api.InvoiceDetail{
		7: {InvoiceSummary: api.InvoiceSummary{ID: 7, SubscriberID: 1, TotalAmount: "942.82"}},
	}}
	mux := newTestMuxWithDeps(t, deps)
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/invoices/7/pdf", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q, want application/pdf", ct)
	}
	if !strings.HasPrefix(rec.Body.String(), "%PDF") {
		t.Errorf("expected PDF bytes in the body, got: %q", rec.Body.String())
	}
}

// TestInvoicePDF_OtherSubscribersInvoice_Returns403 is the ownership check
// this phase exists to add: the admin PDF route (GetInvoicePDF in
// internal/api) has no such check because it is staff-trusted, but this
// route is reachable with any subscriber's own credentials, so it must
// refuse an invoice that does not belong to the caller.
func TestInvoicePDF_OtherSubscribersInvoice_Returns403(t *testing.T) {
	deps := baseTestDeps()
	deps.Invoices = &stubInvoices{details: map[int]*api.InvoiceDetail{
		// Belongs to subscriber 2; the logged-in test user is subscriber 1.
		99: {InvoiceSummary: api.InvoiceSummary{ID: 99, SubscriberID: 2, TotalAmount: "500.00"}},
	}}
	mux := newTestMuxWithDeps(t, deps)
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/invoices/99/pdf", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for another subscriber's invoice, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestInvoicePDF_NonexistentInvoice_Returns404(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	got := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/invoices/404/pdf", nil) //nolint:noctx
	req.AddCookie(got.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestInvoicePDF_Unauthenticated_Returns401(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())

	req := httptest.NewRequest("GET", "/ui/invoices/7/pdf", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Deliberately not redirectToLoginOn401-wrapped: this route serves a
	// file download, not an HTML page, so swapping in a login page would be
	// wrong here in a way it would not be for a full-page GET.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestRenewPage_Unauthenticated_RedirectsToLogin(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())

	req := httptest.NewRequest("GET", "/ui/renew", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303 redirect to login, got %d — %s", res.StatusCode, rec.Body.String())
	}
	if loc := res.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("want redirect to /ui/login, got %q", loc)
	}
}

func TestRenewPage_Authenticated_RendersFormWithCSRFToken(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	body := getRenewPage(t, mux, login.Cookie)

	if extractCSRFToken(t, body) == "" {
		t.Error("expected a non-empty csrf_token in the rendered form")
	}
}

func TestRenew_ValidRequest_ReturnsPaymentLink(t *testing.T) {
	deps := baseTestDeps()
	deps.Razorpay = &stubRazorpay{orderID: "plink_abc", paymentLink: "https://rzp.io/i/abc"}
	mux := newTestMuxWithDeps(t, deps)
	login := loginAndGetCookie(t, mux, "testuser", "testpass")
	token := extractCSRFToken(t, getRenewPage(t, mux, login.Cookie))

	rec := postRenew(t, mux, login.Cookie, url.Values{"amount": {"799.00"}, "csrf_token": {token}})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "https://rzp.io/i/abc") {
		t.Errorf("expected the payment link in the response fragment, got: %s", body)
	}
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("fragment response must not include the full page layout")
	}
}

// TestRenew_MissingCSRFToken_Returns403 and TestRenew_InvalidCSRFToken_Returns403
// are the CSRF protection this phase exists to add: POST /ui/renew is
// reachable via a cookie the browser attaches automatically (unlike the
// JSON API's Bearer-only POST /portal/renew), so it needs its own defense
// beyond SameSite=Lax.
func TestRenew_MissingCSRFToken_Returns403(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	rec := postRenew(t, mux, login.Cookie, url.Values{"amount": {"799.00"}})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a missing csrf_token, got %d", rec.Code)
	}
}

func TestRenew_InvalidCSRFToken_Returns403(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	rec := postRenew(t, mux, login.Cookie, url.Values{"amount": {"799.00"}, "csrf_token": {"not-the-right-token"}})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a wrong csrf_token, got %d", rec.Code)
	}
}

func TestRenew_InvalidAmount_ReturnsErrorFragment(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	login := loginAndGetCookie(t, mux, "testuser", "testpass")
	token := extractCSRFToken(t, getRenewPage(t, mux, login.Cookie))

	rec := postRenew(t, mux, login.Cookie, url.Values{"amount": {"not-a-number"}, "csrf_token": {token}})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (fragment response), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Enter a valid amount") {
		t.Errorf("expected an inline validation error, got: %s", rec.Body.String())
	}
}

func TestRenew_GatewayError_ReturnsErrorFragment(t *testing.T) {
	deps := baseTestDeps()
	deps.Razorpay = &stubRazorpay{err: errors.New("gateway down")}
	mux := newTestMuxWithDeps(t, deps)
	login := loginAndGetCookie(t, mux, "testuser", "testpass")
	token := extractCSRFToken(t, getRenewPage(t, mux, login.Cookie))

	rec := postRenew(t, mux, login.Cookie, url.Values{"amount": {"799.00"}, "csrf_token": {token}})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (fragment response), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Payment gateway error") {
		t.Errorf("expected a gateway error message, got: %s", rec.Body.String())
	}
}

func TestRenew_NoGatewayConfigured_ReturnsErrorFragment(t *testing.T) {
	deps := baseTestDeps()
	deps.Razorpay = nil
	mux := newTestMuxWithDeps(t, deps)
	login := loginAndGetCookie(t, mux, "testuser", "testpass")
	token := extractCSRFToken(t, getRenewPage(t, mux, login.Cookie))

	rec := postRenew(t, mux, login.Cookie, url.Values{"amount": {"799.00"}, "csrf_token": {token}})

	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Errorf("expected a gateway-not-configured message, got: %s", rec.Body.String())
	}
}

func getTicketsPage(t *testing.T, mux *http.ServeMux, cookie *http.Cookie) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/ui/tickets", nil) //nolint:noctx
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/tickets: want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func postCreateTicket(t *testing.T, mux *http.ServeMux, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/ui/tickets", strings.NewReader(form.Encode())) //nolint:noctx
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestTickets_Unauthenticated_RedirectsToLogin(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())

	req := httptest.NewRequest("GET", "/ui/tickets", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303 redirect to login, got %d — %s", res.StatusCode, rec.Body.String())
	}
	if loc := res.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("want redirect to /ui/login, got %q", loc)
	}
}

func TestFR_SUB_004_Tickets_ListsSubscribersOwnTickets(t *testing.T) {
	deps := baseTestDeps()
	deps.Tickets = &stubTickets{entries: []portal.TicketEntry{
		{ID: 1, Category: "connectivity", Description: "No internet since morning", Status: "open", CreatedAt: time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)},
	}}
	mux := newTestMuxWithDeps(t, deps)
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	body := getTicketsPage(t, mux, login.Cookie)

	for _, want := range []string{"connectivity", "No internet since morning", `class="badge badge-open"`, "10 Jan 2026"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected tickets page body to contain %q, got: %s", want, body)
		}
	}
}

func TestTickets_NoTickets_ShowsEmptyState(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	body := getTicketsPage(t, mux, login.Cookie)

	if !strings.Contains(body, "No tickets filed yet") {
		t.Errorf("expected the empty state, got: %s", body)
	}
}

func TestFR_SUB_004_CreateTicket_AddsAndReturnsFragment(t *testing.T) {
	stub := &stubTickets{}
	deps := baseTestDeps()
	deps.Tickets = stub
	mux := newTestMuxWithDeps(t, deps)
	login := loginAndGetCookie(t, mux, "testuser", "testpass")
	token := extractCSRFToken(t, getTicketsPage(t, mux, login.Cookie))

	rec := postCreateTicket(t, mux, login.Cookie, url.Values{
		"category": {"billing"}, "description": {"Overcharged this month"}, "csrf_token": {token},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Overcharged this month") {
		t.Errorf("expected the new ticket in the response fragment, got: %s", body)
	}
	if strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("fragment response must not include the full page layout")
	}
	if len(stub.created) != 1 {
		t.Fatalf("want exactly 1 ticket created, got %d", len(stub.created))
	}
}

// TestCreateTicket_SubscriberIDAlwaysFromSession is the guarantee the phase
// plan called out explicitly: subscriber_id must come from the session
// context, never from client-controlled input, even if a request tries to
// smuggle one in — the handler never reads a subscriber_id form field at
// all, but this proves the *outcome* holds regardless of that
// implementation detail.
func TestCreateTicket_SubscriberIDAlwaysFromSession(t *testing.T) {
	stub := &stubTickets{}
	deps := baseTestDeps()
	deps.Tickets = stub
	mux := newTestMuxWithDeps(t, deps)
	login := loginAndGetCookie(t, mux, "testuser", "testpass")
	token := extractCSRFToken(t, getTicketsPage(t, mux, login.Cookie))

	rec := postCreateTicket(t, mux, login.Cookie, url.Values{
		"category": {"other"}, "description": {"trying to spoof"},
		"subscriber_id": {"999"}, // an attacker-supplied value that must be ignored
		"csrf_token":    {token},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(stub.created) != 1 {
		t.Fatalf("want exactly 1 ticket created, got %d", len(stub.created))
	}
	if stub.created[0].SubscriberID != 1 {
		t.Errorf("SubscriberID = %d, want 1 (from the session, not the spoofed form field)", stub.created[0].SubscriberID)
	}
}

func TestCreateTicket_MissingCSRFToken_Returns403(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	rec := postCreateTicket(t, mux, login.Cookie, url.Values{"category": {"other"}, "description": {"x"}})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a missing csrf_token, got %d", rec.Code)
	}
}

func TestCreateTicket_InvalidCSRFToken_Returns403(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	rec := postCreateTicket(t, mux, login.Cookie, url.Values{
		"category": {"other"}, "description": {"x"}, "csrf_token": {"wrong-token"},
	})

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 for a wrong csrf_token, got %d", rec.Code)
	}
}

func TestCreateTicket_MissingFields_ReturnsValidationError(t *testing.T) {
	stub := &stubTickets{}
	deps := baseTestDeps()
	deps.Tickets = stub
	mux := newTestMuxWithDeps(t, deps)
	login := loginAndGetCookie(t, mux, "testuser", "testpass")
	token := extractCSRFToken(t, getTicketsPage(t, mux, login.Cookie))

	rec := postCreateTicket(t, mux, login.Cookie, url.Values{"category": {""}, "description": {""}, "csrf_token": {token}})

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 (fragment response), got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Category and description are required") {
		t.Errorf("expected a validation error, got: %s", rec.Body.String())
	}
	if len(stub.created) != 0 {
		t.Error("no ticket should have been created")
	}
}

func TestNotifications_Unauthenticated_RedirectsToLogin(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())

	req := httptest.NewRequest("GET", "/ui/notifications", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("want 303 redirect to login, got %d — %s", res.StatusCode, rec.Body.String())
	}
	if loc := res.Header.Get("Location"); loc != "/ui/login" {
		t.Fatalf("want redirect to /ui/login, got %q", loc)
	}
}

func TestNotifications_Authenticated_ListsNotifications(t *testing.T) {
	deps := baseTestDeps()
	deps.Notifications = &stubNotifications{entries: []portal.NotificationEntry{
		{ID: 1, Channel: "whatsapp", TemplateName: "FUP Warning", DeliveryStatus: "delivered", SentAt: time.Date(2026, 1, 12, 14, 30, 0, 0, time.UTC)},
	}}
	mux := newTestMuxWithDeps(t, deps)
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/notifications", nil) //nolint:noctx
	req.AddCookie(login.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"whatsapp", "FUP Warning", `class="badge badge-delivered"`, "12 Jan 2026"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected notifications page body to contain %q, got: %s", want, body)
		}
	}
}

func TestNotifications_NoNotifications_ShowsEmptyState(t *testing.T) {
	mux := newTestMuxWithDeps(t, baseTestDeps())
	login := loginAndGetCookie(t, mux, "testuser", "testpass")

	req := httptest.NewRequest("GET", "/ui/notifications", nil) //nolint:noctx
	req.AddCookie(login.Cookie)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No notifications yet") {
		t.Errorf("expected the empty state, got: %s", rec.Body.String())
	}
}

func TestStaticAssets_Served(t *testing.T) {
	mux := newTestMux(t, &stubSessionsOnline{}, &stubSessionHistory{})

	req := httptest.NewRequest("GET", "/ui/static/portalui.css", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	// The route is public, and http.FileServer lists a directory it is
	// asked for. Harmless with three public files in it, and still the first
	// thing a scanner reports.
	req = httptest.NewRequest("GET", "/ui/static/", nil) //nolint:noctx
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("/ui/static/ lists its directory: %.200s", rec.Body.String())
	}
}

// RateTicket records the rating this fake was asked for, so a test can prove
// the subscriber id came from the session rather than the request — the one
// thing a handler test can get wrong and still pass — FR-SUP-004.
func (s *stubTickets) RateTicket(_ context.Context, ticketID, subscriberID int, r tickets.Rating) error {
	s.ratedTicketID = ticketID
	s.ratedBy = subscriberID
	s.rating = r
	return s.rateErr
}
