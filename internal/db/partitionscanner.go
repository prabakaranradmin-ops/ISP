package db

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// PartitionMonthsAhead is how much runway the roll-forward keeps.
//
// Three months, matching what the migrations provision, so a database that
// has had no maintenance for a quarter still accepts writes while somebody
// works out why. The cost of a spare partition is an empty table and an
// index; the cost of a missing one is an INSERT that fails.
const PartitionMonthsAhead = 3

// Pacing and the alerting floor.
const (
	// partitionScanInterval is how often a pass runs. Daily: the unit of
	// work is a month, and a pass does nothing on all but a few days of it.
	partitionScanInterval = 24 * time.Hour
	// partitionFloor is the coverage below which a pass complains. Reached
	// only if provisioning failed, since a successful pass leaves
	// PartitionMonthsAhead.
	partitionFloor = 1
)

var (
	partitionMonthsAhead = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_partition_months_ahead",
		Help: "Whole months of partition coverage remaining for each monthly-partitioned table",
	}, []string{"table"})
	partitionRollForwardFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "db_partition_rollforward_failures_total",
		Help: "Failed attempts to provision monthly partitions",
	})
	// Exported whether or not retention is configured: with none set these
	// tables grow without bound, and the only honest thing to do about a
	// decision nobody has made is to make its cost visible.
	partitionMonthsHeld = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "db_partition_months_held",
		Help: "Months of history each monthly-partitioned table is holding",
	}, []string{"table"})
	partitionsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "db_partitions_dropped_total",
		Help: "Expired monthly partitions dropped by the retention policy",
	}, []string{"table"})
)

// PartitionScanner keeps the monthly-partitioned tables provisioned ahead of
// the writes that land in them.
//
// It replaces a line in a runbook that was never written. The function it
// calls has carried a comment since migration 010 saying it "must be called
// from the monthly maintenance job (OPS §12)"; there was no such job, and
// OPS §12 did not mention partitions. An operational requirement that
// depends on somebody remembering it every month is a defect with a delay on
// it.
type PartitionScanner struct {
	store       *MaintenanceStore
	interval    time.Duration
	monthsAhead int
	now         func() time.Time
	// retention is empty unless an operator has set one. Empty means report
	// the growth and drop nothing — see partitionretention.go on why there is
	// no default.
	retention RetentionPolicy
}

// NewPartitionScanner constructs the roll-forward, with no retention policy.
func NewPartitionScanner(store *MaintenanceStore) *PartitionScanner {
	return &PartitionScanner{
		store:       store,
		interval:    partitionScanInterval,
		monthsAhead: PartitionMonthsAhead,
		now:         time.Now,
	}
}

// SetRetention turns on dropping expired partitions.
//
// Separate from the constructor so that configuring retention is an explicit
// act in the caller: a scanner built the ordinary way never deletes anything,
// and adding a deletion cannot happen by passing a zero value.
func (s *PartitionScanner) SetRetention(p RetentionPolicy) { s.retention = p }

// Run provisions partitions on a tick until ctx is cancelled, starting with a
// pass immediately.
//
// Immediately, not on the first tick, because the state this exists to
// prevent may already have arrived: a database that ran out of partitions
// last week is refusing accounting writes now, and waiting a day to notice
// would be its own incident.
func (s *PartitionScanner) Run(ctx context.Context) {
	if err := s.Once(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error().Err(err).Msg("db: partition roll-forward failed on the first pass")
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Once(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error().Err(err).Msg("db: partition roll-forward failed")
			}
		}
	}
}

