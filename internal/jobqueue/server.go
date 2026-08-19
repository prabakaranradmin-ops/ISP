package jobqueue

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// Handler executes one task type. Identical in shape to asynq.Handler.
type Handler interface {
	ProcessTask(ctx context.Context, t *Task) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, *Task) error

// ProcessTask implements Handler.
func (f HandlerFunc) ProcessTask(ctx context.Context, t *Task) error { return f(ctx, t) }

// ServeMux routes tasks to handlers by task type.
type ServeMux struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewServeMux constructs an empty ServeMux.
func NewServeMux() *ServeMux {
	return &ServeMux{handlers: make(map[string]Handler)}
}

// Handle registers h for taskType, replacing any previous registration.
func (m *ServeMux) Handle(taskType string, h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[taskType] = h
}

// HandleFunc registers a function for taskType.
func (m *ServeMux) HandleFunc(taskType string, fn func(context.Context, *Task) error) {
	m.Handle(taskType, HandlerFunc(fn))
}

func (m *ServeMux) handlerFor(taskType string) (Handler, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h, ok := m.handlers[taskType]
	return h, ok
}

// Config tunes a Server. Queues matches asynq.Config.Queues: a map of
// queue name to relative weight, where a higher weight means the queue is
// checked more often.
type Config struct {
	Concurrency  int
	Queues       map[string]int
	ErrorHandler func(ctx context.Context, t *Task, err error)

	// LeaseDuration is how long a claimed task stays claimed before
	// another worker may reclaim it. It must exceed the slowest handler's
	// realistic runtime, or a long task gets picked up twice while the
	// first attempt is still running.
	LeaseDuration time.Duration

	// PollInterval is the fallback sweep for tasks a NOTIFY did not
	// announce — one committed while every worker was busy, or one whose
	// notification was lost while the listener was reconnecting. Latency
	// backstop, not the primary path.
	PollInterval time.Duration

	// RetryDelay overrides how long a failed task waits before its next
	// attempt, given how many times it has already been retried. Nil uses
	// the fixed schedule in backoffSchedule.
	RetryDelay func(retryCount int) time.Duration
}

const (
	defaultLeaseDuration = 5 * time.Minute
	defaultPollInterval  = 2 * time.Second
	defaultConcurrency   = 10
	// reapInterval is how often expired leases are returned to pending.
	reapInterval = 30 * time.Second
	// retentionSweepInterval is how often finished rows past their
	// retention are deleted.
	retentionSweepInterval = 10 * time.Minute
)

// Server dequeues tasks and runs them through a ServeMux.
type Server struct {
	pool   *pgxpool.Pool
	cfg    Config
	id     string
	queues []weightedQueue

	wg     sync.WaitGroup
	cancel context.CancelFunc
	wake   chan struct{}
	closed sync.Once
}

// weightedQueue is one queue and how many slots it occupies in the
// selection sequence.
type weightedQueue struct {
	name   string
	weight int
}

// NewServer constructs a Server. It does not start until Start is called.
func NewServer(pool *pgxpool.Pool, cfg Config) *Server {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = defaultLeaseDuration
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if len(cfg.Queues) == 0 {
		cfg.Queues = map[string]int{DefaultQueue: 1}
	}

	queues := make([]weightedQueue, 0, len(cfg.Queues))
	for name, weight := range cfg.Queues {
		if weight < 1 {
			weight = 1
		}
		queues = append(queues, weightedQueue{name: name, weight: weight})
	}

	return &Server{
		pool:   pool,
		cfg:    cfg,
		id:     fmt.Sprintf("worker-%d-%d", time.Now().UnixNano(), rand.Int63()), //nolint:gosec // identity only, not a secret
		queues: queues,
		wake:   make(chan struct{}, 1),
	}
}

// Start launches the worker pool, the lease reaper, the retention sweep
// and the notification listener. Non-blocking, mirroring asynq.Server.Start.
func (s *Server) Start(mux *ServeMux) error {
	if mux == nil {
		return fmt.Errorf("jobqueue: nil mux")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	for i := 0; i < s.cfg.Concurrency; i++ {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.worker(ctx, mux)
		}()
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.listen(ctx)
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.reapExpiredLeases(ctx)
	}()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.sweepRetained(ctx)
	}()

	return nil
}

// Shutdown stops accepting new work and waits for in-flight tasks.
//
// In-flight tasks are allowed to finish rather than being cancelled: a CoA
// abandoned halfway leaves a subscriber throttled in the database but not
// on the NAS, which is exactly the split-brain this queue exists to avoid.
func (s *Server) Shutdown() {
	s.closed.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		s.wg.Wait()
	})
}

// worker is one goroutine claiming and running tasks until ctx is done.
func (s *Server) worker(ctx context.Context, mux *ServeMux) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		// Drain continuously while there is work, so a backlog is worked
		// through without waiting a poll interval per task.
		for {
			if ctx.Err() != nil {
				return
			}
			task, ok := s.claimNext(ctx)
			if !ok {
				break
			}
			s.run(ctx, mux, task)
		}

		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

