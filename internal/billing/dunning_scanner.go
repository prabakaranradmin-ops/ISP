package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// The dunning state machine in dunning.go was complete, correct and fully
// tested, and had no caller anywhere outside tests. Nothing advanced a
// subscriber through remind_7d → … → hard_suspended, so nobody was ever
// reminded to pay and nobody was ever suspended for not paying. This file is
// the missing half: the loop that decides who should move, moves them, and
// tells them.
//
// FR: FR-BIL-004, FR-NOTIF-001, FR-NOTIF-005 | DDS §5.4

const (
	// TaskTypeDunningNotice carries a dunning-stage notification.
	TaskTypeDunningNotice = "notif:dunning"

	// QueueNotifications matches the queue the radiusd worker pool consumes.
	QueueNotifications = "notifications"

	// TemplateDunningReminder is sent while the subscriber can still pay
	// without losing service (FR-NOTIF-001).
	TemplateDunningReminder = "TMPL-003" //nolint:gosec // template id, not a credential
	// TemplateSoftSuspended and TemplateHardSuspended are sent once service
	// has been restricted (FR-NOTIF-005). The traceability index gives them
	// separate ids because they are different messages: soft suspension is
	// still recoverable by paying, hard suspension has already cut the line.
	// Both stages previously sent TMPL-004, so a subscriber whose service had
	// actually been cut received the softer of the two warnings.
	TemplateSoftSuspended = "TMPL-004" //nolint:gosec // template id, not a credential
	TemplateHardSuspended = "TMPL-005" //nolint:gosec // template id, not a credential
	// TemplateServiceRestored tells a subscriber their service is back
	// (FR-NOTIF-006). Sent by the renewal scanner at the moment it actually
	// restores them, not by the payment webhook — see PaymentReceiptHandler.
	TemplateServiceRestored = "TMPL-006" //nolint:gosec // template id, not a credential
	// TemplatePaymentReceived acknowledges money arriving (FR-NOTIF-004). It
	// says nothing about service, because when it is sent nothing about
	// service has changed yet.
	TemplatePaymentReceived = "TMPL-007" //nolint:gosec // template id, not a credential

	// dunningScanInterval is hourly because every edge in this machine is
	// measured in days. Scanning faster would add load without ever finding a
	// transition a day-granular rule had not already found.
	dunningScanInterval = time.Hour

	// GracePeriodDays and SoftSuspendDays are the shipped defaults, kept for
	// callers that describe the ladder (the demo seeder). The live values are
	// DunningPolicy, read from billing_settings on every scan (FR-BIL-017).
	// Every stage is measured from plan_expiry rather than from the previous
	// transition, which is what lets the ladder be derived from one timestamp.
	GracePeriodDays = 3
	SoftSuspendDays = 3
)

var (
	dunningTransitioned = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "billing_dunning_transitions_total",
		Help: "Dunning stage advances, by the stage entered",
	}, []string{"to_state"})
	dunningNoticeEnqueued = promauto.NewCounter(prometheus.CounterOpts{
		Name: "billing_dunning_notice_enqueued_total",
		Help: "Dunning notifications enqueued",
	})
	// A failure here is somebody suspended for not paying who is still
	// online, so it is counted where an alert can see it.
	dunningCutoffQueued = promauto.NewCounter(prometheus.CounterOpts{
		Name: "billing_dunning_cutoff_queued_total",
		Help: "Hard-suspension disconnects queued",
	})
	dunningCutoffFailed = promauto.NewCounter(prometheus.CounterOpts{
		Name: "billing_dunning_cutoff_failed_total",
		Help: "Hard-suspension disconnects that could not be queued",
	})
)

// DunningCandidate is a subscriber the scanner may need to advance.
type DunningCandidate struct {
	SubscriberID int
	Username     string
	MobileNumber string
	State        DunningState
	PlanExpiry   time.Time
	// GraceUntil is a per-subscriber grace extension, nil when none is set
	// (FR-BIL-016).
	GraceUntil *time.Time
}

