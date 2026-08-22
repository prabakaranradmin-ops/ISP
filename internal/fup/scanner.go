// Package fup implements the FUP scanner goroutine, CoA sender, and
// dead-letter monitor for the ISP BSS/OSS platform.
//
// FR: FR-FUP-001..005 | DDS §5.3
package fup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

var (
	fupBreachDetected = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fup_breach_detected_total",
		Help: "Number of FUP thresholds breached",
	})
	coaEnqueued = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fup_coa_enqueued_total",
		Help: "Number of CoA tasks enqueued",
	})
	fupWarningEnqueued = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fup_warning_enqueued_total",
		Help: "Number of 80% FUP warning notifications enqueued",
	})
	fupThrottledEnqueued = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fup_throttled_notification_enqueued_total",
		Help: "Number of FUP throttle-applied notifications enqueued",
	})
	speedOverrideExpired = promauto.NewCounter(prometheus.CounterOpts{
		Name: "speed_override_expired_total",
		Help: "Number of owner-triggered speed overrides auto-reverted on expiry",
	})
)

const (
	TaskTypeCoA          = "network:coa_send"
	TaskTypePoD          = "network:pod_send"
	TaskTypeFUPWarning   = "notif:fup_warning"
	TaskTypeFUPThrottled = "notif:fup_throttled"
	scanInterval         = 10 * time.Second
	FUPWarningPct        = 80
	// QueueNetCommands and QueueNotifications are exported so callers outside
	// this package (the admin API, enqueuing a session-control task) use the
	// exact same queue names the workers here listen on.
	QueueNetCommands   = "network_commands"
	QueueNotifications = "notifications"
)

// SessionStats holds aggregated usage for an active session.
type SessionStats struct {
	SubscriberID int
	Username     string
	NasIP        string
	FUPThreshold int64 // bytes; 0 = unlimited
	BytesUsed    int64
	FUPActive    bool
	// FUPThrottle is the plan's post-breach rate limit ("10M/10M"), empty
	// when the plan sets none. Carried on the session rather than looked up
	// again at notification time so the subscriber is told the speed that
	// was actually applied to them, not whatever the plan says by the time
	// the queued notification runs.
	FUPThrottle string
}

// FUPQuerier is the DB interface for the scanner.
type FUPQuerier interface {
	GetActiveSessionsAboveFUP(ctx context.Context) ([]SessionStats, error)
	GetSessionsAtWarning(ctx context.Context, pct int) ([]SessionStats, error)
	SetFUPActive(ctx context.Context, subscriberID int, active bool) error
	// ListExpiredSpeedOverrides and ClearSpeedOverride back the auto-revert
	// side of the owner's temporary speed override: this scanner's existing
	// tick is reused to sweep for expiries rather than standing up a second
	// ticker/goroutine for one more periodic check.
	ListExpiredSpeedOverrides(ctx context.Context) ([]int, error)
	ClearSpeedOverride(ctx context.Context, subscriberID int) error
}

// Scanner polls active sessions every 10s and enqueues CoA tasks for FUP breaches.
//
// FR: FR-FUP-001 | DDS §5.3
type Scanner struct {
	db     FUPQuerier
	client *jobqueue.Client
}

// NewScanner constructs a FUP Scanner.
func NewScanner(db FUPQuerier, client *jobqueue.Client) *Scanner {
	return &Scanner{db: db, client: client}
}

// Run starts the FUP scanning loop. Blocks until ctx is cancelled.
func (s *Scanner) Run(ctx context.Context) {
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.scan(ctx); err != nil {
				log.Error().Err(err).Msg("fup: scan error")
			}
		}
	}
}

// ScanOnce performs a single scan pass — the same work one tick of Run does.
//
// Exported so a caller that drives its own schedule, or an integration test
// standing up a real database, can exercise the scanner without waiting out a
// 10-second ticker or reaching into the package's internals.
func (s *Scanner) ScanOnce(ctx context.Context) error { return s.scan(ctx) }

func (s *Scanner) scan(ctx context.Context) error {
	if err := s.scanBreaches(ctx); err != nil {
		return err
	}
	if err := s.scanExpiredOverrides(ctx); err != nil {
		return err
	}
	return s.scanWarnings(ctx)
}

// scanExpiredOverrides reverts every owner-triggered speed override whose
// time-box has passed. A subscriber currently offline still gets cleared in
// the database — enqueueing a CoA for them is a harmless no-op that fails
// after retries rather than something worth skipping, and it means the
// override is already gone by the time they next connect.
func (s *Scanner) scanExpiredOverrides(ctx context.Context) error {
	ids, err := s.db.ListExpiredSpeedOverrides(ctx)
	if err != nil {
		return fmt.Errorf("fup: list expired speed overrides: %w", err)
	}
	for _, id := range ids {
		if err := s.db.ClearSpeedOverride(ctx, id); err != nil {
			log.Error().Err(err).Int("sub_id", id).Msg("fup: clear expired speed override failed")
			continue
		}
		speedOverrideExpired.Inc()

		payload, err := json.Marshal(CoAPayload{SubscriberID: id})
		if err != nil {
			log.Error().Err(err).Int("sub_id", id).Msg("fup: marshal revert-CoA payload failed")
			continue
		}
		task := jobqueue.NewTask(TaskTypeCoA, payload,
			jobqueue.Queue(QueueNetCommands), jobqueue.MaxRetry(5), jobqueue.Retention(24*time.Hour))
		if _, err := s.client.EnqueueContext(ctx, task); err != nil {
			log.Error().Err(err).Int("sub_id", id).Msg("fup: enqueue revert CoA for expired override failed")
		}
	}
	return nil
}

