package radius

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/rs/zerolog/log"
)

// Authentication attempt logging — FR-NET-004 | migration 063.
//
// Every Access-Request produces a row: when, who, from which NAS and MAC,
// accepted or rejected, and if rejected, why. Nothing recorded that
// before, so "it was working yesterday" was answerable only by watching
// it happen again.
//
// **The password is never recorded, in any form.** There is no field for
// it on AuthAttempt and no column for it in migration 063, so no future
// code path can add one by accident. The reference product's equivalent
// log keeps it; that turns an operational log into a credential dump.
//
// The hard constraint is NFR-PERF-001: a 15ms p99 at 5,000 requests a
// second. A synchronous INSERT per authentication would spend the whole
// budget in the database, so writes go through a bounded buffer and a
// batching goroutine, and the auth path's cost is one non-blocking
// channel send.

// Reject reasons, matching chk_auth_log_reason_vocab (migration 063, widened
// by 077). A fixed vocabulary so the column can be grouped and counted
// rather than read.
const (
	RejectUnknownUser   = "unknown_user"
	RejectBadPassword   = "bad_password"
	RejectNotAuthorised = "not_authorised"
	RejectLockedOut     = "locked_out"
	RejectNoMABDevice   = "no_mab_device"
	RejectEAPFailed     = "eap_failed"
	RejectInternalError = "internal_error"
	// RejectConnectionLimit: the credentials were right, but the subscriber
	// already holds as many connections as they pay for.
	RejectConnectionLimit = "connection_limit"
)

// RejectReasons is every reason the daemon can record. The vocabulary check
// and the console's labels both range over it, so a reason added here and
// forgotten in either place fails a test rather than a batch insert or a
// raw string on screen.
func RejectReasons() []string {
	return []string{
		RejectUnknownUser, RejectBadPassword, RejectNotAuthorised, RejectLockedOut,
		RejectNoMABDevice, RejectEAPFailed, RejectInternalError, RejectConnectionLimit,
	}
}

// Authentication methods.
const (
	MethodPAP = "pap"
	MethodEAP = "eap"
	MethodMAB = "mab"
)

// Attempt results.
const (
	ResultAccept = "accept"
	ResultReject = "reject"
)

// AuthAttempt is one authentication attempt as it is recorded.
//
// Note what is absent: the password, in cleartext, hashed, or any other
// form. See this file's package comment.
type AuthAttempt struct {
	Username     string
	NASIP        string
	MAC          string
	Result       string
	RejectReason string
	AuthMethod   string
	At           time.Time
}

// AuthLogStore persists batches of attempts. Satisfied by *db.AuthLogStore.
type AuthLogStore interface {
	RecordAuthAttempts(ctx context.Context, attempts []AuthAttempt) error
}

var (
	authLogWritten = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_auth_log_written_total",
		Help: "Authentication attempts persisted to radius_auth_log",
	})
	// authLogDropped is the metric that matters. A sustained non-zero rate
	// means the writer cannot keep up and the log has holes — which is
	// the right trade (the alternative is blocking authentication) but
	// must be visible rather than silent.
	authLogDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_auth_log_dropped_total",
		Help: "Authentication attempts discarded because the log buffer was full",
	})
	authLogBatchFailures = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_auth_log_batch_failures_total",
		Help: "Batches of authentication attempts that could not be written",
	})
)

// Buffer and batch sizing.
const (
	// authLogBufferSize bounds memory. At 5,000 requests a second this is
	// about two seconds of headroom, which rides out a database hiccup
	// without letting a sustained outage grow the heap.
	authLogBufferSize = 8192
	// authLogBatchSize is how many rows one INSERT carries.
	authLogBatchSize = 256
	// authLogFlushInterval bounds how long an attempt waits before it is
	// written, so a quiet network's few rejects still appear promptly
	// rather than sitting in a half-full batch.
	authLogFlushInterval = 2 * time.Second
	// authLogWriteTimeout bounds one batch write.
	authLogWriteTimeout = 10 * time.Second
)

// AuthLogger buffers authentication attempts and writes them in batches.
type AuthLogger struct {
	store AuthLogStore
	ch    chan AuthAttempt
	now   func() time.Time
}

