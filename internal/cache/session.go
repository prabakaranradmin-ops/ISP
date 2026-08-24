// Package cache holds the live session state that the RADIUS daemon writes
// on accounting and that the health endpoint and subscriber portal read.
//
// Live session state lives in its own small table rather than alongside
// subscriber_session_history because it is read on every health check and
// dashboard load but is worthless once the session ends — this was a Redis
// cache for exactly that reason before this stack became a single-machine
// native install (see internal/localcache's package doc for why Redis's
// multi-process visibility is no longer needed). It is still treated as a
// cache throughout: every reader degrades to "no active session" rather than
// erroring when a row is stale or absent, and a failure to write it never
// fails the accounting request that triggered it.
//
// DDS §5.9 | IDD §8.4
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
	"github.com/shopspring/decimal"
)

// SessionTTL bounds how long a session record is treated as live without an
// accounting update. Interim-Updates arrive every 5 minutes, so a record
// older than this belongs to a session whose Accounting-Stop was lost.
const SessionTTL = 30 * time.Minute

// Session is the stored representation of a live RADIUS session.
type Session struct {
	SessionID    string
	SubscriberID int
	NasIP        string
	AssignedIP   string
	BytesIn      int64
	BytesOut     int64
	BytesTotal   int64 // plan quota; 0 = unlimited
	SpeedProfile string
	FUPThrottled bool
	StartedAt    time.Time
}

// BytesUsed is the combined upstream and downstream usage.
func (s *Session) BytesUsed() int64 { return s.BytesIn + s.BytesOut }

// PctUsed is the percentage of quota consumed, 0 for an unlimited plan.
//
// Computed with decimal rather than float division: this value drives the FUP
// banding, and a subscriber must not be reported as throttled because of a
// floating-point rounding artefact at the boundary.
func (s *Session) PctUsed() int {
	if s.BytesTotal <= 0 {
		return 0
	}
	return int(decimal.NewFromInt(s.BytesUsed()).
		Mul(decimal.NewFromInt(100)).
		Div(decimal.NewFromInt(s.BytesTotal)).
		IntPart())
}

// SessionStore reads and writes live session state in Postgres.
type SessionStore struct {
	pool *pgxpool.Pool
}

// NewSessionStore constructs a SessionStore.
func NewSessionStore(pool *pgxpool.Pool) *SessionStore {
	return &SessionStore{pool: pool}
}

var (
	_ health.RedisQuerier      = (*SessionStore)(nil)
	_ api.SessionReader        = (*SessionStore)(nil)
	_ radius.LiveSessionWriter = (*SessionStore)(nil)
)

// PortalView adapts SessionStore to the portal's session interface.
//
// health.RedisQuerier and portal.PortalSessionQuerier both declare
// GetActiveSession but return different types, which no single Go type can
// satisfy — hence the adapter rather than a second method name.
type PortalView struct{ store *SessionStore }

var _ portal.PortalSessionQuerier = (*PortalView)(nil)

// Portal returns the portal-facing view of live session state.
func (s *SessionStore) Portal() *PortalView { return &PortalView{store: s} }

// GetActiveSession implements portal.PortalSessionQuerier.
func (p *PortalView) GetActiveSession(ctx context.Context, subscriberID int) (*portal.ActiveSession, error) {
	return p.store.PortalSession(ctx, subscriberID)
}

// Put stores a session at Accounting-Start (or overwrites a stale one for the
// same subscriber that never got a matching Accounting-Stop). Takes
// radius.LiveSession, not the richer Session this package reads back — the
// byte counters and quota Session additionally carries are populated by
// UpdateOctets and the plan lookup respectively, not known yet at
// Accounting-Start.
func (s *SessionStore) Put(ctx context.Context, sess radius.LiveSession) error {
	const q = `
		INSERT INTO live_sessions
			(subscriber_id, session_id, nas_ip, assigned_ip, bytes_in, bytes_out,
			 bytes_total, speed_profile, fup_throttled, started_at, updated_at)
		VALUES ($1, $2, $3, $4, 0, 0, $5, $6, $7, now(), now())
		ON CONFLICT (subscriber_id) DO UPDATE SET
			session_id = EXCLUDED.session_id, nas_ip = EXCLUDED.nas_ip,
			assigned_ip = EXCLUDED.assigned_ip, bytes_in = 0, bytes_out = 0,
			bytes_total = EXCLUDED.bytes_total,
			speed_profile = EXCLUDED.speed_profile, fup_throttled = EXCLUDED.fup_throttled,
			started_at = EXCLUDED.started_at, updated_at = now()`
	if _, err := s.pool.Exec(ctx, q, sess.SubscriberID, sess.SessionID, sess.NasIP,
		sess.AssignedIP, sess.BytesTotal, sess.SpeedProfile, sess.FUPThrottled); err != nil {
		return fmt.Errorf("cache: store session for subscriber %d: %w", sess.SubscriberID, err)
	}
	return nil
}

// UpdateOctets applies an Accounting-Interim-Update's byte counters, matched
// by session_id rather than subscriber_id: the RADIUS daemon's accounting
// path does not re-resolve which subscriber owns an interim record (Put
// already recorded that at Accounting-Start), so matching here in SQL avoids
// carrying that correlation in Go.
func (s *SessionStore) UpdateOctets(ctx context.Context, sessionID string, inputOctets, outputOctets int64) error {
	const q = `
		UPDATE live_sessions
		SET bytes_in = $2, bytes_out = $3, bytes_total = bytes_total, updated_at = now()
		WHERE session_id = $1`
	if _, err := s.pool.Exec(ctx, q, sessionID, inputOctets, outputOctets); err != nil {
		return fmt.Errorf("cache: update session octets for %q: %w", sessionID, err)
	}
	return nil
}

