// Package jobqueue is a PostgreSQL-backed background task queue, replacing
// Asynq (github.com/hibiken/asynq) and with it the last reason this system
// needed a Redis at all.
//
// The API deliberately mirrors the subset of Asynq this codebase actually
// used — NewTask with functional options, a ServeMux of Handlers keyed by
// task type, a Server with weighted queues — so the nine existing handlers
// and fourteen producer call sites needed an import and a type name
// changed, not a rewrite. That is a constraint on this package, not an
// accident of it: a queue swap that also rewrote every handler would have
// mixed two kinds of risk in one change.
//
// What it does not copy is Asynq's Redis-shaped semantics. Dequeue is
// SELECT ... FOR UPDATE SKIP LOCKED in a short transaction, leases replace
// Redis's in-flight tracking, and LISTEN/NOTIFY replaces a blocking pop.
//
// Durability is the reason for the move, not just dependency removal:
// tasks now live in the same database as the rows they act on, so a task
// and the state change that scheduled it can no longer disagree after a
// crash of one but not the other.
package jobqueue

import (
	"errors"
	"time"
)

// Task is one unit of background work. Type() and Payload() match
// *asynq.Task's shape exactly, which is what lets the existing handlers
// keep their bodies unchanged.
type Task struct {
	typ     string
	payload []byte
	opts    options
}

// Type reports the task type a ServeMux routes on.
func (t *Task) Type() string { return t.typ }

// Payload returns the raw bytes the producer supplied.
func (t *Task) Payload() []byte { return t.payload }

// options carries everything a producer can set on a task. Resolved at
// enqueue time by merging the task's own options with any passed to
// Enqueue, matching Asynq's precedence (call-site options win).
type options struct {
	queue     string
	maxRetry  int
	timeout   time.Duration
	retention time.Duration
	taskID    string
	processIn time.Duration
}

// Option configures a Task at construction or at enqueue time.
type Option func(*options)

// DefaultQueue is where a task with no explicit queue lands.
const DefaultQueue = "default"

// Queue routes the task to a named queue, which Config.Queues weights.
func Queue(name string) Option { return func(o *options) { o.queue = name } }

// MaxRetry caps how many times a failing task is retried before it is
// moved to the dead-letter state. Zero means "run once, never retry".
func MaxRetry(n int) Option { return func(o *options) { o.maxRetry = n } }

// Timeout bounds one execution of the handler. Zero means no per-task
// bound beyond the server's own shutdown.
func Timeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// Retention keeps a completed task's row inspectable for d after it
// finishes, instead of deleting it immediately. Used for operator
// visibility into work that has already succeeded (report exports in
// particular).
func Retention(d time.Duration) Option { return func(o *options) { o.retention = d } }

// TaskID sets an idempotency key. Enqueueing a second task with an id that
// is already present returns ErrTaskIDConflict rather than queueing a
// duplicate — this is what stops an overlapping scan from sending the same
// dunning notice or FUP warning twice.
func TaskID(id string) Option { return func(o *options) { o.taskID = id } }

// ProcessIn delays the task's first attempt by d.
func ProcessIn(d time.Duration) Option { return func(o *options) { o.processIn = d } }

// NewTask builds a Task.
func NewTask(taskType string, payload []byte, opts ...Option) *Task {
	t := &Task{typ: taskType, payload: payload}
	for _, opt := range opts {
		opt(&t.opts)
	}
	return t
}

// TaskInfo is what Enqueue reports back about a queued task. Deliberately
// thin: the two call sites that keep the return value only use it to
// confirm success, and every field Asynq's richer TaskInfo carried beyond
// these was unused here.
type TaskInfo struct {
	ID     int64
	Queue  string
	Type   string
	TaskID string
}

var (
	// ErrTaskIDConflict reports that a task with the same TaskID is already
	// queued. Callers treat this as success-by-idempotency, not failure.
	ErrTaskIDConflict = errors.New("jobqueue: task id already exists")

	// SkipRetry, when returned (or wrapped) by a handler, sends the task
	// straight to the dead-letter state instead of retrying it. Used where
	// a retry provably cannot help — a webhook aimed at a private address
	// stays aimed at a private address.
	SkipRetry = errors.New("jobqueue: skip retry")
)
