package portal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/bcrypt"
)

// â”€â”€ Stubs â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

type stubSubDB struct{}

func (s *stubSubDB) GetSubscriberByUsername(_ context.Context, username string) (*portal.SubscriberAuth, error) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("testpass"), 12)
	return &portal.SubscriberAuth{ID: 1, Username: username, PasswordHash: string(hash)}, nil
}

func (s *stubSubDB) GetSubscriberByID(_ context.Context, id int) (*portal.SubscriberProfile, error) {
	exp := time.Now().Add(30 * 24 * time.Hour)
	return &portal.SubscriberProfile{
		ID:            id,
		Username:      "testuser",
		MobileNumber:  "+919876543210",
		PlanName:      "100 Mbps Unlimited",
		PlanExpiry:    &exp,
		WalletBalance: decimal.NewFromFloat(250.00),
		Status:        "active",
	}, nil
}

type stubSessions struct{}

func (s *stubSessions) GetActiveSession(_ context.Context, _ int) (*portal.ActiveSession, error) {
	return &portal.ActiveSession{
		SessionID:  "sess-001",
		GBUsed:     decimal.NewFromFloat(100),
		GBIncluded: decimal.NewFromFloat(3300),
		PctUsed:    3.03,
	}, nil
}

type stubNotifs struct{}

func (s *stubNotifs) ListNotifications(_ context.Context, _ int, _ int) ([]portal.NotificationEntry, error) {
	return []portal.NotificationEntry{
		{ID: 1, Channel: "whatsapp", TemplateName: "FUP Warning", DeliveryStatus: "delivered"},
	}, nil
}

type stubTickets struct{}

func (s *stubTickets) ListTickets(_ context.Context, _ int) ([]portal.TicketEntry, error) {
	return []portal.TicketEntry{
		{ID: 1, Category: "connectivity", Description: "No internet", Status: "open"},
	}, nil
}

func (s *stubTickets) CreateTicket(_ context.Context, req portal.TicketCreateRequest) (*portal.TicketEntry, error) {
	return &portal.TicketEntry{ID: 2, Category: req.Category, Description: req.Description, Status: "open"}, nil
}

// â”€â”€ Tests â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func newTestHandler(t *testing.T) *http.ServeMux {
	t.Helper()
	h := portal.NewHandler(
		&stubSubDB{},
		&stubSessions{},
		&stubNotifs{},
		&stubTickets{},
		nil, // razorpay — nil tested separately
		"test-portal-secret",
	)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	return mux
}

func TestPortalLogin_ValidCredentials(t *testing.T) {
	mux := newTestHandler(t)

	body := `{"username":"testuser","password":"testpass"}`
	req := httptest.NewRequest("POST", "/portal/login", bytes.NewBufferString(body)) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("login: want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp) //nolint:errcheck,gosec
	if resp["token"] == "" {
		t.Fatal("expected token in response")
	}
}

func TestPortalLogin_InvalidPassword(t *testing.T) {
	mux := newTestHandler(t)

	body := `{"username":"testuser","password":"wrongpass"}`
	req := httptest.NewRequest("POST", "/portal/login", bytes.NewBufferString(body)) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestPortalDashboard_Unauthenticated(t *testing.T) {
	mux := newTestHandler(t)

	req := httptest.NewRequest("GET", "/portal/dashboard", nil) //nolint:noctx
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}
}

func TestPortalRenew_NoGateway(t *testing.T) {
	mux := newTestHandler(t)

	// Login first to get a token
	loginBody := `{"username":"testuser","password":"testpass"}`
	loginReq := httptest.NewRequest("POST", "/portal/login", bytes.NewBufferString(loginBody)) //nolint:noctx
	loginRec := httptest.NewRecorder()
	mux.ServeHTTP(loginRec, loginReq)

	var loginResp map[string]string
	json.NewDecoder(loginRec.Body).Decode(&loginResp) //nolint:errcheck,gosec
	token := loginResp["token"]

	renewBody := `{"amount":"799.00"}`
	renewReq := httptest.NewRequest("POST", "/portal/renew", bytes.NewBufferString(renewBody)) //nolint:noctx
	renewReq.Header.Set("Authorization", "Bearer "+token)
	renewRec := httptest.NewRecorder()
	mux.ServeHTTP(renewRec, renewReq)

	// razorpay is nil, expect 503
	if renewRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 without gateway, got %d — %s", renewRec.Code, renewRec.Body.String())
	}
}