// scanWarnings enqueues an 80%-of-quota warning notification for each session
// approaching its FUP threshold.
//
// FR: FR-FUP-004 | DDS §5.3
func (s *Scanner) scanWarnings(ctx context.Context) error {
	sessions, err := s.db.GetSessionsAtWarning(ctx, FUPWarningPct)
	if err != nil {
		return fmt.Errorf("fup: get warning sessions: %w", err)
	}
	for _, sess := range sessions {
		if sess.FUPThreshold == 0 || sess.FUPActive {
			continue // unlimited, or already throttled and past warning stage
		}
		payload := []byte(fmt.Sprintf(`{"subscriber_id":%d,"username":%q,"pct_used":%d}`,
			sess.SubscriberID, sess.Username, UsagePct(sess.BytesUsed, sess.FUPThreshold)))
		task := jobqueue.NewTask(TaskTypeFUPWarning, payload,
			jobqueue.Queue(QueueNotifications),
			jobqueue.TaskID(WarningTaskID(sess.SubscriberID, sess.FUPThreshold)),
			jobqueue.MaxRetry(3),
			jobqueue.Retention(24*time.Hour))
		if _, err := s.client.EnqueueContext(ctx, task); err != nil {
			// A conflict means this subscriber was already warned for this quota
			// cycle — that is the idempotency guarantee working, not a failure.
			if errors.Is(err, jobqueue.ErrTaskIDConflict) {
				continue
			}
			return fmt.Errorf("fup: enqueue warning for sub %d: %w", sess.SubscriberID, err)
		}
		fupWarningEnqueued.Inc()
	}
	return nil
}

// WarningTaskID returns the task ID that makes an 80% warning idempotent
// for a given subscriber and quota cycle.
func WarningTaskID(subscriberID int, threshold int64) string {
	return fmt.Sprintf("fupwarn-%d-%d", subscriberID, threshold)
}

func (s *Scanner) scanBreaches(ctx context.Context) error {
	sessions, err := s.db.GetActiveSessionsAboveFUP(ctx)
	if err != nil {
		return fmt.Errorf("fup: get sessions: %w", err)
	}
	for _, sess := range sessions {
		if sess.FUPThreshold == 0 || sess.FUPActive {
			continue // unlimited or already throttled
		}
		if sess.BytesUsed >= sess.FUPThreshold {
			fupBreachDetected.Inc()
			if err := s.db.SetFUPActive(ctx, sess.SubscriberID, true); err != nil {
				log.Error().Err(err).Int("sub_id", sess.SubscriberID).Msg("fup: set fup_active failed")
				continue
			}
			coaPayload := []byte(fmt.Sprintf(`{"subscriber_id":%d,"nas_ip":"%s"}`,
				sess.SubscriberID, sess.NasIP))
			task := jobqueue.NewTask(TaskTypeCoA, coaPayload,
				jobqueue.Queue(QueueNetCommands),
				jobqueue.MaxRetry(5),
				jobqueue.Retention(24*time.Hour))
			if _, err := s.client.EnqueueContext(ctx, task); err != nil {
				return fmt.Errorf("fup: enqueue CoA for sub %d: %w", sess.SubscriberID, err)
			}
			coaEnqueued.Inc()

			// Tell the subscriber their speed dropped (FR-NOTIF-003,
			// FR-FUP-005). Enqueued after the CoA rather than before, so a
			// failure to actually apply the throttle does not first
			// announce one that never happened.
			//
			// A failure here is logged, not returned: the throttle itself
			// has already been applied and recorded, and abandoning the
			// scan would leave every later breached session in this pass
			// unthrottled over an undelivered message.
			if err := s.enqueueThrottledNotice(ctx, sess); err != nil {
				log.Error().Err(err).Int("sub_id", sess.SubscriberID).
					Msg("fup: enqueue throttle notification failed; throttle itself was applied")
			}
		}
	}
	return nil
}

// enqueueThrottledNotice queues the throttle-applied notification for one
// session.
func (s *Scanner) enqueueThrottledNotice(ctx context.Context, sess SessionStats) error {
	payload, err := json.Marshal(ThrottledPayload{
		SubscriberID:  sess.SubscriberID,
		Username:      sess.Username,
		ThrottleSpeed: sess.FUPThrottle,
	})
	if err != nil {
		return fmt.Errorf("marshal throttled payload: %w", err)
	}
	task := jobqueue.NewTask(TaskTypeFUPThrottled, payload,
		jobqueue.Queue(QueueNotifications),
		jobqueue.TaskID(ThrottledTaskID(sess.SubscriberID, sess.FUPThreshold)),
		jobqueue.MaxRetry(3),
		jobqueue.Retention(24*time.Hour))
	if _, err := s.client.EnqueueContext(ctx, task); err != nil {
		// Already notified for this quota cycle - the idempotency
		// guarantee working, not a failure.
		if errors.Is(err, jobqueue.ErrTaskIDConflict) {
			return nil
		}
		return err
	}
	fupThrottledEnqueued.Inc()
	return nil
}

// UsagePct computes the percentage of FUP threshold consumed.
func UsagePct(bytesUsed, threshold int64) int {
	if threshold == 0 {
		return 0
	}
	return int(decimal.NewFromInt(bytesUsed).
		Mul(decimal.NewFromInt(100)).
		Div(decimal.NewFromInt(threshold)).
		IntPart())
}
