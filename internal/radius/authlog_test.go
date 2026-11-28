package radius

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Authentication attempt logging — FR-NET-004.

// storedRejectVocabulary reads chk_auth_log_reason_vocab from the
// migrations, taking the definition in the highest-numbered file that
// (re)defines it in its Up section.
func storedRejectVocabulary(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations not found: %v", err)
	}
	sort.Strings(files)
	def := regexp.MustCompile(`(?s)chk_auth_log_reason_vocab CHECK \(reject_reason IN \((.*?)\)\)`)
	quoted := regexp.MustCompile(`'([a-z_]*)'`)
	var latest string
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(body), "-- +goose Down")
		if m := def.FindAllStringSubmatch(up, -1); len(m) > 0 {
			latest = m[len(m)-1][1]
		}
	}
	if latest == "" {
		t.Fatal("no migration defines chk_auth_log_reason_vocab")
	}
	vocab := map[string]bool{}
	for _, m := range quoted.FindAllStringSubmatch(latest, -1) {
		vocab[m[1]] = true
	}
	return vocab
}

type fakeAuthLogStore struct {
	mu      sync.Mutex
	batches [][]AuthAttempt
	err     error
	// block holds RecordAuthAttempts until released, to simulate a slow
	// or stuck database.
	block chan struct{}
}

func (f *fakeAuthLogStore) RecordAuthAttempts(_ context.Context, attempts []AuthAttempt) error {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, append([]AuthAttempt(nil), attempts...))
	return f.err
}

func (f *fakeAuthLogStore) all() []AuthAttempt {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []AuthAttempt
	for _, b := range f.batches {
		out = append(out, b...)
	}
	return out
}

