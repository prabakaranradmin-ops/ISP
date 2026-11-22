//go:build integration

// Announcement and push-token endpoint tests — FR-ANN-001..002,
// FR-NOTIF-013 | MDS §4.17.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// itPortalJWTSecret is the key the portal signs subscriber tokens with, and
// it must never equal itJWTSecret.
//
// Production always separates the two — config.Load derives
// PORTAL_JWT_SECRET as JWT_SECRET+"_portal" when it is unset — so a harness
// that uses one secret for both cannot tell a route that verifies the right
// key from one that verifies the wrong one. That is precisely how the
// subscriber routes shipped broken: they checked the staff key, every real
// subscriber got a 401, and these tests passed throughout because they minted
// their tokens with whatever the route happened to check.
const itPortalJWTSecret = "integration_portal_jwt_secret_32!"

// itSubscriberToken mints a subscriber-role token, the shape portal logins
// issue: signed with the PORTAL secret, as a real one is.
func itSubscriberToken(t *testing.T, subscriberID int) string {
	t.Helper()
	return itSubscriberTokenSignedWith(t, subscriberID, itPortalJWTSecret)
}

// itSubscriberTokenSignedWith mints the same token under a caller-chosen key,
// so a test can present a subscriber token signed with the *staff* secret and
// assert it is refused.
func itSubscriberTokenSignedWith(t *testing.T, subscriberID int, secret string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             "subscriber",
		SubscriberID:     subscriberID,
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign subscriber token: %v", err)
	}
	return tok
}

// ── Stubs ────────────────────────────────────────────────────────────────────

// stubAnnouncements holds the claim under a mutex so it reproduces the real
// store's atomic conditional UPDATE — a stub that let two sends both claim
// would make the double-broadcast test meaningless.
type stubAnnouncements struct {
	mu     sync.Mutex
	byID   map[int]*notifications.Announcement
	nextID int

	segment  []int
	finished []string

	// portalBanners is what the banner feed returns, and portalAskedFor
	// records the subscriber ids it was asked about — the feed must resolve
	// the subscriber from the token and nothing else.
	portalBanners  []notifications.Announcement
	portalAskedFor []int
}

func newStubAnnouncements() *stubAnnouncements {
	return &stubAnnouncements{byID: map[int]*notifications.Announcement{}, nextID: 1}
}

func (s *stubAnnouncements) seed(a notifications.Announcement) *notifications.Announcement {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.ID = s.nextID
	s.nextID++
	if a.Status == "" {
		a.Status = notifications.AnnouncementDraft
	}
	s.byID[a.ID] = &a
	return &a
}

func (s *stubAnnouncements) CreateAnnouncement(_ context.Context, a notifications.Announcement) (*notifications.Announcement, error) {
	return s.seed(a), nil
}

func (s *stubAnnouncements) GetAnnouncement(_ context.Context, id int) (*notifications.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	cp := *a
	return &cp, nil
}

func (s *stubAnnouncements) ListAnnouncements(_ context.Context, status *string) ([]notifications.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notifications.Announcement
	for _, a := range s.byID {
		if status != nil && a.Status != *status {
			continue
		}
		out = append(out, *a)
	}
	return out, nil
}

func (s *stubAnnouncements) ListPortalAnnouncements(_ context.Context, subscriberID int) ([]notifications.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.portalAskedFor = append(s.portalAskedFor, subscriberID)
	return s.portalBanners, nil
}

func (s *stubAnnouncements) ClaimAnnouncementForSending(_ context.Context, id int) (*notifications.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.byID[id]
	if !ok || a.Status != notifications.AnnouncementDraft {
		return nil, nil
	}
	a.Status = notifications.AnnouncementSending
	cp := *a
	return &cp, nil
}

func (s *stubAnnouncements) FinishAnnouncement(_ context.Context, id int, status string, count int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finished = append(s.finished, status)
	if a, ok := s.byID[id]; ok {
		a.Status = status
		a.RecipientCount = count
	}
	return nil
}

func (s *stubAnnouncements) ListSegmentSubscriberIDs(context.Context, int, *int, *int, *string, *int) ([]int, error) {
	return s.segment, nil
}

type stubPushTokens struct {
	mu         sync.Mutex
	registered map[int][]string
}

func newStubPushTokens() *stubPushTokens {
	return &stubPushTokens{registered: map[int][]string{}}
}

func (s *stubPushTokens) RegisterPushToken(_ context.Context, subscriberID int, token, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered[subscriberID] = append(s.registered[subscriberID], token)
	return nil
}

// ── Harness ──────────────────────────────────────────────────────────────────

type annHarness struct {
	mux    *http.ServeMux
	anns   *stubAnnouncements
	tasks  *stubTaskEnqueuer
	tokens *stubPushTokens
}

