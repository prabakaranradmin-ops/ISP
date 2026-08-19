package jobqueue

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Inspector reads queue state for operational monitoring. Replaces
// asynq.Inspector, of which this codebase used exactly one thing: the
// count of archived (dead-lettered) tasks on the network-commands queue,
// which internal/fup's DeadLetterMonitor alerts on.
type Inspector struct {
	pool *pgxpool.Pool
}

// NewInspector constructs an Inspector.
func NewInspector(pool *pgxpool.Pool) *Inspector { return &Inspector{pool: pool} }

// DeadCount reports how many tasks on a queue have exhausted their retries.
//
// A rising count means work is being abandoned — CoA commands that never
// reached a NAS, notifications never delivered — which is why it is
// alerted on rather than merely logged.
func (i *Inspector) DeadCount(ctx context.Context, queue string) (int, error) {
	var n int
	err := i.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobqueue_tasks WHERE queue = $1 AND status = 'dead'`,
		queue).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("jobqueue: count dead tasks on %q: %w", queue, err)
	}
	return n, nil
}

// PendingTask is one queued, not-yet-executed task as an inspector sees it.
//
// Type and Payload are what the tests that assert on enqueued work need,
// and match the fields Asynq's own TaskInfo exposed for the same purpose.
type PendingTask struct {
	ID      int64
	Type    string
	Payload []byte
	Queue   string
	TaskID  string
}

// ListPending returns the tasks waiting on a queue, oldest first.
//
// Includes tasks whose backoff has not yet elapsed: a caller asking what is
// queued wants everything still owed, not only what is runnable this
// instant.
func (i *Inspector) ListPending(ctx context.Context, queue string) ([]PendingTask, error) {
	rows, err := i.pool.Query(ctx, `
		SELECT id, task_type, payload, queue, COALESCE(task_id, '')
		FROM jobqueue_tasks
		WHERE queue = $1 AND status = 'pending'
		ORDER BY run_after, id`, queue)
	if err != nil {
		return nil, fmt.Errorf("jobqueue: list pending on %q: %w", queue, err)
	}
	defer rows.Close()

	var out []PendingTask
	for rows.Next() {
		var t PendingTask
		if err := rows.Scan(&t.ID, &t.Type, &t.Payload, &t.Queue, &t.TaskID); err != nil {
			return nil, fmt.Errorf("jobqueue: scan pending task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskState is one task's execution state, for monitoring a specific task
// rather than a whole queue.
type TaskState struct {
	ID         int64
	Status     string
	RetryCount int
	MaxRetry   int
	LastError  string
}

// TaskByID looks up one task. Returns (nil, nil) when the row is gone,
// which is the normal end state for a task that completed without a
// retention window.
func (i *Inspector) TaskByID(ctx context.Context, id int64) (*TaskState, error) {
	var t TaskState
	err := i.pool.QueryRow(ctx, `
		SELECT id, status, retry_count, max_retry, COALESCE(last_error, '')
		FROM jobqueue_tasks WHERE id = $1`, id).Scan(
		&t.ID, &t.Status, &t.RetryCount, &t.MaxRetry, &t.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jobqueue: look up task %d: %w", id, err)
	}
	return &t, nil
}

// QueueDepth reports how many tasks are waiting to run on a queue,
// including ones whose backoff has not yet elapsed.
func (i *Inspector) QueueDepth(ctx context.Context, queue string) (int, error) {
	var n int
	err := i.pool.QueryRow(ctx,
		`SELECT count(*) FROM jobqueue_tasks WHERE queue = $1 AND status = 'pending'`,
		queue).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("jobqueue: count pending tasks on %q: %w", queue, err)
	}
	return n, nil
}
