package fup

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

var (
	deadLetterQueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fup_dead_letter_queue_depth",
		Help: "Number of tasks in the dead-letter queue",
	})
)

// Alerter is the interface for PagerDuty / alerting integrations.
type Alerter interface {
	Trigger(event string, detail any)
}

// DeadCounter reports how many tasks on a queue have exhausted their
// retries. Satisfied by *jobqueue.Inspector.
//
// An interface rather than the concrete inspector so this package does not
// depend on the queue implementation — it did depend on Asynq's directly before the move,
// which is what made swapping the queue reach into a monitor that has
// nothing to do with how tasks are stored.
type DeadCounter interface {
	DeadCount(ctx context.Context, queue string) (int, error)
}

// DeadLetterMonitor polls the dead-letter count every 30s and fires an
// alert if any task has been abandoned.
//
// A rising count means work is being dropped: CoA commands that never
// reached a NAS, notifications never delivered. That is why it alerts
// rather than only exporting a metric.
//
// FR: FR-FUP-003 | DDS §5.3
type DeadLetterMonitor struct {
	counter  DeadCounter
	alerter  Alerter
	interval time.Duration
	// reminder bounds how often an unchanged, still-broken queue re-alerts.
	reminder time.Duration

	// Alert state, owned by Run's goroutine — checkOnce is not called
	// concurrently, so these need no lock.
	lastAlerted int       // the count at the last alert; 0 means "not alerting"
	lastAlertAt time.Time // when that alert fired
	now         func() time.Time
}

// DefaultDeadLetterInterval is how often the dead-letter count is polled.
const DefaultDeadLetterInterval = 30 * time.Second

// DefaultDeadLetterReminder bounds re-alerting on a queue that is still
// broken but no worse than it was.
//
// This exists because the monitor used to alert on every single poll while
// the count was above zero, which is not a decision anyone made so much as
// what the obvious loop does. On this deployment that produced 2,400
// identical alerts from two stuck tasks over about a week — roughly one
// every three minutes, indefinitely, saying nothing new each time.
//
// That is worse than not alerting. An alert that repeats forever is one
// people filter, and the filter catches the next real incident too. Hourly
// keeps a genuinely stuck queue visible without training anyone to ignore
// it.
const DefaultDeadLetterReminder = time.Hour

// NewDeadLetterMonitor constructs a DeadLetterMonitor.
func NewDeadLetterMonitor(counter DeadCounter, alerter Alerter) *DeadLetterMonitor {
	return &DeadLetterMonitor{
		counter:  counter,
		alerter:  alerter,
		interval: DefaultDeadLetterInterval,
		reminder: DefaultDeadLetterReminder,
		now:      time.Now,
	}
}

// SetInterval overrides the poll interval.
func (m *DeadLetterMonitor) SetInterval(d time.Duration) {
	m.interval = d
}

// SetReminderInterval overrides how often an unchanged, still-broken queue
// re-alerts.
func (m *DeadLetterMonitor) SetReminderInterval(d time.Duration) {
	m.reminder = d
}

// Run starts the monitoring loop. Blocks until ctx is cancelled.
func (m *DeadLetterMonitor) Run(ctx context.Context) {
	interval := m.interval
	if interval <= 0 {
		interval = DefaultDeadLetterInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.checkOnce(ctx); err != nil {
				log.Error().Err(err).Msg("dead_letter_monitor: queue info error")
			}
		}
	}
}

// checkOnce samples the dead-letter depth and alerts on a *change of state*
// rather than on every poll.
//
// Three things are worth alerting about, and repetition is not one of them:
//
//   - the queue just became non-empty (a new incident),
//   - it grew since the last alert (whatever is failing is still failing,
//     and producing new casualties rather than sitting on old ones),
//   - it has stayed broken for a reminder interval (so a stuck queue is not
//     silently forgotten after one page at 3am).
//
// A count that is unchanged and recent is deliberately silent. The metric
// (fup_dead_letter_queue_depth) is still exported every poll, so a
// dashboard and any Prometheus rule see the full picture — this only
// governs the paging path, where the signal-to-noise ratio is what decides
// whether anyone reads it.
func (m *DeadLetterMonitor) checkOnce(ctx context.Context) error {
	if m.counter == nil {
		return nil
	}
	dead, err := m.counter.DeadCount(ctx, QueueNetCommands)
	if err != nil {
		return fmt.Errorf("dead_letter_monitor: count dead tasks: %w", err)
	}
	deadLetterQueueDepth.Set(float64(dead))

	now := m.now
	if now == nil {
		now = time.Now
	}

	if dead == 0 {
		// Recovery. Logged rather than alerted — waking someone to tell them
		// a problem went away is its own kind of noise — but the state has
		// to reset, or a recurrence would be treated as a continuation and
		// stay silent until the reminder elapsed.
		if m.lastAlerted > 0 {
			log.Info().Msg("dead_letter_monitor: dead-letter queue is empty again")
			m.lastAlerted = 0
			m.lastAlertAt = time.Time{}
		}
		return nil
	}

	reminder := m.reminder
	if reminder <= 0 {
		reminder = DefaultDeadLetterReminder
	}

	newIncident := m.lastAlerted == 0
	gotWorse := dead > m.lastAlerted
	stale := !m.lastAlertAt.IsZero() && now().Sub(m.lastAlertAt) >= reminder

	if !newIncident && !gotWorse && !stale {
		// Still broken, no worse, and recently reported. The metric above
		// already carries this; another page would not.
		return nil
	}

	log.Warn().
		Int("archived_count", dead).
		Int("previously_alerted", m.lastAlerted).
		Bool("new_incident", newIncident).
		Msg("dead_letter_monitor: archived tasks detected")
	m.alerter.Trigger("dead_letter_queue_non_empty", dead)

	m.lastAlerted = dead
	m.lastAlertAt = now()
	return nil
}
