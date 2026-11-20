package middleware

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// Persisting the audit trail — FR-OBS-006 | migration 062.
//
// middleware.Audit has always written a structured log line. That line
// goes to the process log, which rotates and is not queryable, so "who
// changed this subscriber's plan last March" was answerable only by
// grepping whatever log survived. This adds a sink so the same entries
// land in an append-only table as well.
//
// A package-level sink rather than a store threaded through every caller:
// Audit is called from well over a hundred places across api, staffui and
// the background scanners, and giving each of them a store to carry would
// be a far larger change than the feature justifies — and one where a
// single missed call site is a silently unaudited action.

// AuditEntry is one staff action, as both the log line and the table
// record it.
type AuditEntry struct {
	CorrelationID string
	ActorID       string
	ActorRole     string
	Action        string
	Target        string
	Detail        map[string]any
}

// AuditSink persists audit entries. Satisfied by *db.AuditStore.
type AuditSink interface {
	RecordAudit(ctx context.Context, e AuditEntry) error
}

// auditSink holds the configured sink. atomic.Value rather than a plain
// variable with a mutex: it is written once at start-up and read on every
// audited action, and the read must not contend.
var auditSink atomic.Value // of AuditSink

// SetAuditSink wires audit persistence. Called once at start-up; passing
// nil leaves Audit logging only, which is what every test and any
// deployment without the table does.
//
// Failures from the sink are logged and swallowed. An audit write must
// never be able to fail the action it is recording: refusing a payment
// because its audit row could not be written would turn a bookkeeping
// problem into an operational one, and the log line — which is written
// first and unconditionally — still carries the record.
func SetAuditSink(s AuditSink) {
	if s == nil {
		auditSink = atomic.Value{}
		return
	}
	auditSink.Store(s)
}

// auditPersistTimeout bounds one audit write.
//
// Short, and with a context of its own rather than the caller's: the
// caller's context may already be cancelled — a handler that has written
// its response, or a scanner shutting down — and the action has happened
// either way, so its record should still be written. A slow database must
// not hold up the request that is merely reporting what it did.
const auditPersistTimeout = 3 * time.Second

// persistAudit writes one entry to the sink, if there is one.
func persistAudit(_ context.Context, e AuditEntry) {
	v := auditSink.Load()
	if v == nil {
		return
	}
	sink, ok := v.(AuditSink)
	if !ok || sink == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), auditPersistTimeout)
	defer cancel()
	if err := sink.RecordAudit(ctx, e); err != nil {
		// Logged at error because an audit trail with holes in it is worth
		// knowing about — but not returned, for the reason SetAuditSink
		// documents.
		log.Error().Err(err).
			Str("action", e.Action).
			Str("actor_id", e.ActorID).
			Str("correlation_id", e.CorrelationID).
			Msg("middleware: audit entry could not be persisted; the log line above is the only record")
	}
}

// AuditFilter narrows a listing of the audit trail. FR-OBS-006 asks for
// actor, action and date.
type AuditFilter struct {
	Actor  string     // "" for any
	Action string     // "" for any
	From   *time.Time // inclusive
	To     *time.Time // exclusive
	Limit  int
}

// AuditRecord is one stored entry, as the console reads it back.
type AuditRecord struct {
	ID            int64
	CorrelationID string
	ActorID       string
	ActorRole     string
	Action        string
	Target        string
	Detail        map[string]any
	CreatedAt     time.Time
}