// NewAuthLogger constructs an AuthLogger. Run must be called to drain it.
func NewAuthLogger(store AuthLogStore) *AuthLogger {
	return &AuthLogger{
		store: store,
		ch:    make(chan AuthAttempt, authLogBufferSize),
		now:   time.Now,
	}
}

// Record queues one attempt. Never blocks.
//
// A full buffer drops the attempt and increments authLogDropped. That is
// the deliberate trade this whole design exists for: an authentication
// must not wait on its own logging, and a subscriber left offline because
// the log could not keep up would be a far worse failure than a missing
// row.
func (l *AuthLogger) Record(a AuthAttempt) {
	if l == nil || l.ch == nil {
		return
	}
	if a.At.IsZero() {
		a.At = l.now()
	}
	select {
	case l.ch <- a:
	default:
		authLogDropped.Inc()
	}
}

// Run drains the buffer until ctx is cancelled, then flushes what is left.
func (l *AuthLogger) Run(ctx context.Context) {
	batch := make([]AuthAttempt, 0, authLogBatchSize)
	ticker := time.NewTicker(authLogFlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Drain whatever is queued before going away: these rows are
			// cheap and the attempts they describe already happened.
			for {
				select {
				case a := <-l.ch:
					batch = append(batch, a)
					if len(batch) >= authLogBatchSize {
						l.flush(batch)
						batch = batch[:0]
					}
				default:
					l.flush(batch)
					return
				}
			}
		case a := <-l.ch:
			batch = append(batch, a)
			if len(batch) >= authLogBatchSize {
				l.flush(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				l.flush(batch)
				batch = batch[:0]
			}
		}
	}
}

// flush writes one batch, logging a failure rather than retrying.
//
// No retry: the next batch is already arriving, and holding a failed one
// to try again would grow unboundedly behind a database that is down —
// which is the failure mode the bounded buffer exists to avoid in the
// first place. The dropped rows are counted.
func (l *AuthLogger) flush(batch []AuthAttempt) {
	if len(batch) == 0 || l.store == nil {
		return
	}
	// A context of its own: Run's ctx may already be cancelled during
	// shutdown, and these rows should still be written.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), authLogWriteTimeout)
	defer cancel()

	if err := l.store.RecordAuthAttempts(ctx, batch); err != nil {
		authLogBatchFailures.Inc()
		authLogDropped.Add(float64(len(batch)))
		log.Error().Err(err).Int("attempts", len(batch)).
			Msg("radius: a batch of authentication attempts could not be written")
		return
	}
	authLogWritten.Add(float64(len(batch)))
}

// SetAuthLogger attaches authentication attempt logging (FR-NET-004).
//
// Optional, like every other store on this daemon: a deployment without it
// authenticates exactly as before and records nothing. The daemon never
// fails an authentication because logging is unavailable.
func (d *RadiusDaemon) SetAuthLogger(l *AuthLogger) { d.authLog = l }

// logAuthAttempt records one attempt, if logging is wired.
func (d *RadiusDaemon) logAuthAttempt(a AuthAttempt) {
	if d.authLog == nil {
		return
	}
	d.authLog.Record(a)
}

// AuthLogRetention is how long attempts are kept, as FR-NET-004's
// "bounded retention" requires.
//
// Thirty days: long enough to investigate "it stopped working last week",
// short enough that a table taking a row per authentication stays an
// operational tool rather than becoming an archive nobody sized for.
const AuthLogRetention = 30 * 24 * time.Hour

// AuthLogFilter narrows a listing of attempts.
type AuthLogFilter struct {
	Username string // "" for any
	Result   string // "" for any
	Reason   string // "" for any
	From     *time.Time
	To       *time.Time
	Limit    int
}

// AuthLogEntry is one stored attempt, as the console reads it back.
type AuthLogEntry struct {
	ID           int64
	Username     string
	NASIP        string
	MAC          string
	Result       string
	RejectReason string
	AuthMethod   string
	CreatedAt    time.Time
}

// Accepted reports whether this attempt succeeded.
func (e AuthLogEntry) Accepted() bool { return e.Result == ResultAccept }

// AuthRejectSummary is one reject reason and how often it occurred.
type AuthRejectSummary struct {
	Reason string
	Count  int
}