func newAnnHarness(t *testing.T) *annHarness {
	t.Helper()
	h := &annHarness{
		anns: newStubAnnouncements(), tasks: &stubTaskEnqueuer{}, tokens: newStubPushTokens(),
	}
	handler := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Announcements: h.anns, PushTokens: h.tokens, Tasks: h.tasks,
		PortalJWTSecret: itPortalJWTSecret,
	})
	h.mux = http.NewServeMux()
	handler.RegisterRoutes(h.mux, itJWTSecret)
	return h
}

func (h *annHarness) do(t *testing.T, method, path, body, role, subject string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, role, subject))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

// ── FR-ANN-001 ──────────────────────────────────────────────────────────────

func TestFR_ANN_001_CreateAnnouncementDefaultsToMarketing(t *testing.T) {
	h := newAnnHarness(t)

	body := `{"title":"Maintenance","body":"Brief outage tonight","channels":["sms","email"]}`
	rec := h.do(t, http.MethodPost, "/api/v1/announcements", body, "billing_admin", "ops1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	var a notifications.Announcement
	if err := json.Unmarshal(rec.Body.Bytes(), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Marketing by default is what makes dnd_opt_out meaningful: a broadcast
	// that defaulted to transactional would quietly override every opt-out.
	if a.Class != "marketing" {
		t.Errorf("class = %q, want marketing by default", a.Class)
	}
	if a.Status != notifications.AnnouncementDraft {
		t.Errorf("status = %q, want draft — creating must not broadcast", a.Status)
	}
	if len(h.tasks.snapshot()) != 0 {
		t.Error("creating an announcement must not enqueue anything")
	}
}

func TestFR_ANN_001_CreateAnnouncementValidates(t *testing.T) {
	h := newAnnHarness(t)

	cases := []struct{ name, body string }{
		{"missing title", `{"body":"x","channels":["sms"]}`},
		{"missing body", `{"title":"x","channels":["sms"]}`},
		{"unknown channel", `{"title":"x","body":"y","channels":["telepathy"]}`},
		{"portal is not a channel", `{"title":"x","body":"y","channels":["portal"]}`},
		{"no destination at all", `{"title":"x","body":"y","channels":[]}`},
		{"bad class", `{"title":"x","body":"y","channels":["sms"],"class":"urgent"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(t, http.MethodPost, "/api/v1/announcements", tc.body, "billing_admin", "ops1")
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestFR_ANN_001_PortalOnlyAnnouncementIsAllowed: a banner with no dispatched
// channel is a legitimate announcement.
func TestFR_ANN_001_PortalOnlyAnnouncementIsAllowed(t *testing.T) {
	h := newAnnHarness(t)

	body := `{"title":"Notice","body":"Read me","channels":[],"show_in_portal":true}`
	rec := h.do(t, http.MethodPost, "/api/v1/announcements", body, "billing_admin", "ops1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201 for a banner-only announcement, got %d — %s", rec.Code, rec.Body.String())
	}
}

// TestFR_ANN_001_SendFansOutOneTaskPerRecipientPerChannel is the core of the
// broadcast: N recipients × M channels tasks, each independently retryable.
func TestFR_ANN_001_SendFansOutOneTaskPerRecipientPerChannel(t *testing.T) {
	h := newAnnHarness(t)
	h.anns.seed(notifications.Announcement{
		Title: "Maintenance", Body: "Outage", Channels: []string{"sms", "email"},
		Class: "marketing", CreatedBy: "ops1",
	})
	h.anns.segment = []int{1, 2, 3}

	rec := h.do(t, http.MethodPost, "/api/v1/announcements/1/send", ``, "billing_admin", "ops1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d — %s", rec.Code, rec.Body.String())
	}

	tasks := h.tasks.snapshot()
	if len(tasks) != 6 {
		t.Fatalf("want 3 recipients × 2 channels = 6 tasks, got %d", len(tasks))
	}
	for _, task := range tasks {
		if task.Type() != notifications.TaskTypeAnnouncement {
			t.Errorf("unexpected task type %q", task.Type())
		}
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["tasks_enqueued"] != float64(6) {
		t.Errorf("tasks_enqueued = %v, want 6", resp["tasks_enqueued"])
	}
}

// TestFR_ANN_001_ConcurrentSendsBroadcastOnce is the double-click guard at
// the HTTP layer: eight simultaneous sends must produce one broadcast.
func TestFR_ANN_001_ConcurrentSendsBroadcastOnce(t *testing.T) {
	h := newAnnHarness(t)
	h.anns.seed(notifications.Announcement{
		Title: "Maintenance", Body: "Outage", Channels: []string{"sms"},
		Class: "marketing", CreatedBy: "ops1",
	})
	h.anns.segment = []int{1, 2, 3, 4, 5}

	const racers = 8
	var wg sync.WaitGroup
	codes := make([]int, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = h.do(t, http.MethodPost, "/api/v1/announcements/1/send", ``, "billing_admin", "ops1").Code
		}(i)
	}
	close(start)
	wg.Wait()

	accepted, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusAccepted:
			accepted++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if accepted != 1 {
		t.Errorf("DOUBLE BROADCAST: %d of %d sends were accepted, want exactly 1", accepted, racers)
	}
	if conflicts != racers-1 {
		t.Errorf("want %d conflicts, got %d", racers-1, conflicts)
	}
	// 5 recipients × 1 channel, sent once.
	if len(h.tasks.snapshot()) != 5 {
		t.Errorf("want 5 tasks from a single broadcast, got %d", len(h.tasks.snapshot()))
	}
}

func TestFR_ANN_001_SendingAnAlreadySentAnnouncementIs409(t *testing.T) {
	h := newAnnHarness(t)
	h.anns.seed(notifications.Announcement{
		Title: "X", Body: "Y", Channels: []string{"sms"},
		Class: "marketing", CreatedBy: "ops1", Status: notifications.AnnouncementSent,
	})

	rec := h.do(t, http.MethodPost, "/api/v1/announcements/1/send", ``, "billing_admin", "ops1")
	if rec.Code != http.StatusConflict {
		t.Errorf("want 409, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(h.tasks.snapshot()) != 0 {
		t.Error("an already-sent announcement must not re-enqueue anything")
	}
}

func TestFR_ANN_001_SendingAnUnknownAnnouncementIs404(t *testing.T) {
	h := newAnnHarness(t)
	rec := h.do(t, http.MethodPost, "/api/v1/announcements/9999/send", ``, "billing_admin", "ops1")
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

// TestFR_ANN_001_PortalOnlySendEnqueuesNothingButSucceeds: zero dispatched
// recipients is a correct outcome for a banner, not a failure.
func TestFR_ANN_001_PortalOnlySendEnqueuesNothingButSucceeds(t *testing.T) {
	h := newAnnHarness(t)
	h.anns.seed(notifications.Announcement{
		Title: "Notice", Body: "Read me", Channels: []string{},
		ShowInPortal: true, Class: "marketing", CreatedBy: "ops1",
	})
	h.anns.segment = []int{1, 2}

	rec := h.do(t, http.MethodPost, "/api/v1/announcements/1/send", ``, "billing_admin", "ops1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(h.tasks.snapshot()) != 0 {
		t.Error("a banner-only announcement dispatches nothing")
	}
	if len(h.anns.finished) != 1 || h.anns.finished[0] != notifications.AnnouncementSent {
		t.Errorf("want the announcement recorded as sent, got %v", h.anns.finished)
	}
}

func TestFR_ANN_001_ComposingIsRestrictedToTheBillingTier(t *testing.T) {
	h := newAnnHarness(t)
	h.anns.seed(notifications.Announcement{
		Title: "X", Body: "Y", Channels: []string{"sms"}, Class: "marketing", CreatedBy: "ops1",
	})

	body := `{"title":"x","body":"y","channels":["sms"]}`
	for _, role := range []string{"csr", "technician", "noc_engineer"} {
		if rec := h.do(t, http.MethodPost, "/api/v1/announcements", body, role, "someone"); rec.Code != http.StatusForbidden {
			t.Errorf("%s composing: want 403, got %d", role, rec.Code)
		}
		if rec := h.do(t, http.MethodPost, "/api/v1/announcements/1/send", ``, role, "someone"); rec.Code != http.StatusForbidden {
			t.Errorf("%s sending: want 403, got %d", role, rec.Code)
		}
	}
}

// ── FR-NOTIF-013: push token registration ───────────────────────────────────

// TestFR_NOTIF_013_PushTokenBindsToTheTokensOwnSubscriber is the isolation
// that matters: the subscriber comes from the JWT, never the body, or anyone
// could point someone else's device at their account.
func TestFR_NOTIF_013_PushTokenBindsToTheTokensOwnSubscriber(t *testing.T) {
	h := newAnnHarness(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/push-tokens",
		strings.NewReader(`{"token":"device-abc","platform":"android","subscriber_id":999}`))
	req.Header.Set("Authorization", "Bearer "+itSubscriberToken(t, 42))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(h.tokens.registered[42]) != 1 {
		t.Errorf("the token must bind to the JWT's subscriber (42), got %+v", h.tokens.registered)
	}
	if len(h.tokens.registered[999]) != 0 {
		t.Error("a subscriber_id in the body must be ignored entirely")
	}
}

func TestFR_NOTIF_013_PushTokenValidates(t *testing.T) {
	h := newAnnHarness(t)

	for _, tc := range []struct{ name, body string }{
		{"missing token", `{"platform":"android"}`},
		{"unknown platform", `{"token":"abc","platform":"blackberry"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/push-tokens", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+itSubscriberToken(t, 42))
			rec := httptest.NewRecorder()
			h.mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A staff token is refused at the signature now, not at the role.
//
// It used to be refused at the role (403): the route verified the staff key,
// so a staff token parsed fine and only RequireRole turned it away. Now that
// the route verifies the portal key, a staff token does not survive signature
// verification at all — 401. That is the stronger of the two refusals and the
// point of signing the two audiences with different keys: a leaked staff
// token is not merely the wrong role here, it is unreadable.
func TestFR_NOTIF_013_StaffTokenCannotRegisterAPushDevice(t *testing.T) {
	h := newAnnHarness(t)
	rec := h.do(t, http.MethodPost, "/api/v1/push-tokens",
		`{"token":"abc","platform":"android"}`, "billing_admin", "ops1")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 for a staff-signed token on a subscriber route, got %d — %s",
			rec.Code, rec.Body.String())
	}
	if len(h.tokens.registered) != 0 {
		t.Errorf("a staff token registered a push device: %+v", h.tokens.registered)
	}
}

// ── The portal token the portal actually issues ─────────────────────────────

// FR-ANN-002: a subscriber token signed with the PORTAL secret reaches the
// banner feed, and one signed with the staff secret does not.
//
// This is the test that was missing. Until 2026-11-22 both subscriber routes
// verified against the staff key while every portal login signed with
// PORTAL_JWT_SECRET, so a real subscriber got a 401 from the banner feed and
// from push registration on every deployment path — confirmed against a
// running server. No test caught it because the only tokens ever presented
// were minted with the key the route checked, and the banner feed had no
// route-level test at all.
func TestFR_ANN_002_APortalSignedTokenReachesTheBannerFeed(t *testing.T) {
	h := newAnnHarness(t)
	h.anns.portalBanners = []notifications.Announcement{
		{ID: 7, Title: "Scheduled maintenance", Body: "Sunday 2am-4am", Status: "sent"},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/announcements/portal", nil)
	req.Header.Set("Authorization", "Bearer "+itSubscriberToken(t, 42))
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("a portal-signed subscriber token must reach its own banner feed, got %d — %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Scheduled maintenance") {
		t.Errorf("the feed should carry the subscriber's banner, got %s", rec.Body.String())
	}
	// The subscriber is taken from the token, never from a path or body.
	if got := h.anns.portalAskedFor; len(got) != 1 || got[0] != 42 {
		t.Errorf("the feed must be resolved for the token's own subscriber (42), got %v", got)
	}

	// The same claims signed with the staff key must not be accepted: if the
	// route still verified that key, this would pass and the test above would
	// pass too, which is exactly how the bug hid.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/announcements/portal", nil)
	req.Header.Set("Authorization", "Bearer "+itSubscriberTokenSignedWith(t, 42, itJWTSecret))
	rec = httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a subscriber token signed with the staff key must be refused, got %d — %s",
			rec.Code, rec.Body.String())
	}
}

// A missing portal secret refuses subscriber routes rather than verifying
// against an empty key, which anybody can sign a token with.
func TestSubscriberRoutes_WithoutAPortalSecretRefuseEverything(t *testing.T) {
	handler := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Announcements: newStubAnnouncements(), PushTokens: newStubPushTokens(),
		// PortalJWTSecret deliberately unset.
	})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, itJWTSecret)

	for _, tc := range []struct{ name, token string }{
		{"a token signed with the empty key", itSubscriberTokenSignedWith(t, 42, "")},
		{"a token signed with the staff key", itSubscriberTokenSignedWith(t, 42, itJWTSecret)},
		{"a portal-signed token", itSubscriberToken(t, 42)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/announcements/portal", nil)
			req.Header.Set("Authorization", "Bearer "+tc.token)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("want 503 with no portal secret configured, got %d — %s",
					rec.Code, rec.Body.String())
			}
			// Asserted on the code, not just the status: the handler returns
			// 503 too when its store is nil, and a test that accepted any 503
			// could not tell "refused, unconfigured auth" from "got through
			// auth, store missing".
			if !strings.Contains(rec.Body.String(), "ERR_PORTAL_AUTH_UNCONFIGURED") {
				t.Errorf("the refusal must name the unconfigured portal secret, got %s", rec.Body.String())
			}
		})
	}
}

func TestAnnouncementRoutes_UnconfiguredReturns503(t *testing.T) {
	handler := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		// Announcements and PushTokens left nil.
	})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/announcements", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when unconfigured, got %d", rec.Code)
	}
}
