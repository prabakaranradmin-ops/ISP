// Package portal implements the subscriber self-service portal API.
//
// Provides subscriber-facing JWT auth, usage dashboard, plan renewal, and
// notification + ticket history.
//
// FR: FR-SUB-001..005 | DDS §5.8 | API §7 (portal endpoints)
package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials is returned by Authenticate for any of: unknown
// username, wrong password, or a subscriber lookup failure — collapsed into
// one sentinel so callers cannot distinguish "no such user" from "wrong
// password" and inadvertently leak that distinction to the client.
var ErrInvalidCredentials = errors.New("portal: invalid credentials")

// â”€â”€ Querier interfaces â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// PortalSubscriberQuerier is the minimal DB surface used by portal handlers.
type PortalSubscriberQuerier interface {
	GetSubscriberByUsername(ctx context.Context, username string) (*SubscriberAuth, error)
	GetSubscriberByID(ctx context.Context, id int) (*SubscriberProfile, error)
}

// PortalSessionQuerier fetches the active RADIUS session for a subscriber.
type PortalSessionQuerier interface {
	GetActiveSession(ctx context.Context, subscriberID int) (*ActiveSession, error)
}

// PortalSessionHistoryQuerier lists a subscriber's past internet sessions.
type PortalSessionHistoryQuerier interface {
	ListSessionHistory(ctx context.Context, subscriberID, limit int) ([]SessionHistoryEntry, error)
}

// PortalNotificationQuerier fetches the notification history.
type PortalNotificationQuerier interface {
	ListNotifications(ctx context.Context, subscriberID int, limit int) ([]NotificationEntry, error)
}

// PortalTicketQuerier fetches and creates tickets.
type PortalTicketQuerier interface {
	ListTickets(ctx context.Context, subscriberID int) ([]TicketEntry, error)
	CreateTicket(ctx context.Context, t TicketCreateRequest) (*TicketEntry, error)
}

// RazorpayOrderCreator creates a Razorpay order and returns a payment link.
type RazorpayOrderCreator interface {
	CreateOrder(ctx context.Context, subscriberID int, amount decimal.Decimal) (string, string, error)
}

// PlanExpiryStore extends a subscriber's plan validity after a renewal is
// credited. Satisfied by *db.PortalStore.
type PlanExpiryStore interface {
	// GetPlanRenewalInfo returns the validity window (in days) of the
	// subscriber's current plan and their current plan_expiry (nil if never
	// set), for computing where a renewal extends to.
	GetPlanRenewalInfo(ctx context.Context, subscriberID int) (validityDays int, currentExpiry *time.Time, err error)
	SetPlanExpiry(ctx context.Context, subscriberID int, expiry time.Time) error
}

// RenewalPayment is the outcome of applying a completed renewal payment.
type RenewalPayment struct {
	TransactionID int             `json:"transaction_id"`
	Balance       decimal.Decimal `json:"wallet_balance"`
}

// RenewalProcessor credits a completed renewal payment to the subscriber's
// wallet. Implementations must be idempotent on paymentID: the gateway may
// deliver the same callback more than once, and a subscriber must never be
// charged — or credited — twice for one payment.
type RenewalProcessor interface {
	ApplyRenewal(ctx context.Context, subscriberID int, amount decimal.Decimal, paymentID string) (*RenewalPayment, error)
}

// â”€â”€ Domain types â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// SubscriberAuth holds credentials for portal login.
type SubscriberAuth struct {
	ID           int
	Username     string
	PasswordHash string
}

// SubscriberProfile is the self-service view of a subscriber.
type SubscriberProfile struct {
	ID            int             `json:"id"`
	Username      string          `json:"username"`
	MobileNumber  string          `json:"mobile_number"`
	PlanName      string          `json:"plan_name"`
	PlanExpiry    *time.Time      `json:"plan_expiry,omitempty"`
	WalletBalance decimal.Decimal `json:"wallet_balance"`
	Status        string          `json:"status"`
	DunningState  string          `json:"dunning_state"`
}

// ActiveSession carries the subscriber's current internet session state.
type ActiveSession struct {
	SessionID    string          `json:"session_id"`
	NASIP        string          `json:"nas_ip"`
	AssignedIP   string          `json:"assigned_ip"`
	BytesIn      int64           `json:"bytes_in"`
	BytesOut     int64           `json:"bytes_out"`
	GBUsed       decimal.Decimal `json:"gb_used"`
	GBIncluded   decimal.Decimal `json:"gb_included"`
	PctUsed      float64         `json:"pct_used"`
	SpeedProfile string          `json:"speed_profile"`
	FUPThrottled bool            `json:"fup_throttled"`
	StartedAt    time.Time       `json:"started_at"`
}

