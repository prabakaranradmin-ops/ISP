// Package health implements the single-call subscriber diagnostic endpoint.
//
// FR: FR-OBS-004 | DDS §5.9
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// SubscriberRecord is the DB view of a subscriber.
type SubscriberRecord struct {
	ID             int
	Username       string
	Status         string
	WalletBalance  decimal.Decimal
	PlanExpiry     *time.Time
	DndOptOut      bool
	OpenTickets    int
	LastNotifEvent string
	LastNotifAt    *time.Time
}

// SessionSummary holds live session state from Redis.
type SessionSummary struct {
	SessionID    string `json:"session_id"`
	NasIP        string `json:"nas_ip"`
	AssignedIP   string `json:"assigned_ip"`
	BytesUsed    int64  `json:"bytes_used"`
	BytesTotal   int64  `json:"bytes_total"`
	PctUsed      int    `json:"pct_used"`
	SpeedProfile string `json:"speed_profile"`
	SessionAge   string `json:"session_age"`
}

// SubscriberHealth is the response payload for GET /subscribers/{id}/health.
type SubscriberHealth struct {
	SubscriberID  int             `json:"subscriber_id"`
	Username      string          `json:"username"`
	Status        string          `json:"status"`
	WalletBalance string          `json:"wallet_balance"`
	PlanExpiry    *time.Time      `json:"plan_expiry"`
	ActiveSession *SessionSummary `json:"active_session"`
	FupStatus     string          `json:"fup_status"` // below | warning | throttled
	LastCoaResult string          `json:"last_coa_result"`
	OpenTickets   int             `json:"open_tickets"`
}

// DBQuerier is the minimal DB interface for the health handler.
type DBQuerier interface {
	GetSubscriberWithMeta(ctx context.Context, subscriberID int) (*SubscriberRecord, error)
}

// RedisQuerier retrieves active session state from Redis.
type RedisQuerier interface {
	GetActiveSession(ctx context.Context, subscriberID int) (*SessionSummary, error)
}

// Handler serves the subscriber health endpoint.
type Handler struct {
	db    DBQuerier
	redis RedisQuerier
}

// NewHandler constructs a health Handler.
func NewHandler(db DBQuerier, redis RedisQuerier) *Handler {
	return &Handler{db: db, redis: redis}
}

// GetSubscriberHealth handles GET /subscribers/{id}/health.
// Fans out a DB query and Redis session fetch concurrently for â‰¤200ms p99 (NFR-PERF-002).
func (h *Handler) GetSubscriberHealth(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	subscriberID, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "invalid subscriber id", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Millisecond)
	defer cancel()

	var (
		sub     *SubscriberRecord
		session *SessionSummary
		subErr  error
		wg      sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		sub, subErr = h.db.GetSubscriberWithMeta(ctx, subscriberID)
	}()
	go func() {
		defer wg.Done()
		session, _ = h.redis.GetActiveSession(ctx, subscriberID)
	}()
	wg.Wait()

	if subErr != nil {
		log.Error().Err(subErr).Int("subscriber_id", subscriberID).Msg("health: db lookup failed")
		http.Error(w, "subscriber not found", http.StatusNotFound)
		return
	}

	fupStatus := "below"
	if session != nil {
		if session.PctUsed >= 100 {
			fupStatus = "throttled"
		} else if session.PctUsed >= 80 {
			fupStatus = "warning"
		}
	}

	resp := SubscriberHealth{
		SubscriberID:  sub.ID,
		Username:      sub.Username,
		Status:        sub.Status,
		WalletBalance: sub.WalletBalance.String(),
		PlanExpiry:    sub.PlanExpiry,
		ActiveSession: session,
		FupStatus:     fupStatus,
		LastCoaResult: "none",
		OpenTickets:   sub.OpenTickets,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp) //nolint:errcheck
}
