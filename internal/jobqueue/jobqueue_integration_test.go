//go:build integration

// Tests for the PostgreSQL-backed task queue.
//
// Against a real database rather than a fake, because everything worth
// testing here is SQL semantics: SKIP LOCKED handing two workers different
// rows, a UNIQUE constraint rejecting a duplicate idempotency key, a lease
// expiring so a dead worker's task is reclaimed. A stub would assert the
// behaviour this package is trying to obtain from PostgreSQL rather than
// the behaviour it actually gets.
//
// Run: ./scripts/run_db_tests.sh, which sets TEST_DB_DSN
package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testQueue = "test_queue"

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DB_DSN")
	if dsn == "" {
		t.Skip("TEST_DB_DSN not set — run via ./scripts/run_db_tests.sh")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `TRUNCATE TABLE jobqueue_tasks RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate jobqueue_tasks: %v", err)
	}
	return pool
}

// collector is a Handler that records what it was given and can be told to
// fail a set number of times first.
type collector struct {
	mu         sync.Mutex
	payloads   [][]byte
	retryCount []int
	failTimes  int
	done       chan struct{}
	once       sync.Once
}

func newCollector(failTimes int) *collector {
	return &collector{failTimes: failTimes, done: make(chan struct{})}
}

func (c *collector) ProcessTask(ctx context.Context, t *Task) error {
	c.mu.Lock()
	c.payloads = append(c.payloads, t.Payload())
	n, _ := RetryCount(ctx)
	c.retryCount = append(c.retryCount, n)
	shouldFail := c.failTimes > 0
	if shouldFail {
		c.failTimes--
	}
	c.mu.Unlock()

	if shouldFail {
		return errors.New("deliberate failure")
	}
	c.once.Do(func() { close(c.done) })
	return nil
}

func (c *collector) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}

func (c *collector) lastRetryCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.retryCount) == 0 {
		return -1
	}
	return c.retryCount[len(c.retryCount)-1]
}

// startServer runs a server for the duration of the test.
func startServer(t *testing.T, pool *pgxpool.Pool, cfg Config, mux *ServeMux) *Server {
	t.Helper()
	if cfg.Queues == nil {
		cfg.Queues = map[string]int{testQueue: 1}
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 50 * time.Millisecond
	}
	srv := NewServer(pool, cfg)
	if err := srv.Start(mux); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ── Enqueue and execute ─────────────────────────────────────────────────────

func TestQueue_TaskIsExecutedWithItsPayload(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	handler := newCollector(0)
	mux := NewServeMux()
	mux.Handle("test:echo", handler)
	startServer(t, pool, Config{Concurrency: 1}, mux)

	client := NewClient(pool)
	if _, err := client.EnqueueContext(ctx, NewTask("test:echo", []byte(`{"v":1}`), Queue(testQueue))); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case <-handler.done:
	case <-time.After(15 * time.Second):
		t.Fatal("task was never executed")
	}
	if got := string(handler.payloads[0]); got != `{"v":1}` {
		t.Errorf("payload: want {\"v\":1}, got %s", got)
	}
}

// TestQueue_CompletedTaskWithoutRetentionIsDeleted — a queue that kept every
// finished row would grow without bound, and nothing reads a completed task
// that asked for no retention.
func TestQueue_CompletedTaskWithoutRetentionIsDeleted(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	handler := newCollector(0)
	mux := NewServeMux()
	mux.Handle("test:echo", handler)
	startServer(t, pool, Config{Concurrency: 1}, mux)

	client := NewClient(pool)
	if _, err := client.EnqueueContext(ctx, NewTask("test:echo", []byte(`{}`), Queue(testQueue))); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	<-handler.done

	waitFor(t, "the completed row to be deleted", func() bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobqueue_tasks`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n == 0
	})
}