// DunningScanQuerier is the database surface the scanner needs.
type DunningScanQuerier interface {
	DunningQuerier
	// ListDunningCandidates returns subscribers expiring within windowDays
	// (the first reminder's lead time) or already mid-dunning, or holding a
	// grace extension.
	ListDunningCandidates(ctx context.Context, windowDays int) ([]DunningCandidate, error)
	// DunningPolicy reads the configured ladder timing (FR-BIL-017).
	DunningPolicy(ctx context.Context) (DunningPolicy, error)
}

// DunningNoticePayload is the task payload for a dunning notification.
type DunningNoticePayload struct {
	SubscriberID int          `json:"subscriber_id"`
	Username     string       `json:"username"`
	State        DunningState `json:"state"`
	TemplateID   string       `json:"template_id"`
	DaysOverdue  int          `json:"days_overdue"`
}

// NextDunningState returns the stage a subscriber belongs in given their plan
// expiry, or the current state if no change is due.
//
// This is the single place the ladder is defined. The SQL that finds
// candidates deliberately filters broadly and defers to this function, because
// a rule split between a query predicate and Go code is a rule that will drift.
//
// Paid-up subscribers walk back to active from anywhere: renewing is the one
// event that should undo any amount of dunning, and expressing that here keeps
// the restore path from needing its own copy of the ladder.
func NextDunningState(current DunningState, planExpiry time.Time, now time.Time) DunningState {
	return NextDunningStateWith(DefaultDunningPolicy(), current, planExpiry, now, nil)
}

// TemplateForDunningState returns the notification template for a stage, and
// false when entering that stage warrants no message.
//
// Entering remind_7d/3d/1d or grace_period still leaves the subscriber online,
// so those are reminders. soft_suspended and hard_suspended mean service has
// actually been restricted, which is a different message. Returning to active
// is acknowledged on the payment path, not here, so it is silent.
func TemplateForDunningState(s DunningState) (string, bool) {
	switch s {
	case DunningRemind7d, DunningRemind3d, DunningRemind1d, DunningGracePeriod:
		return TemplateDunningReminder, true
	case DunningSoftSuspended:
		return TemplateSoftSuspended, true
	case DunningHardSuspended:
		return TemplateHardSuspended, true
	default:
		return "", false
	}
}

// DunningScanner advances subscribers through the dunning ladder and enqueues
// the notification for each stage entered.
type DunningScanner struct {
	db       DunningScanQuerier
	tasks    TaskEnqueuer
	subCache CacheInvalidator
	now      func() time.Time // injectable for tests
}

// NewDunningScanner constructs a DunningScanner. A nil client leaves the
// queue unset, rather than storing a typed nil the nil checks would miss.
func NewDunningScanner(db DunningScanQuerier, client *jobqueue.Client) *DunningScanner {
	s := &DunningScanner{db: db, now: time.Now}
	if client != nil {
		s.tasks = client
	}
	return s
}

// SetTaskEnqueuer replaces the queue the notices and disconnects go to.
func (s *DunningScanner) SetTaskEnqueuer(t TaskEnqueuer) { s.tasks = t }

// SetSubscriberCache attaches the RADIUS auth cache, so a hard suspension is
// forgotten there before the line is cut. See cutOff.
func (s *DunningScanner) SetSubscriberCache(c CacheInvalidator) { s.subCache = c }

// Run scans hourly until ctx is cancelled. It scans once immediately: after a
// deployment that has never run dunning, waiting an hour to start collecting
// serves nobody.
func (s *DunningScanner) Run(ctx context.Context) {
	if err := s.Scan(ctx); err != nil {
		log.Error().Err(err).Msg("billing: dunning scan error")
	}
	ticker := time.NewTicker(dunningScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Scan(ctx); err != nil {
				log.Error().Err(err).Msg("billing: dunning scan error")
			}
		}
	}
}