// SessionHistoryEntry is one past (or currently active) internet session for
// a subscriber, from subscriber_session_history.
type SessionHistoryEntry struct {
	SessionID      string          `json:"session_id"`
	NASIP          string          `json:"nas_ip"`
	AssignedIP     string          `json:"assigned_ip,omitempty"`
	StartTime      time.Time       `json:"start_time"`
	StopTime       *time.Time      `json:"stop_time,omitempty"` // nil = still active
	GBUsed         decimal.Decimal `json:"gb_used"`
	TerminateCause string          `json:"terminate_cause,omitempty"`
}

// NotificationEntry is one row from notification_log.
type NotificationEntry struct {
	ID             int       `json:"id"`
	Channel        string    `json:"channel"`
	TemplateName   string    `json:"template_name"`
	Class          string    `json:"class"`
	DeliveryStatus string    `json:"delivery_status"`
	SentAt         time.Time `json:"sent_at"`
}

// TicketEntry is one row from the tickets table.
//
// Priority and the two SLA due-by times are populated (migration
// 023_create_sla_engine.sql, resolveTicketSLA at ticket creation) whether or
// not a reader ever looks at them — ListTickets did not select them at all
// until the staff console's Tickets screen needed to show them (CRD-EXP-005),
// which is why this shared type, used by both the subscriber portal and the
// staff console, carries fields the portal's own templates simply do not
// render: a subscriber does not set their own priority (see CreateTicket's
// own comment), but that is a rendering choice, not a reason to fetch a
// narrower row for one of the two callers.
type TicketEntry struct {
	ID                 int        `json:"id"`
	Category           string     `json:"category"`
	Description        string     `json:"description"`
	Status             string     `json:"status"`
	Priority           string     `json:"priority"`
	SLAResponseDueAt   *time.Time `json:"sla_response_due_at,omitempty"`
	SLAResolutionDueAt *time.Time `json:"sla_resolution_due_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
}

// TicketCreateRequest is the body for POST /portal/tickets.
type TicketCreateRequest struct {
	SubscriberID int    `json:"subscriber_id"`
	Category     string `json:"category"`
	Description  string `json:"description"`
}

// DashboardResponse aggregates wallet, plan, session, and FUP state.
type DashboardResponse struct {
	WalletBalance decimal.Decimal `json:"wallet_balance"`
	PlanName      string          `json:"plan_name"`
	PlanExpiry    *time.Time      `json:"plan_expiry,omitempty"`
	Status        string          `json:"status"`
	ActiveSession *ActiveSession  `json:"active_session,omitempty"`
}

// â”€â”€ Handler â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// Handler is the portal HTTP handler.
type Handler struct {
	subscribers   PortalSubscriberQuerier
	sessions      PortalSessionQuerier
	notifications PortalNotificationQuerier
	tickets       PortalTicketQuerier
	razorpay      RazorpayOrderCreator
	renewals      RenewalProcessor
	jwtSecret     string
}

// NewHandler constructs the portal Handler.
func NewHandler(
	subscribers PortalSubscriberQuerier,
	sessions PortalSessionQuerier,
	notifications PortalNotificationQuerier,
	tickets PortalTicketQuerier,
	razorpay RazorpayOrderCreator,
	jwtSecret string,
) *Handler {
	return &Handler{
		subscribers:   subscribers,
		sessions:      sessions,
		notifications: notifications,
		tickets:       tickets,
		razorpay:      razorpay,
		jwtSecret:     jwtSecret,
	}
}

// SetRenewalProcessor wires the component that credits completed renewals.
func (h *Handler) SetRenewalProcessor(p RenewalProcessor) {
	h.renewals = p
}

// RegisterRoutes wires all portal routes onto mux.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Public
	mux.HandleFunc("POST /portal/login", h.Login)

	// Subscriber-authenticated routes
	auth := middleware.JWTMiddleware(h.jwtSecret)
	self := func(next http.Handler) http.Handler {
		return auth(middleware.RequireRole("subscriber")(next))
	}
	mux.Handle("GET /portal/me", self(http.HandlerFunc(h.Me)))
	mux.Handle("GET /portal/dashboard", self(http.HandlerFunc(h.Dashboard)))
	mux.Handle("POST /portal/renew", self(http.HandlerFunc(h.Renew)))
	mux.Handle("POST /portal/renew/callback", self(http.HandlerFunc(h.RenewalCallback)))
	mux.Handle("GET /portal/notifications", self(http.HandlerFunc(h.ListNotifications)))
	mux.Handle("GET /portal/tickets", self(http.HandlerFunc(h.ListTickets)))
	mux.Handle("POST /portal/tickets", self(http.HandlerFunc(h.CreateTicket)))
}

// â”€â”€ POR-001: Auth â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// Login handles POST /portal/login.
// Verifies credentials and issues a subscriber-scoped JWT (role="subscriber").
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("ERR_BAD_REQUEST", "invalid request body"))
		return
	}
	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusUnprocessableEntity, errBody("ERR_VALIDATION", "username and password required"))
		return
	}

	tokenStr, err := Authenticate(r.Context(), h.subscribers, req.Username, req.Password, h.jwtSecret)
	if errors.Is(err, ErrInvalidCredentials) {
		writeJSON(w, http.StatusUnauthorized, errBody("ERR_UNAUTHORIZED", "invalid credentials"))
		return
	}
	if err != nil {
		log.Error().Err(err).Msg("portal: jwt sign failed")
		writeJSON(w, http.StatusInternalServerError, errBody("ERR_INTERNAL", "token generation failed"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tokenStr})
}

// Authenticate verifies a subscriber's username/password against subscribers,
// and on success issues a signed portal JWT. Shared by the JSON Login handler
// and any other frontend (e.g. the server-rendered portal UI) that needs the
// exact same "what makes a valid subscriber login" logic in one place.
func Authenticate(ctx context.Context, subscribers PortalSubscriberQuerier, username, password, jwtSecret string) (string, error) {
	sub, err := subscribers.GetSubscriberByUsername(ctx, username)
	if err != nil || sub == nil {
		// Constant-time fallback to prevent username enumeration
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$12$dummyhashforenumeration/protect"), []byte(password)) //nolint:errcheck
		return "", ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(sub.PasswordHash), []byte(password)); err != nil {
		return "", ErrInvalidCredentials
	}
	return issueSubscriberJWT(sub.ID, sub.Username, jwtSecret)
}

// issueSubscriberJWT creates a 24-hour JWT with role="subscriber".
func issueSubscriberJWT(subscriberID int, username, secret string) (string, error) {
	claims := middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   username,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Role:         "subscriber",
		SubscriberID: subscriberID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// Me handles GET /portal/me — returns the subscriber's own profile.
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	subID := subscriberIDFromCtx(r)
	if subID == 0 {
		writeJSON(w, http.StatusUnauthorized, errBody("ERR_UNAUTHORIZED", "missing subscriber context"))
		return
	}
	profile, err := h.subscribers.GetSubscriberByID(r.Context(), subID)
	if err != nil || profile == nil {
		writeJSON(w, http.StatusNotFound, errBody("ERR_NOT_FOUND", "subscriber not found"))
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// â”€â”€ POR-002: Dashboard â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// Dashboard handles GET /portal/dashboard.
// Returns wallet balance, plan details, current session usage, and FUP status.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	subID := subscriberIDFromCtx(r)
	if subID == 0 {
		writeJSON(w, http.StatusUnauthorized, errBody("ERR_UNAUTHORIZED", "missing subscriber context"))
		return
	}

	profile, err := h.subscribers.GetSubscriberByID(r.Context(), subID)
	if err != nil || profile == nil {
		writeJSON(w, http.StatusNotFound, errBody("ERR_NOT_FOUND", "subscriber not found"))
		return
	}

	resp := DashboardResponse{
		WalletBalance: profile.WalletBalance,
		PlanName:      profile.PlanName,
		PlanExpiry:    profile.PlanExpiry,
		Status:        profile.Status,
	}

	if h.sessions != nil {
		sess, err := h.sessions.GetActiveSession(r.Context(), subID)
		if err == nil && sess != nil {
			resp.ActiveSession = sess
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// â”€â”€ POR-003: One-tap renewal â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// Renew handles POST /portal/renew.
// Creates a Razorpay order and returns a payment deeplink for the subscriber.
func (h *Handler) Renew(w http.ResponseWriter, r *http.Request) {
	subID := subscriberIDFromCtx(r)
	if subID == 0 {
		writeJSON(w, http.StatusUnauthorized, errBody("ERR_UNAUTHORIZED", "missing subscriber context"))
		return
	}

	var req struct {
		Amount string `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("ERR_BAD_REQUEST", "invalid request body"))
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		writeJSON(w, http.StatusUnprocessableEntity, errBody("ERR_VALIDATION", "invalid amount"))
		return
	}

	if h.razorpay == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("ERR_UNAVAILABLE", "payment gateway not configured"))
		return
	}

	orderID, paymentLink, err := h.razorpay.CreateOrder(r.Context(), subID, amount)
	if err != nil {
		log.Error().Err(err).Msg("portal: razorpay create order failed")
		writeJSON(w, http.StatusBadGateway, errBody("ERR_GATEWAY", "payment gateway error"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"order_id":     orderID,
		"payment_link": paymentLink,
	})
}