// Once runs a single pass: provision every monthly-partitioned table, then
// re-read coverage and report it.
//
// Reading coverage back after provisioning rather than assuming it worked is
// the point of the second query. The failure this guards against is a
// privilege error — the services connect as bss_app, which does not own the
// partitioned parents, and before migration 076 could not create a partition
// at all. A scanner that logged "provisioned" without checking would have
// been indistinguishable from one that worked.
func (s *PartitionScanner) Once(ctx context.Context) error {
	if s.store == nil {
		return nil
	}

	before, err := s.store.PartitionCoverage(ctx)
	if err != nil {
		partitionRollForwardFailures.Inc()
		return err
	}
	if len(before) == 0 {
		// No monthly-partitioned tables. Said out loud, because the three
		// that exist are not optional and their absence means the schema is
		// not what this code was written against.
		log.Warn().Msg("db: no monthly-partitioned tables found; partition roll-forward has nothing to do")
		return nil
	}

	var firstErr error
	for _, c := range before {
		if err := s.store.EnsureMonthlyPartitions(ctx, c.Table, s.monthsAhead); err != nil {
			partitionRollForwardFailures.Inc()
			log.Error().Err(err).Str("table", c.Table).
				Msg("db: could not provision monthly partitions — writes to this table will " +
					"fail outright once the covered months run out")
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}

	after, err := s.store.PartitionCoverage(ctx)
	if err != nil {
		partitionRollForwardFailures.Inc()
		if firstErr == nil {
			firstErr = err
		}
		return firstErr
	}

	now := s.now()
	for _, c := range after {
		months := c.MonthsAhead(now)
		partitionMonthsAhead.WithLabelValues(c.Table).Set(float64(months))
		switch {
		case months < 0:
			log.Error().Str("table", c.Table).
				Msg("db: no partition covers the current month — every insert into this table " +
					"is failing")
		case months <= partitionFloor:
			log.Warn().Str("table", c.Table).Int("months_ahead", months).
				Time("covered_until", c.CoveredUntil).
				Msg("db: partition coverage is nearly exhausted")
		}
	}

	if err := s.retire(ctx, now); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// retire reports how much history each table holds and, where a retention is
// configured, drops at most one expired partition per table.
//
// One per pass is deliberate. A first run against years of accumulated
// history would otherwise drop dozens of partitions in one go, holding a lock
// on the largest tables in the database while it did; at one a day the
// backlog clears quietly instead. The reporting half runs either way, because
// a growth problem nobody has decided about is still a growth problem.
func (s *PartitionScanner) retire(ctx context.Context, now time.Time) error {
	all, err := s.store.ListPartitions(ctx)
	if err != nil {
		return err
	}

	// Oldest month held per table, which is what the gauge is about.
	oldest := map[string]time.Time{}
	for _, p := range all {
		if p.Month.IsZero() {
			continue
		}
		if cur, ok := oldest[p.Parent]; !ok || p.Month.Before(cur) {
			oldest[p.Parent] = p.Month
		}
	}
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	for table, first := range oldest {
		held := (thisMonth.Year()-first.Year())*12 + int(thisMonth.Month()-first.Month()) + 1
		partitionMonthsHeld.WithLabelValues(table).Set(float64(held))
		if _, set := s.retention.MonthsFor(table); !set && held >= 24 {
			// Said once a day rather than never: two years of CGNAT and
			// session history with no decision behind it is a cost somebody
			// is paying without having chosen to.
			log.Warn().Str("table", table).Int("months_held", held).
				Msg("db: no retention configured and the table is holding years of " +
					"history; set PARTITION_RETENTION_MONTHS or accept the growth " +
					"deliberately")
		}
	}

	droppable := DroppablePartitions(all, s.retention, now)
	if len(droppable) == 0 {
		return nil
	}

	// At most one per table per pass, oldest first.
	done := map[string]bool{}
	for _, p := range droppable {
		if done[p.Parent] {
			continue
		}
		done[p.Parent] = true
		// Logged before the drop, not after: if this is the wrong decision,
		// the log line is the only record that it was made.
		log.Warn().Str("table", p.Parent).Str("partition", p.Name).
			Time("month", p.Month).
			Msg("db: dropping an expired partition; the records in it are gone")
		if err := s.store.DropPartition(ctx, p.Name); err != nil {
			log.Error().Err(err).Str("partition", p.Name).
				Msg("db: dropping an expired partition failed")
			return err
		}
		partitionsDropped.WithLabelValues(p.Parent).Inc()
	}
	return nil
}