// Delete removes a session by subscriber id — the public, subscriber-facing
// removal path (e.g. a manual disconnect from the operations console).
func (s *SessionStore) Delete(ctx context.Context, subscriberID int) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM live_sessions WHERE subscriber_id = $1`, subscriberID); err != nil {
		return fmt.Errorf("cache: delete session for subscriber %d: %w", subscriberID, err)
	}
	return nil
}

// DeleteBySessionID removes a session at Accounting-Stop, matched the same
// way UpdateOctets is — see its comment.
func (s *SessionStore) DeleteBySessionID(ctx context.Context, sessionID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM live_sessions WHERE session_id = $1`, sessionID); err != nil {
		return fmt.Errorf("cache: delete session %q: %w", sessionID, err)
	}
	return nil
}

// get loads the raw session, returning (nil, nil) when absent or stale.
// Staleness is checked here rather than relying solely on the periodic
// sweep (see NewStalenessSweeper): a row can be up to one sweep interval
// past SessionTTL before the sweep reaches it, and a read must never report
// a subscriber online past the TTL a stale accounting stream implies.
func (s *SessionStore) get(ctx context.Context, subscriberID int) (*Session, error) {
	const q = `
		SELECT session_id, subscriber_id, nas_ip, assigned_ip, bytes_in, bytes_out,
		       bytes_total, speed_profile, fup_throttled, started_at
		FROM live_sessions
		WHERE subscriber_id = $1 AND updated_at > now() - ($2 * interval '1 second')`
	var sess Session
	err := s.pool.QueryRow(ctx, q, subscriberID, SessionTTL.Seconds()).Scan(
		&sess.SessionID, &sess.SubscriberID, &sess.NasIP, &sess.AssignedIP,
		&sess.BytesIn, &sess.BytesOut, &sess.BytesTotal, &sess.SpeedProfile,
		&sess.FUPThrottled, &sess.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil // offline, which is a normal state rather than an error
	}
	if err != nil {
		return nil, fmt.Errorf("cache: read session for subscriber %d: %w", subscriberID, err)
	}
	return &sess, nil
}

// Get returns the live session for a subscriber, or nil when offline.
func (s *SessionStore) Get(ctx context.Context, subscriberID int) (*Session, error) {
	return s.get(ctx, subscriberID)
}

// GetActiveSession implements health.RedisQuerier.
func (s *SessionStore) GetActiveSession(ctx context.Context, subscriberID int) (*health.SessionSummary, error) {
	sess, err := s.get(ctx, subscriberID)
	if err != nil || sess == nil {
		return nil, err
	}
	return &health.SessionSummary{
		SessionID:    sess.SessionID,
		NasIP:        sess.NasIP,
		AssignedIP:   sess.AssignedIP,
		BytesUsed:    sess.BytesUsed(),
		BytesTotal:   sess.BytesTotal,
		PctUsed:      sess.PctUsed(),
		SpeedProfile: sess.SpeedProfile,
		SessionAge:   formatAge(time.Since(sess.StartedAt)),
	}, nil
}

// PortalSession adapts the stored session to the portal's view.
//
// The portal reports usage in GB because that is the unit a subscriber's plan is
// sold in; the health endpoint keeps raw octets for support diagnostics.
func (s *SessionStore) PortalSession(ctx context.Context, subscriberID int) (*portal.ActiveSession, error) {
	sess, err := s.get(ctx, subscriberID)
	if err != nil || sess == nil {
		return nil, err
	}
	const bytesPerGB = 1024 * 1024 * 1024
	gb := func(b int64) decimal.Decimal {
		return decimal.NewFromInt(b).Div(decimal.NewFromInt(bytesPerGB)).Round(2)
	}
	pct := 0.0
	if sess.BytesTotal > 0 {
		pct, _ = decimal.NewFromInt(sess.BytesUsed()).
			Mul(decimal.NewFromInt(100)).
			Div(decimal.NewFromInt(sess.BytesTotal)).
			Round(2).Float64()
	}
	return &portal.ActiveSession{
		SessionID:    sess.SessionID,
		NASIP:        sess.NasIP,
		AssignedIP:   sess.AssignedIP,
		BytesIn:      sess.BytesIn,
		BytesOut:     sess.BytesOut,
		GBUsed:       gb(sess.BytesUsed()),
		GBIncluded:   gb(sess.BytesTotal),
		PctUsed:      pct,
		FUPThrottled: sess.FUPThrottled,
		StartedAt:    sess.StartedAt,
	}, nil
}

// defaultSweepInterval bounds how long a stale row can linger before
// StalenessSweeper reclaims it. Reads already filter on updated_at
// themselves (see get), so this only bounds table growth, not correctness.
const defaultSweepInterval = 10 * time.Minute

// StalenessSweeper deletes live_sessions rows whose Accounting-Stop never
// arrived — a crashed NAS, a lost packet, or a subscriber's connection
// dropping mid-session all leave a row nothing will ever delete otherwise.
type StalenessSweeper struct {
	pool     *pgxpool.Pool
	interval time.Duration
}

// NewStalenessSweeper constructs a StalenessSweeper. A zero or negative
// interval uses defaultSweepInterval.
func NewStalenessSweeper(pool *pgxpool.Pool, interval time.Duration) *StalenessSweeper {
	if interval <= 0 {
		interval = defaultSweepInterval
	}
	return &StalenessSweeper{pool: pool, interval: interval}
}

// Run sweeps on an interval until ctx is cancelled.
func (s *StalenessSweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = s.pool.Exec(ctx,
				`DELETE FROM live_sessions WHERE updated_at < now() - ($1 * interval '1 second')`,
				SessionTTL.Seconds())
		}
	}
}

// formatAge renders a session duration as a compact human string.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}
