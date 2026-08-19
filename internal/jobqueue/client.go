package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// uniqueViolation is PostgreSQL's SQLSTATE for a unique constraint breach,
// which is how a duplicate TaskID surfaces.
const uniqueViolation = "23505"

// notifyChannel is the LISTEN/NOTIFY channel a Server waits on so a newly
// enqueued task is picked up immediately rather than at the next poll.
const notifyChannel = "jobqueue_new"

// Client enqueues tasks. Safe for concurrent use.
type Client struct {
	pool *pgxpool.Pool
}

// NewClient constructs a Client over an existing pool.
func NewClient(pool *pgxpool.Pool) *Client { return &Client{pool: pool} }

// Close exists so the call sites that deferred asynq.Client.Close keep
// compiling and reading sensibly. The pool is owned by whoever created it,
// so this deliberately does not close it — closing a caller's pool from
// here would take down everything else sharing it.
func (c *Client) Close() error { return nil }

// Enqueue queues a task for execution.
//
// Signature matches asynq.Client.Enqueue (no context) because two
// packages' TaskEnqueuer interfaces are declared against that shape; use
// EnqueueContext where a context is available, which is everywhere in this
// codebase that enqueues directly.
func (c *Client) Enqueue(task *Task, opts ...Option) (*TaskInfo, error) {
	return c.EnqueueContext(context.Background(), task, opts...)
}

// EnqueueContext queues a task, resolving the task's own options against
// any supplied here (these win, matching Asynq).
func (c *Client) EnqueueContext(ctx context.Context, task *Task, opts ...Option) (*TaskInfo, error) {
	if task == nil {
		return nil, fmt.Errorf("jobqueue: nil task")
	}
	o := task.opts
	for _, opt := range opts {
		opt(&o)
	}
	if o.queue == "" {
		o.queue = DefaultQueue
	}

	var (
		taskID   any = nil
		timeout  any = nil
		retUntil any = nil
	)
	if o.taskID != "" {
		taskID = o.taskID
	}
	if o.timeout > 0 {
		timeout = int(o.timeout.Seconds())
	}
	// Retention is resolved to an absolute instant at enqueue time rather
	// than stored as a duration, so the sweep is a plain timestamp
	// comparison and does not have to know when the task finished.
	if o.retention > 0 {
		retUntil = time.Now().Add(o.retention)
	}

	const q = `
		INSERT INTO jobqueue_tasks
			(task_id, queue, task_type, payload, max_retry, timeout_seconds,
			 run_after, retention_until)
		VALUES ($1, $2, $3, $4, $5, $6, now() + ($7 * interval '1 second'), $8)
		RETURNING id`

	var id int64
	err := c.pool.QueryRow(ctx, q,
		taskID, o.queue, task.typ, task.payload, o.maxRetry, timeout,
		o.processIn.Seconds(), retUntil,
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return nil, ErrTaskIDConflict
		}
		return nil, fmt.Errorf("jobqueue: enqueue %s: %w", task.typ, err)
	}

	// Wake an idle worker. Best-effort on purpose: the row is already
	// committed and the Server's poll fallback will find it regardless, so
	// a failed notification costs latency, never the task.
	if _, err := c.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, notifyChannel, o.queue); err != nil {
		// Deliberately not returned: see above.
		_ = err
	}

	return &TaskInfo{ID: id, Queue: o.queue, Type: task.typ, TaskID: o.taskID}, nil
}
