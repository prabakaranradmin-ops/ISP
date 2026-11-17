//go:build integration

// Integration tests for the subscriber self-service portal.
//
// Covers INT-SUB-001 .. INT-SUB-003 from the Integration Tests tracker sheet
// (listed there under ./web/portal/; the handlers live in this package).
//
// Every request goes through the real login -> JWT -> route chain, so a test can
// only reach subscriber data by authenticating as that subscriber. INT-SUB-002
// checks the renewal callback moves no money: payments are credited only by
// the gateway's signed webhook (internal/api), never on a client's word.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/portal -Tags integration
package portal_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/bcrypt"
)

const itPortalSecret = "portal_integration_secret_32char!"

// ── Seeded portal store ─────────────────────────────────────────────────────

type itSubscriberSeed struct {
	ID           int
	Username     string
	Password     string
	PlanName     string
	Balance      decimal.Decimal
	Status       string
	Session      *portal.ActiveSession
	Notification []portal.NotificationEntry
	Tickets      []portal.TicketEntry
}

// itPortalStore serves every portal querier from one seeded data set, so a
// scoping bug shows up as one subscriber seeing another's rows.
type itPortalStore struct {
	mu   sync.Mutex
	subs map[int]*itSubscriberSeed
}

func newITPortalStore(seeds ...*itSubscriberSeed) *itPortalStore {
	s := &itPortalStore{subs: map[int]*itSubscriberSeed{}}
	for _, seed := range seeds {
		s.subs[seed.ID] = seed
	}
	return s
}

func (s *itPortalStore) GetSubscriberByUsername(_ context.Context, username string) (*portal.SubscriberAuth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seed := range s.subs {
		if seed.Username == username {
			hash, err := bcrypt.GenerateFromPassword([]byte(seed.Password), bcrypt.MinCost)
			if err != nil {
				return nil, err
			}
			return &portal.SubscriberAuth{ID: seed.ID, Username: seed.Username, PasswordHash: string(hash)}, nil
		}
	}
	return nil, nil
}

func (s *itPortalStore) GetSubscriberByID(_ context.Context, id int) (*portal.SubscriberProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seed, ok := s.subs[id]
	if !ok {
		return nil, nil
	}
	expiry := time.Now().Add(20 * 24 * time.Hour)
	return &portal.SubscriberProfile{
		ID:            seed.ID,
		Username:      seed.Username,
		MobileNumber:  "+919876543210",
		PlanName:      seed.PlanName,
		PlanExpiry:    &expiry,
		WalletBalance: seed.Balance,
		Status:        seed.Status,
		DunningState:  "active",
	}, nil
}

func (s *itPortalStore) GetActiveSession(_ context.Context, subscriberID int) (*portal.ActiveSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seed, ok := s.subs[subscriberID]
	if !ok {
		return nil, nil
	}
	return seed.Session, nil
}

func (s *itPortalStore) ListNotifications(_ context.Context, subscriberID, limit int) ([]portal.NotificationEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seed, ok := s.subs[subscriberID]
	if !ok {
		return nil, nil
	}
	rows := seed.Notification
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (s *itPortalStore) ListTickets(_ context.Context, subscriberID int) ([]portal.TicketEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seed, ok := s.subs[subscriberID]
	if !ok {
		return nil, nil
	}
	return seed.Tickets, nil
}

func (s *itPortalStore) CreateTicket(_ context.Context, req portal.TicketCreateRequest) (*portal.TicketEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seed, ok := s.subs[req.SubscriberID]
	if !ok {
		return nil, fmt.Errorf("subscriber %d not found", req.SubscriberID)
	}
	ticket := portal.TicketEntry{
		ID:          100 + len(seed.Tickets),
		Category:    req.Category,
		Description: req.Description,
		Status:      "open",
		CreatedAt:   time.Now(),
	}
	seed.Tickets = append(seed.Tickets, ticket)
	return &ticket, nil
}

// ── Harness ─────────────────────────────────────────────────────────────────

type itPortal struct {
	mux   *http.ServeMux
	store *itPortalStore
}

func itNewPortal(t *testing.T, seeds ...*itSubscriberSeed) *itPortal {
	t.Helper()
	store := newITPortalStore(seeds...)

	h := portal.NewHandler(store, store, store, store, nil, itPortalSecret)

	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return &itPortal{mux: mux, store: store}
}

// login authenticates and returns the subscriber's bearer token.
func (p *itPortal) login(t *testing.T, username, password string) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req := httptest.NewRequest(http.MethodPost, "/portal/login", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login %s: want 200, got %d — %s", username, rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if resp["token"] == "" {
		t.Fatal("login returned no token")
	}
	return resp["token"]
}

