package revenue

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// ReconcileJob shipped complete and tested, documented as running "nightly at
// 02:00 IST on a nightly schedule", and was never constructed anywhere in cmd/. The
// nightly revenue reconciliation therefore never ran: no snapshot was written,
// no ledger variance was ever compared, and no collections forecast was
// produced. This is the scheduler that runs it.
//
// FR: FR-REV-001..004 | DDS §5.10

const (
	// reconcileHourIST is 02:00 — chosen in the design so the job sees a
	// settled day's ledger rather than one still being written to.
	reconcileHourIST = 2
	istZone          = "Asia/Kolkata"
)

var (
	reconcileRuns = promauto.NewCounter(prometheus.CounterOpts{
		Name: "revenue_reconcile_runs_total",
		Help: "Nightly revenue reconciliation runs started",
	})
	reconcileFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "revenue_reconcile_failures_total",
		Help: "Nightly revenue reconciliation runs that returned an error",
	})
)

// ReconcileScheduler runs a ReconcileJob once per night at 02:00 IST.
type ReconcileScheduler struct {
	job *ReconcileJob
	loc *time.Location
	now func() time.Time // injectable for tests
}

// NewReconcileScheduler constructs a scheduler for the nightly reconciliation.
//
// A missing Asia/Kolkata database is reported rather than silently tolerated:
// falling back to UTC would move the run to 07:30 IST, into the working day,
// and it would keep running at the wrong hour with nothing to indicate why.
// The radiusd image installs tzdata for exactly this reason.
func NewReconcileScheduler(job *ReconcileJob) *ReconcileScheduler {
	loc, err := time.LoadLocation(istZone)
	if err != nil {
		log.Error().Err(err).
			Str("zone", istZone).
			Msg("revenue: timezone database unavailable — reconciliation will run at 02:00 UTC, not IST")
		loc = time.UTC
	}
	return &ReconcileScheduler{job: job, loc: loc, now: time.Now}
}

// Run blocks until ctx is cancelled, running the reconciliation each night.
//
// It deliberately does not run at startup. Unlike dunning — where a missed day
// leaves subscribers un-warned and is worth catching up immediately — a
// reconciliation snapshot is keyed to a date, and running one on deploy would
// overwrite the night's figures with a partial day's.
func (s *ReconcileScheduler) Run(ctx context.Context) {
	for {
		wait := s.untilNextRun(s.now())
		log.Info().
			Dur("in", wait).
			Str("zone", s.loc.String()).
			Msg("revenue: next reconciliation scheduled")

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		reconcileRuns.Inc()
		if err := s.job.Run(ctx); err != nil {
			reconcileFailures.Inc()
			// Logged, not fatal: tomorrow's run should still happen, and a
			// scheduler that exited on one bad night would silently stop
			// reconciling revenue from then on.
			log.Error().Err(err).Msg("revenue: nightly reconciliation failed")
			continue
		}
		log.Info().Msg("revenue: nightly reconciliation complete")
	}
}

// untilNextRun returns how long to wait for the next 02:00 in the scheduler's
// zone. Exported behaviour is tested through UntilNextRun in export_test.go.
func (s *ReconcileScheduler) untilNextRun(from time.Time) time.Duration {
	local := from.In(s.loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), reconcileHourIST, 0, 0, 0, s.loc)
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(local)
}
