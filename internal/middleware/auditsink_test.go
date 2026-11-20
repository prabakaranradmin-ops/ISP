package middleware

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Audit persistence — FR-OBS-006.

type recordingSink struct {
	mu      sync.Mutex
	entries []AuditEntry
	err     error
	// block holds RecordAudit until released, to test that a slow sink
	// does not hold up the caller's own deadline.
	block chan struct{}
	// sawDeadline records whether the context handed to the sink carried
	// one, and whether it was already cancelled.
	sawDeadline  bool
	sawCancelled bool
}

func (s *recordingSink) RecordAudit(ctx context.Context, e AuditEntry) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, s.sawDeadline = ctx.Deadline()
	s.sawCancelled = ctx.Err() != nil
	s.entries = append(s.entries, e)
	return s.err
}

func (s *recordingSink) all() []AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEntry(nil), s.entries...)
}

// resetSink clears the package-level sink, so one test cannot leak into
// the next through it.
func resetSink(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { SetAuditSink(nil) })
}

func TestFR_OBS_006_AuditEntriesReachTheSink(t *testing.T) {
	resetSink(t)
	sink := &recordingSink{}
	SetAuditSink(sink)

	ctx := WithSubject(context.Background(), "meena")
	Audit(ctx, "subscriber.plan_change", "42", map[string]any{"new_plan_id": 7})

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	e := got[0]
	if e.Action != "subscriber.plan_change" || e.Target != "42" {
		t.Errorf("entry reads %+v", e)
	}
	if e.ActorID != "meena" {
		t.Errorf("actor is %q, want meena", e.ActorID)
	}
	if e.Detail["new_plan_id"] != 7 {
		t.Errorf("detail is %v", e.Detail)
	}
	// The same correlation id the log line carries, so a row and a line
	// can be tied together.
	if e.CorrelationID == "" {
		t.Error("the entry carries no correlation id")
	}
}

// Each entry gets its own correlation id: two actions must be
// distinguishable in the trail.
func TestFR_OBS_006_EachEntryHasItsOwnCorrelationID(t *testing.T) {
	resetSink(t)
	sink := &recordingSink{}
	SetAuditSink(sink)

	ctx := WithSubject(context.Background(), "meena")
	Audit(ctx, "product.attached", "ROUTER", nil)
	Audit(ctx, "product.ended", "ROUTER", nil)

	got := sink.all()
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].CorrelationID == got[1].CorrelationID {
		t.Error("two actions share one correlation id")
	}
}

// The contract that matters: a sink that fails must not fail the action.
// Audit returns nothing, so the only way this could break a caller is by
// panicking — which is exactly what an unchecked type assertion or a nil
// map would do.
func TestFR_OBS_006_AFailingSinkDoesNotBreakTheAction(t *testing.T) {
	resetSink(t)
	SetAuditSink(&recordingSink{err: errors.New("database down")})

	// No panic, no hang: the caller carries on.
	done := make(chan struct{})
	go func() {
		defer close(done)
		Audit(WithSubject(context.Background(), "meena"), "payment.recorded", "RCPT-1", nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Audit did not return with a failing sink")
	}
}

// With no sink at all — every test, and any deployment without the table
// — Audit still works and simply logs.
func TestFR_OBS_006_NoSinkIsNotAnError(t *testing.T) {
	resetSink(t)
	SetAuditSink(nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		Audit(context.Background(), "plan.updated", "Fibre 100", nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Audit did not return with no sink")
	}
}

// The sink gets a context of its own, with a deadline and not already
// cancelled.
//
// This is the property a plain `ctx` pass-through would break: a handler
// that has written its response, or a scanner shutting down, has a
// cancelled context — and the action has happened either way, so its
// record must still be written.
func TestFR_OBS_006_TheSinkGetsALiveContextEvenWhenTheCallersIsCancelled(t *testing.T) {
	resetSink(t)
	sink := &recordingSink{}
	SetAuditSink(sink)

	ctx, cancel := context.WithCancel(WithSubject(context.Background(), "meena"))
	cancel() // the caller is already done
	Audit(ctx, "subscriber.terminated", "42", nil)

	if len(sink.all()) != 1 {
		t.Fatal("a cancelled caller context dropped the audit entry")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.sawCancelled {
		t.Error("the sink was handed an already-cancelled context")
	}
	if !sink.sawDeadline {
		t.Error("the sink was handed a context with no deadline; a hung database would block the caller forever")
	}
}

// Replacing the sink takes effect, and clearing it stops persistence.
func TestFR_OBS_006_SinkCanBeReplacedAndCleared(t *testing.T) {
	resetSink(t)
	first := &recordingSink{}
	SetAuditSink(first)
	Audit(context.Background(), "a.one", "x", nil)

	second := &recordingSink{}
	SetAuditSink(second)
	Audit(context.Background(), "a.two", "x", nil)

	SetAuditSink(nil)
	Audit(context.Background(), "a.three", "x", nil)

	if n := len(first.all()); n != 1 {
		t.Errorf("the first sink got %d entries, want 1", n)
	}
	if got := second.all(); len(got) != 1 || got[0].Action != "a.two" {
		t.Errorf("the second sink got %+v", got)
	}
}

// An action with no subject on the context is still recorded: who is
// missing is itself worth knowing, and dropping the entry would hide the
// path that forgot to set one.
func TestFR_OBS_006_AnActionWithNoSubjectIsStillRecorded(t *testing.T) {
	resetSink(t)
	sink := &recordingSink{}
	SetAuditSink(sink)

	Audit(context.Background(), "something.happened", "x", nil)

	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	// Empty here; the store substitutes "unknown", which is where that
	// decision belongs since it is the column's NOT NULL that forces it.
	if got[0].ActorID != "" {
		t.Errorf("actor is %q, want it passed through empty", got[0].ActorID)
	}
}