func (p *itPortal) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.mux.ServeHTTP(rec, req)
	return rec
}

// ── INT-SUB-001 ─────────────────────────────────────────────────────────────

// TestFR_SUB_001_Dashboard_ShowsUsage verifies the dashboard returns the subscriber's live
// session usage alongside wallet and plan state.
//
// INT-SUB-001 | FR-SUB-001
func TestFR_SUB_001_Dashboard_ShowsUsage(t *testing.T) {
	session := &portal.ActiveSession{
		SessionID:    "sess-live-001",
		NASIP:        "10.10.0.1",
		AssignedIP:   "100.64.12.7",
		BytesIn:      912_680_550_400,
		BytesOut:     108_447_924_224,
		GBUsed:       decimal.RequireFromString("950.25"),
		GBIncluded:   decimal.RequireFromString("3300.00"),
		PctUsed:      28.79,
		FUPThrottled: false,
		StartedAt:    time.Now().Add(-3 * time.Hour),
	}
	p := itNewPortal(t, &itSubscriberSeed{
		ID: 1, Username: "usage@isp", Password: "pw-usage",
		PlanName: "100 Mbps Unlimited", Balance: decimal.RequireFromString("250.00"),
		Status: "active", Session: session,
	})

	token := p.login(t, "usage@isp", "pw-usage")
	rec := p.do(t, http.MethodGet, "/portal/dashboard", token, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	var resp portal.DashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}

	if resp.ActiveSession == nil {
		t.Fatal("active_session must be present for a subscriber with a live session")
	}
	if !resp.ActiveSession.GBUsed.Equal(session.GBUsed) {
		t.Errorf("gb_used: want %s, got %s", session.GBUsed, resp.ActiveSession.GBUsed)
	}
	if !resp.ActiveSession.GBIncluded.Equal(session.GBIncluded) {
		t.Errorf("gb_included: want %s, got %s", session.GBIncluded, resp.ActiveSession.GBIncluded)
	}
	if resp.ActiveSession.SessionID != session.SessionID {
		t.Errorf("session_id: want %s, got %s", session.SessionID, resp.ActiveSession.SessionID)
	}
	if !resp.WalletBalance.Equal(decimal.RequireFromString("250.00")) {
		t.Errorf("wallet_balance: want 250.00, got %s", resp.WalletBalance)
	}
	if resp.PlanName != "100 Mbps Unlimited" {
		t.Errorf("plan_name: want '100 Mbps Unlimited', got %q", resp.PlanName)
	}

	// The seeded usage figures must be visible in the rendered payload.
	body := rec.Body.String()
	for _, want := range []string{"950.25", "3300", "sess-live-001"} {
		if !strings.Contains(body, want) {
			t.Errorf("response body missing seeded value %q: %s", want, body)
		}
	}
}

// TestFR_SUB_001_Dashboard_ThrottledSessionReported verifies a throttled session surfaces
// its FUP state so the subscriber can see why speeds dropped.
//
// INT-SUB-001 (supporting) | FR-SUB-001
func TestFR_SUB_001_Dashboard_ThrottledSessionReported(t *testing.T) {
	p := itNewPortal(t, &itSubscriberSeed{
		ID: 1, Username: "throttled@isp", Password: "pw",
		PlanName: "Basic", Balance: decimal.Zero, Status: "active",
		Session: &portal.ActiveSession{
			SessionID: "sess-throttled", GBUsed: decimal.RequireFromString("3400.00"),
			GBIncluded: decimal.RequireFromString("3300.00"), PctUsed: 103.03, FUPThrottled: true,
		},
	})

	token := p.login(t, "throttled@isp", "pw")
	rec := p.do(t, http.MethodGet, "/portal/dashboard", token, "")

	var resp portal.DashboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	if resp.ActiveSession == nil || !resp.ActiveSession.FUPThrottled {
		t.Error("expected fup_throttled=true for a session past its quota")
	}
}