// claimedTask is a row this worker holds a lease on.
type claimedTask struct {
	id         int64
	taskType   string
	payload    []byte
	queue      string
	retryCount int
	maxRetry   int
	timeout    time.Duration
	retained   bool
}

// selectionOrder returns the queue names to try, expanded by weight and
// shuffled.
//
// Weight-as-repetition rather than strict priority: strict ordering would
// let a saturated network_commands queue starve notifications entirely,
// whereas repetition makes a higher-weighted queue proportionally more
// likely to be checked first while still leaving every queue reachable.
// This is the same trade Asynq's own weighted-priority mode makes, and the
// weights in cmd/radiusd were tuned against that behaviour.
func (s *Server) selectionOrder() []string {
	total := 0
	for _, q := range s.queues {
		total += q.weight
	}
	order := make([]string, 0, total)
	for _, q := range s.queues {
		for i := 0; i < q.weight; i++ {
			order = append(order, q.name)
		}
	}
	rand.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] }) //nolint:gosec // scheduling fairness, not security
	return order
}

// claimNext atomically claims one due task, or reports that none is ready.
//
// The claim is a single short statement that both selects and locks: the
// transaction commits immediately and the handler runs outside it, so a
// twenty-second webhook delivery does not hold a database connection for
// its whole duration. SKIP LOCKED is what makes concurrent workers safe
// without any of them blocking on each other.
func (s *Server) claimNext(ctx context.Context) (*claimedTask, bool) {
	const q = `
		UPDATE jobqueue_tasks SET
			status           = 'processing',
			locked_by        = $2,
			lease_expires_at = now() + ($3 * interval '1 second')
		WHERE id = (
			SELECT id FROM jobqueue_tasks
			WHERE status = 'pending' AND queue = $1 AND run_after <= now()
			ORDER BY run_after, id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, task_type, payload, queue, retry_count, max_retry,
		          COALESCE(timeout_seconds, 0), retention_until IS NOT NULL`

	for _, queue := range s.selectionOrder() {
		var (
			t              claimedTask
			timeoutSeconds int
		)
		err := s.pool.QueryRow(ctx, q, queue, s.id, s.cfg.LeaseDuration.Seconds()).Scan(
			&t.id, &t.taskType, &t.payload, &t.queue, &t.retryCount, &t.maxRetry,
			&timeoutSeconds, &t.retained)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // nothing due on this queue; try the next
		}
		if err != nil {
			if ctx.Err() == nil {
				log.Error().Err(err).Str("queue", queue).Msg("jobqueue: claim failed")
			}
			return nil, false
		}
		t.timeout = time.Duration(timeoutSeconds) * time.Second
		return &t, true
	}
	return nil, false
}

// run executes one claimed task and records the outcome.
func (s *Server) run(ctx context.Context, mux *ServeMux, ct *claimedTask) {
	handler, ok := mux.handlerFor(ct.taskType)
	if !ok {
		// No registered handler. Dead-lettered rather than retried: no
		// amount of waiting registers a handler, and leaving it pending
		// would have it reclaimed forever.
		s.finish(ctx, ct, fmt.Errorf("jobqueue: no handler registered for %q", ct.taskType), true)
		return
	}

	// Detached from the server's lifecycle context so an in-flight task is
	// not cancelled by Shutdown — see Shutdown's comment.
	runCtx := context.WithoutCancel(ctx)
	var cancel context.CancelFunc
	if ct.timeout > 0 {
		runCtx, cancel = context.WithTimeout(runCtx, ct.timeout)
		defer cancel()
	}
	runCtx = withRetryCount(runCtx, ct.retryCount)

	err := func() (err error) {
		// A panicking handler must not take the worker pool with it.
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("jobqueue: handler panicked: %v", r)
			}
		}()
		return handler.ProcessTask(runCtx, &Task{typ: ct.taskType, payload: ct.payload})
	}()

	if err != nil && s.cfg.ErrorHandler != nil {
		s.cfg.ErrorHandler(runCtx, &Task{typ: ct.taskType, payload: ct.payload}, err)
	}
	s.finish(ctx, ct, err, errors.Is(err, SkipRetry))
}

