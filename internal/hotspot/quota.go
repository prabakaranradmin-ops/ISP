package hotspot

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// Voucher data-cap enforcement — FR-HSP-001, FR-HSP-003 | migration 035.
//
// A voucher's data_cap_bytes was recorded and never read, so a voucher sold as
// "1 GB" was limited only by its duration. Closing that needed a different
// route from the subscriber path rather than a lookup, for a structural
// reason: the FUP scanner finds over-quota sessions by joining subscribers,
// and a voucher grant has no subscriber by design
// (chk_grant_has_exactly_one_source, migration 034).
//
// So voucher sessions are metered on the grant, and this scanner enforces the
// cap. Enforcement is a disconnect, not a throttle, which is the one real
// design decision here: a voucher is prepaid for a fixed volume, and leaving
// somebody connected at a crawl after it runs out occupies a slot, reads as a
// broken network rather than a spent voucher, and gives the counter staff
// nothing to point at. Time-expiry already ends a session outright; volume
// expiry behaving differently would be the surprise.

var (
	quotaExhaustedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "hotspot_voucher_exhausted_total",
		Help: "Voucher grants ended because their data cap was reached",
	})
	quotaEnforceFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "hotspot_voucher_enforce_failures_total",
		Help: "Failures enforcing a voucher data cap, by stage",
	}, []string{"stage"})
)

const (
	// defaultQuotaInterval matches the FUP scanner's cadence closely enough
	// that a hotspot user and a PPPoE user experience the same lag between
	// exhausting a quota and being cut off.
	defaultQuotaInterval = 30 * time.Second
	defaultQuotaBatch    = 100
)

// OverCapGrant is a live voucher grant that has reached its data cap.
type OverCapGrant struct {
	GrantID   int64
	MAC       string
	VoucherID int
	SessionID string
	NASIP     string
	BytesUsed int64
	CapBytes  int64
}

// QuotaStore is the persistence surface for cap enforcement.
// Satisfied by *db.HotspotStore.
type QuotaStore interface {
	// ListGrantsOverCap returns live voucher grants at or past their cap.
	ListGrantsOverCap(ctx context.Context, limit int) ([]OverCapGrant, error)
	// MarkGrantExhausted revokes a grant and records that the cap is what
	// ended it, reporting whether this call was the one that did it.
	MarkGrantExhausted(ctx context.Context, grantID int64) (bool, error)
}

// Disconnector ends a live session on the NAS. Satisfied by a small adapter
// over the queue client in cmd/radiusd.
//
// An interface rather than a direct dependency on the task queue so this
// package does not import internal/fup, and so the scanner can be tested
// without Redis.
type Disconnector interface {
	Disconnect(ctx context.Context, nasIP, sessionID string) error
}

// QuotaScanner ends voucher sessions that have used their allowance.
type QuotaScanner struct {
	db       QuotaStore
	pod      Disconnector
	interval time.Duration
	batch    int
}

// NewQuotaScanner constructs a QuotaScanner. An interval of 0 uses the default.
func NewQuotaScanner(store QuotaStore, pod Disconnector, interval time.Duration) *QuotaScanner {
	if interval <= 0 {
		interval = defaultQuotaInterval
	}
	return &QuotaScanner{db: store, pod: pod, interval: interval, batch: defaultQuotaBatch}
}

// SetBatchSize overrides how many grants one pass handles.
func (s *QuotaScanner) SetBatchSize(n int) {
	if n > 0 {
		s.batch = n
	}
}

// Run scans on an interval until ctx is cancelled.
func (s *QuotaScanner) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.ScanOnce(ctx); err != nil {
				log.Error().Err(err).Msg("hotspot: voucher quota scan failed")
			}
		}
	}
}

// ScanOnce performs a single enforcement pass, returning how many grants were
// ended.
//
// The grant is revoked before the disconnect is attempted, which is the
// opposite order from the document purge and deliberately so. Revoking is what
// stops the MAC re-authenticating; if the disconnect failed and revocation
// were still pending, the user would simply reconnect and keep going on an
// exhausted voucher. A disconnect that fails after revocation costs the user
// the remainder of a session they are no longer entitled to, and the next
// Access-Request refuses them anyway.
func (s *QuotaScanner) ScanOnce(ctx context.Context) (int, error) {
	over, err := s.db.ListGrantsOverCap(ctx, s.batch)
	if err != nil {
		quotaEnforceFailures.WithLabelValues("list").Inc()
		return 0, err
	}

	ended := 0
	for _, g := range over {
		if ctx.Err() != nil {
			return ended, ctx.Err()
		}

		claimed, err := s.db.MarkGrantExhausted(ctx, g.GrantID)
		if err != nil {
			quotaEnforceFailures.WithLabelValues("revoke").Inc()
			log.Error().Err(err).Int64("grant_id", g.GrantID).
				Msg("hotspot: could not revoke an exhausted voucher grant")
			continue
		}
		if !claimed {
			// Another replica's scan got there first.
			continue
		}

		quotaExhaustedTotal.Inc()
		ended++
		log.Info().Int64("grant_id", g.GrantID).Str("mac", g.MAC).
			Int64("used", g.BytesUsed).Int64("cap", g.CapBytes).
			Msg("hotspot: voucher data cap reached; access revoked")

		// Best-effort. The grant is already revoked, so the worst case is a
		// session that survives until the NAS next re-authenticates it rather
		// than one that continues indefinitely.
		if s.pod == nil || g.SessionID == "" || g.NASIP == "" {
			continue
		}
		if err := s.pod.Disconnect(ctx, g.NASIP, g.SessionID); err != nil {
			quotaEnforceFailures.WithLabelValues("disconnect").Inc()
			log.Warn().Err(err).Int64("grant_id", g.GrantID).
				Msg("hotspot: exhausted voucher revoked but the session could not be disconnected")
		}
	}
	return ended, nil
}