// â”€â”€ POR-004: Notification history + tickets â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

// RenewalCallback handles POST /portal/renew/callback.
// The payment gateway may deliver the same callback more than once, so crediting
// is keyed on payment_id and a replay returns the original transaction.
func (h *Handler) RenewalCallback(w http.ResponseWriter, r *http.Request) {
	subID := subscriberIDFromCtx(r)
	if subID == 0 {
		writeJSON(w, http.StatusUnauthorized, errBody("ERR_UNAUTHORIZED", "missing subscriber context"))
		return
	}

	var req struct {
		PaymentID string `json:"payment_id"`
		Amount    string `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("ERR_BAD_REQUEST", "invalid request body"))
		return
	}
	if req.PaymentID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, errBody("ERR_VALIDATION", "payment_id is required"))
		return
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.LessThanOrEqual(decimal.Zero) {
		writeJSON(w, http.StatusUnprocessableEntity, errBody("ERR_VALIDATION", "invalid amount"))
		return
	}

	if h.renewals == nil {
		writeJSON(w, http.StatusServiceUnavailable, errBody("ERR_UNAVAILABLE", "renewal processing not configured"))
		return
	}

	payment, err := h.renewals.ApplyRenewal(r.Context(), subID, amount, req.PaymentID)
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", subID).Msg("portal: renewal credit failed")
		writeJSON(w, http.StatusInternalServerError, errBody("ERR_INTERNAL", "renewal could not be applied"))
		return
	}

	writeJSON(w, http.StatusOK, payment)
}

// ListNotifications handles GET /portal/notifications.
func (h *Handler) ListNotifications(w http.ResponseWriter, r *http.Request) {
	subID := subscriberIDFromCtx(r)
	if subID == 0 {
		writeJSON(w, http.StatusUnauthorized, errBody("ERR_UNAUTHORIZED", "missing subscriber context"))
		return
	}

	notifications, err := h.notifications.ListNotifications(r.Context(), subID, 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("ERR_INTERNAL", "failed to fetch notifications"))
		return
	}
	if notifications == nil {
		notifications = []NotificationEntry{}
	}
	writeJSON(w, http.StatusOK, notifications)
}

// ListTickets handles GET /portal/tickets.
func (h *Handler) ListTickets(w http.ResponseWriter, r *http.Request) {
	subID := subscriberIDFromCtx(r)
	if subID == 0 {
		writeJSON(w, http.StatusUnauthorized, errBody("ERR_UNAUTHORIZED", "missing subscriber context"))
		return
	}

	tickets, err := h.tickets.ListTickets(r.Context(), subID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("ERR_INTERNAL", "failed to fetch tickets"))
		return
	}
	if tickets == nil {
		tickets = []TicketEntry{}
	}
	writeJSON(w, http.StatusOK, tickets)
}

// CreateTicket handles POST /portal/tickets.
func (h *Handler) CreateTicket(w http.ResponseWriter, r *http.Request) {
	subID := subscriberIDFromCtx(r)
	if subID == 0 {
		writeJSON(w, http.StatusUnauthorized, errBody("ERR_UNAUTHORIZED", "missing subscriber context"))
		return
	}

	var req TicketCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("ERR_BAD_REQUEST", "invalid request body"))
		return
	}
	if req.Category == "" || req.Description == "" {
		writeJSON(w, http.StatusUnprocessableEntity, errBody("ERR_VALIDATION", "category and description required"))
		return
	}
	req.SubscriberID = subID

	ticket, err := h.tickets.CreateTicket(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody("ERR_INTERNAL", "create ticket failed"))
		return
	}
	writeJSON(w, http.StatusCreated, ticket)
}

// â”€â”€ Helpers â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€

func subscriberIDFromCtx(r *http.Request) int {
	return middleware.SubscriberIDFromContext(r.Context())
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func errBody(code, msg string) map[string]string {
	return map[string]string{"code": code, "message": msg}
}

// portalJWTSecret returns the JWT secret for portal tokens.
// In production this comes from secret manager; here env var is the fallback.
//
//nolint:unused // called during server startup outside of this file.
func portalJWTSecret() string {
	if s := os.Getenv("PORTAL_JWT_SECRET"); s != "" {
		return s
	}
	return fmt.Sprintf("%s_portal", os.Getenv("JWT_SECRET"))
}