// TestFR_SUB_001_Dashboard_RequiresSubscriberToken verifies the dashboard is closed to
// anonymous callers.
//
// INT-SUB-001 (supporting) | FR-SUB-001
func TestFR_SUB_001_Dashboard_RequiresSubscriberToken(t *testing.T) {
	p := itNewPortal(t, &itSubscriberSeed{ID: 1, Username: "a@isp", Password: "pw", Status: "active"})

	rec := p.do(t, http.MethodGet, "/portal/dashboard", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 without a token, got %d", rec.Code)
	}
}

// ── INT-SUB-002 ─────────────────────────────────────────────────────────────

// TestFR_SUB_003_RenewalCallback_MovesNoMoney is the regression test for the
// callback that credited whatever a subscriber's client sent. A logged-in
// subscriber naming any payment and any amount gets an acknowledgement and
// nothing else: the handler no longer has a wallet to credit, and the reply
// claims no transaction and no balance.
//
// INT-SUB-002 | FR-SUB-003, FR-SEC-004
func TestFR_SUB_003_RenewalCallback_MovesNoMoney(t *testing.T) {
	p := itNewPortal(t, &itSubscriberSeed{
		ID: 1, Username: "renew@isp", Password: "pw-renew",
		PlanName: "100 Mbps Unlimited", Balance: decimal.Zero, Status: "active",
	})
	token := p.login(t, "renew@isp", "pw-renew")

	for name, body := range map[string]string{
		"an invented payment for a large sum": `{"payment_id":"pay_made_up","amount":"99999.00"}`,
		"no payment at all":                   `{}`,
		"malformed":                           `{"payment_id":`,
	} {
		t.Run(name, func(t *testing.T) {
			rec := p.do(t, http.MethodPost, "/portal/renew/callback", token, body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("want 202, got %d — %s", rec.Code, rec.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["status"] != "awaiting_gateway_confirmation" {
				t.Errorf("status: %v", got["status"])
			}
			for _, claim := range []string{"transaction_id", "wallet_balance"} {
				if _, ok := got[claim]; ok {
					t.Errorf("the reply must not claim a %s", claim)
				}
			}
		})
	}

	// The subscriber's balance, as the portal itself reports it, is untouched.
	rec := p.do(t, http.MethodGet, "/portal/dashboard", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: %d", rec.Code)
	}
	var dash struct {
		WalletBalance decimal.Decimal `json:"wallet_balance"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if !dash.WalletBalance.IsZero() {
		t.Errorf("balance after callbacks: want 0, got %s", dash.WalletBalance)
	}
}

// TestFR_SUB_003_RenewalCallback_RequiresAuth: anonymous callers are still
// refused before anything is logged against a subscriber.
//
// INT-SUB-002 (supporting) | FR-SUB-003
func TestFR_SUB_003_RenewalCallback_RequiresAuth(t *testing.T) {
	p := itNewPortal(t, &itSubscriberSeed{
		ID: 1, Username: "renew4@isp", Password: "pw", Balance: decimal.Zero, Status: "active",
	})
	rec := p.do(t, http.MethodPost, "/portal/renew/callback", "", `{"payment_id":"pay_anon","amount":"799.00"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("want 401 without a token, got %d", rec.Code)
	}
}

// ── INT-SUB-003 ─────────────────────────────────────────────────────────────

// TestFR_SUB_005_NotificationHistory_ScopedToSubscriber verifies each subscriber sees only
// their own notification_log rows.
//
// INT-SUB-003 | FR-SUB-005
func TestFR_SUB_005_NotificationHistory_ScopedToSubscriber(t *testing.T) {
	p := itNewPortal(t,
		&itSubscriberSeed{
			ID: 1, Username: "alice@isp", Password: "pw-alice", Status: "active",
			Notification: []portal.NotificationEntry{
				{ID: 11, Channel: "whatsapp", TemplateName: "fup_warning_80pct", Class: "transactional", DeliveryStatus: "delivered"},
				{ID: 12, Channel: "sms", TemplateName: "payment_reminder", Class: "transactional", DeliveryStatus: "sent"},
			},
		},
		&itSubscriberSeed{
			ID: 2, Username: "bob@isp", Password: "pw-bob", Status: "active",
			Notification: []portal.NotificationEntry{
				{ID: 21, Channel: "whatsapp", TemplateName: "service_suspended", Class: "transactional", DeliveryStatus: "read"},
			},
		},
	)

	aliceToken := p.login(t, "alice@isp", "pw-alice")
	rec := p.do(t, http.MethodGet, "/portal/notifications", aliceToken, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	var rows []portal.NotificationEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode notifications: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("alice must see exactly her 2 rows, got %d: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.ID == 21 {
			t.Error("alice received bob's notification row")
		}
	}
	if strings.Contains(rec.Body.String(), "service_suspended") {
		t.Error("response leaked another subscriber's notification")
	}

	// Bob sees only his own row.
	bobToken := p.login(t, "bob@isp", "pw-bob")
	rec = p.do(t, http.MethodGet, "/portal/notifications", bobToken, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode bob's notifications: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 21 {
		t.Errorf("bob must see exactly his 1 row, got %+v", rows)
	}
}

// TestFR_SUB_005_TicketHistory_ScopedToSubscriber verifies ticket history is scoped the
// same way, and that a created ticket is attributed to the caller.
//
// INT-SUB-003 (supporting) | FR-SUB-005
func TestFR_SUB_005_TicketHistory_ScopedToSubscriber(t *testing.T) {
	p := itNewPortal(t,
		&itSubscriberSeed{
			ID: 1, Username: "alice@isp", Password: "pw-alice", Status: "active",
			Tickets: []portal.TicketEntry{{ID: 1, Category: "connectivity", Description: "No internet", Status: "open"}},
		},
		&itSubscriberSeed{
			ID: 2, Username: "bob@isp", Password: "pw-bob", Status: "active",
			Tickets: []portal.TicketEntry{{ID: 2, Category: "billing", Description: "Wrong invoice", Status: "open"}},
		},
	)

	aliceToken := p.login(t, "alice@isp", "pw-alice")

	rec := p.do(t, http.MethodGet, "/portal/tickets", aliceToken, "")
	var tickets []portal.TicketEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &tickets); err != nil {
		t.Fatalf("decode tickets: %v", err)
	}
	if len(tickets) != 1 || tickets[0].ID != 1 {
		t.Errorf("alice must see only her ticket, got %+v", tickets)
	}
	if strings.Contains(rec.Body.String(), "Wrong invoice") {
		t.Error("response leaked another subscriber's ticket")
	}

	// A new ticket is filed against the authenticated subscriber, not a body value.
	created := p.do(t, http.MethodPost, "/portal/tickets", aliceToken,
		`{"subscriber_id":2,"category":"speed","description":"Slow at night"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create ticket: want 201, got %d — %s", created.Code, created.Body.String())
	}

	bobToken := p.login(t, "bob@isp", "pw-bob")
	rec = p.do(t, http.MethodGet, "/portal/tickets", bobToken, "")
	if err := json.Unmarshal(rec.Body.Bytes(), &tickets); err != nil {
		t.Fatalf("decode bob's tickets: %v", err)
	}
	if len(tickets) != 1 {
		t.Errorf("ticket must be filed against the caller, not the body's subscriber_id; bob has %d", len(tickets))
	}
	if strings.Contains(rec.Body.String(), "Slow at night") {
		t.Error("alice's new ticket was attributed to bob")
	}
}

// TestFR_SUB_005_PortalMe_ScopedToSubscriber verifies the profile endpoint returns the
// caller's own record.
//
// INT-SUB-003 (supporting) | FR-SUB-005
func TestFR_SUB_005_PortalMe_ScopedToSubscriber(t *testing.T) {
	p := itNewPortal(t,
		&itSubscriberSeed{ID: 1, Username: "alice@isp", Password: "pw-alice", Status: "active", PlanName: "Plan A"},
		&itSubscriberSeed{ID: 2, Username: "bob@isp", Password: "pw-bob", Status: "active", PlanName: "Plan B"},
	)

	token := p.login(t, "bob@isp", "pw-bob")
	rec := p.do(t, http.MethodGet, "/portal/me", token, "")

	var profile portal.SubscriberProfile
	if err := json.Unmarshal(rec.Body.Bytes(), &profile); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	if profile.ID != 2 || profile.Username != "bob@isp" {
		t.Errorf("want bob's own profile, got %+v", profile)
	}
	if profile.PlanName != "Plan B" {
		t.Errorf("plan_name: want Plan B, got %q", profile.PlanName)
	}
}