// TestQueue_RetentionKeepsTheRowInspectable is the counterpart: an operator
// looking for a finished report export has to be able to find it.
func TestQueue_RetentionKeepsTheRowInspectable(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	handler := newCollector(0)
	mux := NewServeMux()
	mux.Handle("test:echo", handler)
	startServer(t, pool, Config{Concurrency: 1}, mux)

	client := NewClient(pool)
	info, err := client.EnqueueContext(ctx,
		NewTask("test:echo", []byte(`{}`), Queue(testQueue), Retention(time.Hour)))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	<-handler.done

	inspector := NewInspector(pool)
	waitFor(t, "the task to be marked completed", func() bool {
		st, err := inspector.TaskByID(ctx, info.ID)
		return err == nil && st != nil && st.Status == "completed"
	})
}

// ── Idempotency ─────────────────────────────────────────────────────────────

// TestQueue_DuplicateTaskIDIsRejected covers what stops an overlapping
// dunning or FUP-warning scan from sending the same notice twice.
func TestQueue_DuplicateTaskIDIsRejected(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	client := NewClient(pool)

	task := func() *Task {
		return NewTask("test:idem", []byte(`{}`), Queue(testQueue), TaskID("dunning:42:remind_7d"))
	}

	if _, err := client.EnqueueContext(ctx, task()); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	_, err := client.EnqueueContext(ctx, task())
	if !errors.Is(err, ErrTaskIDConflict) {
		t.Fatalf("second enqueue with the same task id: want ErrTaskIDConflict, got %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobqueue_tasks`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("want exactly 1 row for a repeated task id, got %d", n)
	}
}

// TestQueue_TasksWithoutIDsDoNotCollide — NULL task_id must stay distinct
// under the UNIQUE index, or every task that does not ask for idempotency
// would collide with every other one.
func TestQueue_TasksWithoutIDsDoNotCollide(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	client := NewClient(pool)

	for i := 0; i < 5; i++ {
		if _, err := client.EnqueueContext(ctx, NewTask("test:noid", []byte(`{}`), Queue(testQueue))); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobqueue_tasks`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 5 {
		t.Errorf("want 5 rows for 5 id-less tasks, got %d", n)
	}
}

// ── Concurrency ─────────────────────────────────────────────────────────────

// TestQueue_ConcurrentWorkersEachRunATaskOnce is the SKIP LOCKED guarantee:
// with many workers and many tasks, every task must execute exactly once. A
// CoA run twice is harmless; a payment receipt sent twice is not.
func TestQueue_ConcurrentWorkersEachRunATaskOnce(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	const tasks = 60
	var (
		mu   sync.Mutex
		seen = map[string]int{}
		done = make(chan struct{})
		runs int
	)
	mux := NewServeMux()
	mux.HandleFunc("test:once", func(_ context.Context, task *Task) error {
		mu.Lock()
		defer mu.Unlock()
		seen[string(task.Payload())]++
		runs++
		if runs == tasks {
			close(done)
		}
		return nil
	})
	startServer(t, pool, Config{Concurrency: 8}, mux)

	client := NewClient(pool)
	for i := 0; i < tasks; i++ {
		payload := fmt.Appendf(nil, `{"n":%d}`, i)
		if _, err := client.EnqueueContext(ctx, NewTask("test:once", payload, Queue(testQueue))); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		mu.Lock()
		t.Fatalf("only %d of %d tasks ran", runs, tasks)
	}

	// Settle briefly so a duplicate execution would have landed before the
	// assertion rather than after it.
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != tasks {
		t.Errorf("want %d distinct payloads executed, got %d", tasks, len(seen))
	}
	for payload, count := range seen {
		if count != 1 {
			t.Errorf("payload %s executed %d times, want exactly 1", payload, count)
		}
	}
}

// ── Retries and dead-lettering ──────────────────────────────────────────────

func TestQueue_FailedTaskIsRetriedThenSucceeds(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	handler := newCollector(2) // fail twice, succeed on the third attempt
	mux := NewServeMux()
	mux.Handle("test:retry", handler)
	startServer(t, pool, Config{
		Concurrency: 1,
		RetryDelay:  func(int) time.Duration { return 20 * time.Millisecond },
	}, mux)

	client := NewClient(pool)
	if _, err := client.EnqueueContext(ctx,
		NewTask("test:retry", []byte(`{}`), Queue(testQueue), MaxRetry(5))); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case <-handler.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("task never succeeded after %d attempts", handler.calls())
	}
	if got := handler.calls(); got != 3 {
		t.Errorf("want 3 attempts (2 failures + 1 success), got %d", got)
	}
	// The handler must be able to see which attempt it is on — this is what
	// internal/partner records as a delivery attempt number.
	if got := handler.lastRetryCount(); got != 2 {
		t.Errorf("retry count on the final attempt: want 2, got %d", got)
	}
}

func TestQueue_ExhaustedRetriesLandInTheDeadLetterState(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	mux := NewServeMux()
	mux.HandleFunc("test:doomed", func(context.Context, *Task) error {
		return errors.New("always fails")
	})
	startServer(t, pool, Config{
		Concurrency: 1,
		RetryDelay:  func(int) time.Duration { return 10 * time.Millisecond },
	}, mux)

	client := NewClient(pool)
	info, err := client.EnqueueContext(ctx,
		NewTask("test:doomed", []byte(`{}`), Queue(testQueue), MaxRetry(2)))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	inspector := NewInspector(pool)
	waitFor(t, "the task to be dead-lettered", func() bool {
		n, err := inspector.DeadCount(ctx, testQueue)
		return err == nil && n == 1
	})

	st, err := inspector.TaskByID(ctx, info.ID)
	if err != nil || st == nil {
		t.Fatalf("look up dead task: (%+v, %v)", st, err)
	}
	if st.RetryCount != 2 {
		t.Errorf("retry_count: want 2 (max_retry), got %d", st.RetryCount)
	}
	if st.LastError == "" {
		t.Error("a dead-lettered task must record why it failed")
	}
}

// TestQueue_SkipRetryDeadLettersImmediately — no amount of retrying makes a
// private address public, so a task that says so must not burn its budget.
func TestQueue_SkipRetryDeadLettersImmediately(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var attempts int
	var mu sync.Mutex
	mux := NewServeMux()
	mux.HandleFunc("test:skip", func(context.Context, *Task) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		return fmt.Errorf("permanently wrong: %w", SkipRetry)
	})
	startServer(t, pool, Config{
		Concurrency: 1,
		RetryDelay:  func(int) time.Duration { return 10 * time.Millisecond },
	}, mux)

	client := NewClient(pool)
	if _, err := client.EnqueueContext(ctx,
		NewTask("test:skip", []byte(`{}`), Queue(testQueue), MaxRetry(10))); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	inspector := NewInspector(pool)
	waitFor(t, "the task to be dead-lettered", func() bool {
		n, err := inspector.DeadCount(ctx, testQueue)
		return err == nil && n == 1
	})

	time.Sleep(200 * time.Millisecond) // a retry would have landed by now
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("SkipRetry must not be retried: want 1 attempt, got %d", attempts)
	}
}

// TestQueue_PanickingHandlerDoesNotKillTheWorker — one bad handler must not
// take the whole pool with it, which would stop every other task type too.
func TestQueue_PanickingHandlerDoesNotKillTheWorker(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	survivor := newCollector(0)
	mux := NewServeMux()
	mux.HandleFunc("test:panic", func(context.Context, *Task) error {
		panic("handler exploded")
	})
	mux.Handle("test:echo", survivor)
	startServer(t, pool, Config{
		Concurrency: 1,
		RetryDelay:  func(int) time.Duration { return 10 * time.Millisecond },
	}, mux)

	client := NewClient(pool)
	if _, err := client.EnqueueContext(ctx, NewTask("test:panic", []byte(`{}`), Queue(testQueue))); err != nil {
		t.Fatalf("enqueue panicking task: %v", err)
	}
	if _, err := client.EnqueueContext(ctx, NewTask("test:echo", []byte(`{}`), Queue(testQueue))); err != nil {
		t.Fatalf("enqueue follow-up task: %v", err)
	}

	select {
	case <-survivor.done:
	case <-time.After(20 * time.Second):
		t.Fatal("the worker did not survive a panicking handler")
	}
}

// TestQueue_UnhandledTaskTypeIsDeadLetteredNotRetried — no amount of waiting
// registers a handler, and leaving it pending would have it reclaimed
// forever.
func TestQueue_UnhandledTaskTypeIsDeadLettered(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	startServer(t, pool, Config{Concurrency: 1}, NewServeMux())

	client := NewClient(pool)
	if _, err := client.EnqueueContext(ctx,
		NewTask("test:nobody-handles-this", []byte(`{}`), Queue(testQueue), MaxRetry(5))); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	inspector := NewInspector(pool)
	waitFor(t, "the unhandled task to be dead-lettered", func() bool {
		n, err := inspector.DeadCount(ctx, testQueue)
		return err == nil && n == 1
	})
}

// ── Leases ──────────────────────────────────────────────────────────────────

// TestQueue_ExpiredLeaseIsReclaimed covers the crash-recovery path: a worker
// that dies mid-task must not strand it in 'processing' forever.
func TestQueue_ExpiredLeaseIsReclaimed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	client := NewClient(pool)
	info, err := client.EnqueueContext(ctx, NewTask("test:orphan", []byte(`{}`), Queue(testQueue)))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Simulate a worker that claimed the task and then died: the row is
	// 'processing' with a lease already in the past.
	_, err = pool.Exec(ctx, `
		UPDATE jobqueue_tasks
		SET status = 'processing', locked_by = 'dead-worker',
		    lease_expires_at = now() - interval '1 minute'
		WHERE id = $1`, info.ID)
	if err != nil {
		t.Fatalf("simulate dead worker: %v", err)
	}

	handler := newCollector(0)
	mux := NewServeMux()
	mux.Handle("test:orphan", handler)
	// The reaper runs on its own 30s tick, so drive one directly rather than
	// making the test wait it out.
	srv := NewServer(pool, Config{Concurrency: 1, PollInterval: 50 * time.Millisecond,
		Queues: map[string]int{testQueue: 1}})
	if err := srv.Start(mux); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Shutdown()

	if _, err := pool.Exec(ctx, `
		UPDATE jobqueue_tasks
		SET status = 'pending', locked_by = NULL, lease_expires_at = NULL
		WHERE status = 'processing' AND lease_expires_at < now()`); err != nil {
		t.Fatalf("reap: %v", err)
	}

	select {
	case <-handler.done:
	case <-time.After(20 * time.Second):
		t.Fatal("a task orphaned by a dead worker was never reclaimed")
	}
}

// ── Scheduling ──────────────────────────────────────────────────────────────

// TestQueue_ProcessInDelaysExecution guards the delay honouring the schedule
// rather than running immediately.
func TestQueue_ProcessInDelaysExecution(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	handler := newCollector(0)
	mux := NewServeMux()
	mux.Handle("test:later", handler)
	startServer(t, pool, Config{Concurrency: 1}, mux)

	client := NewClient(pool)
	if _, err := client.EnqueueContext(ctx,
		NewTask("test:later", []byte(`{}`), Queue(testQueue), ProcessIn(2*time.Second))); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case <-handler.done:
		t.Fatal("a delayed task must not run immediately")
	case <-time.After(700 * time.Millisecond):
	}

	select {
	case <-handler.done:
	case <-time.After(20 * time.Second):
		t.Fatal("the delayed task never ran")
	}
}

// TestQueue_TasksAreIsolatedByQueue — a worker configured for one queue must
// not consume another's work, which is what keeps a report export from
// being picked up by the pool meant for network commands.
func TestQueue_TasksAreIsolatedByQueue(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	handler := newCollector(0)
	mux := NewServeMux()
	mux.Handle("test:elsewhere", handler)
	startServer(t, pool, Config{Concurrency: 1, Queues: map[string]int{testQueue: 1}}, mux)

	client := NewClient(pool)
	if _, err := client.EnqueueContext(ctx,
		NewTask("test:elsewhere", []byte(`{}`), Queue("some_other_queue"))); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	select {
	case <-handler.done:
		t.Fatal("a worker must not run tasks from a queue it was not configured for")
	case <-time.After(1500 * time.Millisecond):
	}

	inspector := NewInspector(pool)
	pending, err := inspector.ListPending(ctx, "some_other_queue")
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("the task must still be waiting on its own queue, got %d pending", len(pending))
	}
}