// waitFor polls until cond holds or the deadline passes, so a test of an
// asynchronous writer does not depend on a fixed sleep.
func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// The structural guarantee: AuthAttempt has no field that could hold a
// password, in any form.
//
// Asserted by reflection over the type rather than by reading it, because
// the risk this guards against is a future field added in good faith —
// "store the supplied password so staff can see what the customer typed",
// which is exactly what the reference product does and what turns an
// operational log into a credential dump.
func TestFR_NET_004_AuthAttemptCannotCarryAPassword(t *testing.T) {
	forbidden := []string{"password", "passwd", "secret", "credential", "pass", "pwd", "nthash", "hash"}

	typ := reflect.TypeOf(AuthAttempt{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, bad := range forbidden {
			if strings.Contains(name, bad) {
				t.Errorf("AuthAttempt has a field %q: the authentication log must never carry a credential (FR-NET-004)",
					typ.Field(i).Name)
			}
		}
	}

	// And the fields it does have are the ones the requirement names.
	want := map[string]bool{
		"Username": true, "NASIP": true, "MAC": true, "Result": true,
		"RejectReason": true, "AuthMethod": true, "At": true,
	}
	for i := 0; i < typ.NumField(); i++ {
		if !want[typ.Field(i).Name] {
			t.Errorf("AuthAttempt has an unexpected field %q; check it carries nothing sensitive", typ.Field(i).Name)
		}
	}
}

func TestFR_NET_004_AttemptsAreBufferedAndWritten(t *testing.T) {
	store := &fakeAuthLogStore{}
	l := NewAuthLogger(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	l.Record(AuthAttempt{Username: "acme", NASIP: "198.51.100.1", MAC: "AA:BB",
		Result: ResultReject, RejectReason: RejectBadPassword, AuthMethod: MethodPAP})
	l.Record(AuthAttempt{Username: "acme", NASIP: "198.51.100.1",
		Result: ResultAccept, AuthMethod: MethodPAP})

	if !waitFor(t, func() bool { return len(store.all()) == 2 }) {
		t.Fatalf("want 2 attempts written, got %d", len(store.all()))
	}
	got := store.all()
	if got[0].RejectReason != RejectBadPassword || got[0].Username != "acme" {
		t.Errorf("first attempt reads %+v", got[0])
	}
	// A timestamp is stamped even when the caller leaves it zero, so an
	// attempt cannot land with a zero time.
	for _, a := range got {
		if a.At.IsZero() {
			t.Error("an attempt was written with no timestamp")
		}
	}
}

// The contract this whole design exists for: Record never blocks, even
// with the buffer full and the database stuck.
//
// A subscriber left waiting — or worse, rejected — because the log could
// not keep up would be a far worse failure than a missing row.
func TestFR_NET_004_RecordNeverBlocksWhenTheBufferIsFull(t *testing.T) {
	// A store that never returns, so the drain goroutine is stuck and the
	// buffer fills.
	store := &fakeAuthLogStore{block: make(chan struct{})}
	defer close(store.block)

	l := NewAuthLogger(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	// Far more than the buffer holds.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < authLogBufferSize*3; i++ {
			l.Record(AuthAttempt{Username: "flood", Result: ResultReject,
				RejectReason: RejectBadPassword, AuthMethod: MethodPAP})
		}
	}()

	select {
	case <-done:
		// Returned promptly despite a stuck database: the drops are the
		// intended behaviour and the metric records them.
	case <-time.After(10 * time.Second):
		t.Fatal("Record blocked when the buffer was full; authentication would have waited on its own logging")
	}
}

// A store that fails must not stop the writer: the next batch is already
// arriving.
func TestFR_NET_004_AFailingStoreDoesNotStopTheWriter(t *testing.T) {
	store := &fakeAuthLogStore{err: errors.New("database down")}
	l := NewAuthLogger(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.Run(ctx)

	for i := 0; i < 3; i++ {
		l.Record(AuthAttempt{Username: "acme", Result: ResultAccept, AuthMethod: MethodPAP})
	}
	// The writer keeps accepting and keeps trying.
	if !waitFor(t, func() bool { return len(store.all()) >= 1 }) {
		t.Fatal("the writer stopped after a failure")
	}

	store.mu.Lock()
	store.err = nil
	store.mu.Unlock()
	l.Record(AuthAttempt{Username: "later", Result: ResultAccept, AuthMethod: MethodPAP})
	if !waitFor(t, func() bool {
		for _, a := range store.all() {
			if a.Username == "later" {
				return true
			}
		}
		return false
	}) {
		t.Error("the writer did not recover after the store came back")
	}
}

// Cancelling drains what is queued: the attempts already happened, and
// the rows are cheap.
func TestFR_NET_004_ShutdownFlushesWhatIsQueued(t *testing.T) {
	store := &fakeAuthLogStore{}
	l := NewAuthLogger(store)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		l.Run(ctx)
	}()

	for i := 0; i < 10; i++ {
		l.Record(AuthAttempt{Username: "shutdown", Result: ResultAccept, AuthMethod: MethodPAP})
	}
	cancel()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if n := len(store.all()); n != 10 {
		t.Errorf("shutdown wrote %d of 10 queued attempts", n)
	}
}

// A nil logger is a no-op, so a deployment without the table
// authenticates exactly as before.
func TestFR_NET_004_ANilLoggerIsSafe(t *testing.T) {
	var l *AuthLogger
	l.Record(AuthAttempt{Username: "x", Result: ResultAccept})

	d := &RadiusDaemon{}
	d.logAuthAttempt(AuthAttempt{Username: "x", Result: ResultAccept})
}

// Every reject reason the daemon can produce is in the stored vocabulary. A
// reason outside it is refused by the CHECK, which would lose the whole
// batch — and nothing in Go would catch it, because the reason is a string
// either side of the wire.
//
// The vocabulary is read from the migrations, the latest definition of
// chk_auth_log_reason_vocab winning, rather than copied here: a hand-kept
// copy is exactly what a new reason gets added to and the CHECK does not.
func TestFR_NET_004_EveryRejectReasonIsInTheStoredVocabulary(t *testing.T) {
	vocab := storedRejectVocabulary(t)
	for _, reason := range RejectReasons() {
		if !vocab[reason] {
			t.Errorf("reject reason %q is not in chk_auth_log_reason_vocab; the CHECK would refuse the batch", reason)
		}
	}
	for _, method := range []string{MethodPAP, MethodEAP, MethodMAB} {
		if method != "pap" && method != "eap" && method != "mab" {
			t.Errorf("auth method %q is not in migration 063's vocabulary", method)
		}
	}
	if ResultAccept != "accept" || ResultReject != "reject" {
		t.Error("the result values do not match migration 063's CHECK")
	}
}
