package radius

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// Bounded retention for the authentication log — FR-NET-004.
//
// A row per authentication is high-volume by nature, so the requirement
// asks for bounded retention rather than an archive. Without a purge the
// table grows until it is the largest thing in the database and the
// indexes stop fitting in memory — at which point the log that exists to
// diagnose problems becomes one.

// AuthLogPurger deletes expired attempts. Satisfied by *db.AuthLogStore.
type AuthLogPurger interface {
	PurgeAuthLog(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
}

var authLogPurged = promauto.NewCounter(prometheus.CounterOpts{
	Name: "radius_auth_log_purged_total",
	Help: "Authentication attempts deleted by the retention purge",
})

// Purge pacing.
const (
	// authPurgeInterval is how often a pass runs. Hourly: the window is
	// measured in days, so there is nothing to gain from checking more
	// often, and an hourly pass spreads the deletion rather than doing a
	// day's worth at midnight.
	authPurgeInterval = time.Hour
	// authPurgeBatch bounds one DELETE, so a backlog — a first run after
	// an upgrade, say — drains over several passes instead of holding one
	// long transaction over a large table.
	authPurgeBatch = 10000
	// authPurgeMaxBatches bounds one pass. A pass that keeps finding work
	// stops and lets the next one continue, so the purge can never
	// monopolise a connection indefinitely.
	authPurgeMaxBatches = 20
)

// AuthLogPurgeScanner deletes authentication attempts past their
// retention window.
type AuthLogPurgeScanner struct {
	store     AuthLogPurger
	retention time.Duration
	interval  time.Duration
}

// NewAuthLogPurgeScanner constructs the purge. A zero retention uses the
// store's own default.
func NewAuthLogPurgeScanner(store AuthLogPurger, retention time.Duration) *AuthLogPurgeScanner {
	return &AuthLogPurgeScanner{store: store, retention: retention, interval: authPurgeInterval}
}

// Run purges on a tick until ctx is cancelled, starting with one pass
// immediately so a fresh deployment does not wait an hour to bound a
// table that may already be large.
func (s *AuthLogPurgeScanner) Run(ctx context.Context) {
	s.scan(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scan(ctx)
		}
	}
}

// scan runs one pass, in bounded batches.
func (s *AuthLogPurgeScanner) scan(ctx context.Context) {
	if s.store == nil {
		return
	}
	var total int64
	for i := 0; i < authPurgeMaxBatches; i++ {
		n, err := s.store.PurgeAuthLog(ctx, s.retention, authPurgeBatch)
		if err != nil {
			// Logged, not retried in a loop: the next pass tries again in
			// an hour, and a database that is down will not be fixed by
			// asking it twenty times in a row.
			log.Error().Err(err).Msg("radius: authentication log purge failed")
			return
		}
		total += n
		if n < authPurgeBatch {
			// Caught up.
			break
		}
		// Stop if the context went away mid-pass rather than starting
		// another batch against it.
		if ctx.Err() != nil {
			break
		}
	}
	if total > 0 {
		authLogPurged.Add(float64(total))
		log.Info().Int64("deleted", total).Dur("retention", s.retention).
			Msg("radius: purged expired authentication attempts")
	}
}
