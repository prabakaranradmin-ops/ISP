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
}

// NewPartitionScanner constructs the roll-forward.
func NewPartitionScanner(store *MaintenanceStore) *PartitionScanner {
	return &PartitionScanner{
		store:       store,
		interval:    partitionScanInterval,
		monthsAhead: PartitionMonthsAhead,
		now:         time.Now,
	}
}

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
	return firstErr
}