// finish records a task's outcome: completed, scheduled for retry, or dead.
func (s *Server) finish(ctx context.Context, ct *claimedTask, taskErr error, skipRetry bool) {
	// The claiming context may already be cancelled by a shutdown; the
	// outcome still has to be written or the task would be reclaimed and
	// re-run after having actually succeeded.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if taskErr == nil {
		var err error
		if ct.retained {
			_, err = s.pool.Exec(writeCtx, `
				UPDATE jobqueue_tasks
				SET status = 'completed', completed_at = now(),
				    locked_by = NULL, lease_expires_at = NULL, last_error = NULL
				WHERE id = $1`, ct.id)
		} else {
			// No retention requested: the row's only remaining purpose was
			// to be run, so it goes rather than accumulating.
			_, err = s.pool.Exec(writeCtx, `DELETE FROM jobqueue_tasks WHERE id = $1`, ct.id)
		}
		if err != nil {
			log.Error().Err(err).Int64("task", ct.id).Msg("jobqueue: could not record completion")
		}
		return
	}

	exhausted := skipRetry || ct.retryCount >= ct.maxRetry
	if exhausted {
		_, err := s.pool.Exec(writeCtx, `
			UPDATE jobqueue_tasks
			SET status = 'dead', completed_at = now(), last_error = $2,
			    locked_by = NULL, lease_expires_at = NULL
			WHERE id = $1`, ct.id, taskErr.Error())
		if err != nil {
			log.Error().Err(err).Int64("task", ct.id).Msg("jobqueue: could not dead-letter task")
		}
		return
	}

	delay := backoff(ct.retryCount)
	if s.cfg.RetryDelay != nil {
		delay = s.cfg.RetryDelay(ct.retryCount)
	}
	_, err := s.pool.Exec(writeCtx, `
		UPDATE jobqueue_tasks
		SET status = 'pending', retry_count = retry_count + 1, last_error = $2,
		    run_after = now() + ($3 * interval '1 second'),
		    locked_by = NULL, lease_expires_at = NULL
		WHERE id = $1`, ct.id, taskErr.Error(), delay.Seconds())
	if err != nil {
		log.Error().Err(err).Int64("task", ct.id).Msg("jobqueue: could not schedule retry")
	}
}

// listen waits on the NOTIFY channel and wakes a worker per notification.
//
// A dedicated connection, taken out of the pool for its whole life:
// LISTEN is connection-scoped, so a pooled connection handed back between
// notifications would silently stop listening.
func (s *Server) listen(ctx context.Context) {
	for ctx.Err() == nil {
		if err := s.listenOnce(ctx); err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Msg("jobqueue: notification listener dropped, retrying")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}
}

func (s *Server) listenOnce(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire listener connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		s.signal()
	}
}

// signal nudges one idle worker. Non-blocking: the channel has depth one,
// and a wake-up already pending is as good as a second one.
func (s *Server) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// reapExpiredLeases returns tasks whose worker died to the pending pool.
func (s *Server) reapExpiredLeases(ctx context.Context) {
	ticker := time.NewTicker(reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tag, err := s.pool.Exec(ctx, `
				UPDATE jobqueue_tasks
				SET status = 'pending', locked_by = NULL, lease_expires_at = NULL,
				    last_error = 'lease expired; worker presumed dead'
				WHERE status = 'processing' AND lease_expires_at < now()`)
			if err != nil {
				if ctx.Err() == nil {
					log.Error().Err(err).Msg("jobqueue: lease reaper failed")
				}
				continue
			}
			if n := tag.RowsAffected(); n > 0 {
				log.Warn().Int64("tasks", n).Msg("jobqueue: reclaimed tasks from expired leases")
				s.signal()
			}
		}
	}
}

// sweepRetained deletes finished rows whose retention window has passed.
func (s *Server) sweepRetained(ctx context.Context) {
	ticker := time.NewTicker(retentionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := s.pool.Exec(ctx, `
				DELETE FROM jobqueue_tasks
				WHERE retention_until IS NOT NULL AND retention_until < now()
				  AND status IN ('completed','dead')`)
			if err != nil && ctx.Err() == nil {
				log.Error().Err(err).Msg("jobqueue: retention sweep failed")
			}
		}
	}
}

// backoffSchedule is a fixed table rather than a computed shift.
//
// Deliberately a lookup: an exponential computed from retry_count would
// overflow or produce absurd delays if a row's counter were ever corrupted
// or a max_retry raised, and the ceiling here is explicit and readable.
// Mirrors the schedule internal/partner/dispatch.go already documents for
// its own delivery-attempt records.
var backoffSchedule = []time.Duration{
	10 * time.Second,
	30 * time.Second,
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	30 * time.Minute,
	1 * time.Hour,
	2 * time.Hour,
}

func backoff(retryCount int) time.Duration {
	if retryCount < 0 {
		retryCount = 0
	}
	if retryCount >= len(backoffSchedule) {
		return backoffSchedule[len(backoffSchedule)-1]
	}
	return backoffSchedule[retryCount]
}

// retryCountKey types the context value carrying a task's retry count.
type retryCountKey struct{}

func withRetryCount(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, retryCountKey{}, n)
}

// RetryCount reports how many times the running task has already been
// retried, and whether that information was available. Replaces
// asynq.GetRetryCount, which internal/partner uses to record a delivery
// attempt's number.
func RetryCount(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(retryCountKey{}).(int)
	return n, ok
}