// Scan performs one pass. Exported so a test can drive a single pass without
// waiting on the ticker.
func (s *DunningScanner) Scan(ctx context.Context) error {
	// Name the scanner as the actor for migration 031's status-capture
	// trigger. Without this every automatic suspension is recorded as
	// "unknown", and a churn report cannot separate accounts the system
	// suspended for non-payment from ones a person acted on.
	ctx = middleware.WithSubject(ctx, "system:dunning-scanner")

	// The policy is read each pass, so a change on the settings screen
	// applies from the next hourly run without a restart. If it cannot be
	// read, the shipped default is used rather than skipping the pass.
	policy, err := s.db.DunningPolicy(ctx)
	if err != nil {
		log.Error().Err(err).Msg("billing: dunning policy unreadable; using the default ladder")
		policy = DefaultDunningPolicy()
	}

	candidates, err := s.db.ListDunningCandidates(ctx, policy.RemindFirstDays)
	if err != nil {
		return fmt.Errorf("dunning scan: list candidates: %w", err)
	}

	now := s.now()
	for _, c := range candidates {
		target := NextDunningStateWith(policy, c.State, c.PlanExpiry, now, c.GraceUntil)
		if target == c.State {
			continue
		}
		if err := s.advance(ctx, c, target, now); err != nil {
			// One subscriber's failure must not stop the rest of the run: a
			// single bad row would otherwise stall collections for everyone.
			log.Error().Err(err).
				Int("subscriber_id", c.SubscriberID).
				Str("from", string(c.State)).
				Str("to", string(target)).
				Msg("billing: dunning transition failed")
		}
	}
	return nil
}

// advance walks the subscriber from their current stage to the target one
// stage at a time, then sends a single notification for where they landed.
//
// Stepping through intermediate stages keeps every change going through
// TransitionDunning, so the state machine stays the only thing that decides
// what is legal. Notifying once at the end matters on the first run after this
// scanner is deployed: subscribers whose expiry passed weeks ago while nothing
// was advancing them will jump several stages at once, and sending a message
// per stage would deliver a burst of four to someone who should receive one.
func (s *DunningScanner) advance(ctx context.Context, c DunningCandidate, target DunningState, now time.Time) error {
	const maxSteps = 8 // the ladder is 6 long; this only bounds a cycle that cannot occur

	state := c.State
	for step := 0; state != target && step < maxSteps; step++ {
		next, ok := stepToward(state, target)
		if !ok {
			return fmt.Errorf("no path from %s to %s", state, target)
		}
		if err := TransitionDunning(ctx, s.db, c.SubscriberID, next); err != nil {
			return err
		}
		dunningTransitioned.WithLabelValues(string(next)).Inc()
		state = next
	}
	if state != target {
		return fmt.Errorf("did not reach %s from %s within %d steps", target, c.State, maxSteps)
	}

	// Only the move into hard suspension, not every pass over someone
	// already there.
	if target == DunningHardSuspended && c.State != DunningHardSuspended {
		s.cutOff(ctx, c)
	}

	templateID, notify := TemplateForDunningState(target)
	if !notify || s.tasks == nil {
		return nil
	}

	daysOverdue := int(now.Sub(c.PlanExpiry).Hours() / 24)
	if daysOverdue < 0 {
		daysOverdue = 0
	}
	payload, err := json.Marshal(DunningNoticePayload{
		SubscriberID: c.SubscriberID,
		Username:     c.Username,
		State:        target,
		TemplateID:   templateID,
		DaysOverdue:  daysOverdue,
	})
	if err != nil {
		return fmt.Errorf("marshal dunning notice: %w", err)
	}

	// The task id makes the notice idempotent per subscriber per stage, so a
	// scanner restart, an overlapping run or a redelivery cannot send the same
	// subscriber the same warning twice.
	task := jobqueue.NewTask(TaskTypeDunningNotice, payload,
		jobqueue.Queue(QueueNotifications),
		jobqueue.TaskID(DunningNoticeTaskID(c.SubscriberID, target, c.PlanExpiry)),
		jobqueue.MaxRetry(3),
		jobqueue.Retention(24*time.Hour))

	if _, err := s.tasks.EnqueueContext(ctx, task); err != nil {
		if errors.Is(err, jobqueue.ErrTaskIDConflict) {
			return nil // already notified for this stage and billing cycle
		}
		return fmt.Errorf("enqueue dunning notice: %w", err)
	}
	dunningNoticeEnqueued.Inc()
	return nil
}

