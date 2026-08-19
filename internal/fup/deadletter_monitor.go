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
}

// DefaultDeadLetterInterval is how often the dead-letter count is polled.
const DefaultDeadLetterInterval = 30 * time.Second

// NewDeadLetterMonitor constructs a DeadLetterMonitor.
func NewDeadLetterMonitor(counter DeadCounter, alerter Alerter) *DeadLetterMonitor {
	return &DeadLetterMonitor{counter: counter, alerter: alerter, interval: DefaultDeadLetterInterval}
}

// SetInterval overrides the poll interval.
func (m *DeadLetterMonitor) SetInterval(d time.Duration) {
	m.interval = d
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

// checkOnce samples the dead-letter depth and alerts if any task has been
// abandoned.
func (m *DeadLetterMonitor) checkOnce(ctx context.Context) error {
	if m.counter == nil {
		return nil
	}
	dead, err := m.counter.DeadCount(ctx, QueueNetCommands)
	if err != nil {
		return fmt.Errorf("dead_letter_monitor: count dead tasks: %w", err)
	}
	deadLetterQueueDepth.Set(float64(dead))
	if dead > 0 {
		log.Warn().Int("archived_count", dead).Msg("dead_letter_monitor: archived tasks detected")
		m.alerter.Trigger("dead_letter_queue_non_empty", dead)
	}
	return nil
}