// stepToward returns the next single stage between current and target.
func stepToward(current, target DunningState) (DunningState, bool) {
	// Restoring is one hop from wherever they are.
	if target == DunningActive {
		return DunningActive, true
	}
	// So is a grace extension lifting a suspension (FR-BIL-016).
	if target == DunningGracePeriod && (current == DunningSoftSuspended || current == DunningHardSuspended) {
		return DunningGracePeriod, true
	}
	ladder := []DunningState{
		DunningActive, DunningRemind7d, DunningRemind3d, DunningRemind1d,
		DunningGracePeriod, DunningSoftSuspended, DunningHardSuspended,
	}
	ci, ti := -1, -1
	for i, s := range ladder {
		if s == current {
			ci = i
		}
		if s == target {
			ti = i
		}
	}
	if ci < 0 || ti < 0 || ti <= ci {
		return "", false
	}
	return ladder[ci+1], true
}

// DunningNoticeTaskID is the idempotency key for a dunning notice: one per
// subscriber, per stage, per billing cycle. Including the expiry means a
// subscriber who renews and later lapses again is warned afresh rather than
// being silently suppressed by last cycle's task id.
func DunningNoticeTaskID(subscriberID int, state DunningState, planExpiry time.Time) string {
	return fmt.Sprintf("dunning-%d-%s-%d", subscriberID, state, planExpiry.Unix())
}

// DunningCutoffTaskID keys the hard-suspension disconnect per subscriber per
// billing cycle, like the notice, so an overlapping pass cannot queue two and
// a later lapse is not suppressed by an earlier one's id.
func DunningCutoffTaskID(subscriberID int, planExpiry time.Time) string {
	return fmt.Sprintf("dunning-cutoff-%d-%d", subscriberID, planExpiry.Unix())
}

// cutOff ends a newly hard-suspended subscriber's live session.
//
// The dunning screen says hard suspension means "Disconnected", and RADIUS
// refuses the next login, but until 2026-11-28 nothing ended the login they
// already had: the status changed, the notice went, and a session that was
// up stayed up for as long as the line held — on always-on PPPoE, days or
// weeks of service for somebody suspended for not paying.
//
// The cache goes first. A disconnected router reconnects within seconds and
// radiusd answers from an auth cache that holds a subscriber for a minute;
// without the invalidation the reconnect is accepted on the old status and
// the session is simply back.
//
// The disconnect names the subscriber, not a session: the PoD handler looks
// up whatever session is live when it runs and skips without retrying when
// there is none. Failures are logged and counted, never returned — the
// suspension has committed and the notice should still go.
func (s *DunningScanner) cutOff(ctx context.Context, c DunningCandidate) {
	if s.subCache != nil {
		if err := s.subCache.InvalidateSubscriber(ctx, c.Username); err != nil {
			log.Error().Err(err).Int("subscriber_id", c.SubscriberID).
				Msg("billing: auth-cache invalidation failed on hard suspension")
		}
	}
	if s.tasks == nil {
		return
	}
	payload, err := json.Marshal(fup.PoDPayload{SubscriberID: c.SubscriberID})
	if err != nil {
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).Msg("billing: disconnect payload")
		return
	}
	task := jobqueue.NewTask(fup.TaskTypePoD, payload,
		jobqueue.Queue(fup.QueueNetCommands),
		jobqueue.TaskID(DunningCutoffTaskID(c.SubscriberID, c.PlanExpiry)),
		jobqueue.MaxRetry(5),
		jobqueue.Retention(24*time.Hour))
	if _, err := s.tasks.EnqueueContext(ctx, task); err != nil && !errors.Is(err, jobqueue.ErrTaskIDConflict) {
		dunningCutoffFailed.Inc()
		log.Error().Err(err).Int("subscriber_id", c.SubscriberID).
			Msg("billing: could not queue the hard-suspension disconnect; the live session stays up")
		return
	}
	dunningCutoffQueued.Inc()
}
