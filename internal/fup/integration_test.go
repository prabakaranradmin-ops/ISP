//go:build integration

// Integration tests for the FUP scanner, CoA sender and dead-letter monitor.
//
// Covers INT-FUP-001 .. INT-FUP-004 and INT-NOTIF-005 from the Integration Tests
// tracker sheet. The tracker lists the last three under ./internal/tasks/; the
// task handlers actually live in this package, so they are exercised here.
//
// The queue runs against a real PostgreSQL (internal/jobqueue, migration
// 037), so queue routing, idempotency keys and dead-lettering are exercised
// through the genuine client and inspector rather than mocked. These ran
// against a real in-process Redis (miniredis) for the same reason before
// the queue moved off Asynq.
//
// Run: ./scripts/run_db_tests.sh, which sets TEST_DB_DSN
package fup

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"layeh.com/radius"
)

// ── Test doubles ────────────────────────────────────────────────────────────

// itFUPDB is an in-memory FUPQuerier.
type itFUPDB struct {
	mu          sync.Mutex
	aboveFUP    []SessionStats
	atWarning   []SessionStats
	fupActiveOn map[int]bool
}

func (db *itFUPDB) GetActiveSessionsAboveFUP(context.Context) ([]SessionStats, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.aboveFUP, nil
}

func (db *itFUPDB) GetSessionsAtWarning(context.Context, int) ([]SessionStats, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.atWarning, nil
}

func (db *itFUPDB) SetFUPActive(_ context.Context, subscriberID int, active bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.fupActiveOn == nil {
		db.fupActiveOn = map[int]bool{}
	}
	db.fupActiveOn[subscriberID] = active
	return nil
}

// itCoADB is an in-memory CoAQuerier pointing at a stub NAS.
type itCoADB struct {
	nasIP     string
	sessionID string
	rateLimit string
}

func (db *itCoADB) GetSubscriberNASSession(context.Context, int) (string, string, string, int, error) {
	return db.nasIP, db.sessionID, db.rateLimit, 0, nil
}

// itAlerter records the alerts fired at it.
type itAlerter struct {
	mu     sync.Mutex
	events []string
}

func (a *itAlerter) Trigger(event string, _ any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event)
}

func (a *itAlerter) WasTriggered(event string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.events {
		if e == event {
			return true
		}
	}
	return false
}

// itNotifier records the notifications the warning handler dispatches.
type itNotifier struct {
	mu    sync.Mutex
	calls []itNotifyCall
	err   error
}

type itNotifyCall struct {
	SubscriberID int
	TemplateID   string
	TriggerEvent string
	Vars         []string
}

func (n *itNotifier) Notify(_ context.Context, subscriberID int, templateID, triggerEvent string, vars []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	n.calls = append(n.calls, itNotifyCall{subscriberID, templateID, triggerEvent, vars})
	return nil
}

func (n *itNotifier) snapshot() []itNotifyCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]itNotifyCall(nil), n.calls...)
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// itQueue returns a pool, client and inspector against the real queue
// tables, emptied first so a test starts from a known state.
func itQueue(t *testing.T) (*pgxpool.Pool, *jobqueue.Client, *jobqueue.Inspector) {
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
	return pool, jobqueue.NewClient(pool), jobqueue.NewInspector(pool)
}

// itPendingTasks lists pending tasks on a queue.
func itPendingTasks(t *testing.T, inspector *jobqueue.Inspector, queue string) []jobqueue.PendingTask {
	t.Helper()
	tasks, err := inspector.ListPending(context.Background(), queue)
	if err != nil {
		t.Fatalf("list %s: %v", queue, err)
	}
	return tasks
}

// itDeadLetter drives a task to the terminal dead state directly.
//
// Exhausting retries is what dead-letters a task in production; forcing the
// state reaches the same place without burning the backoff delays, which is
// the same shortcut the Asynq version of these tests took with ArchiveTask.
func itDeadLetter(t *testing.T, pool *pgxpool.Pool, taskID int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE jobqueue_tasks SET status = 'dead', completed_at = now() WHERE id = $1`, taskID)
	if err != nil {
		t.Fatalf("dead-letter task %d: %v", taskID, err)
	}
}

func itCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func itCounterVecValue(t *testing.T, v *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	c, err := v.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("counter vec %v: %v", labels, err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter vec: %v", err)
	}
	return m.GetCounter().GetValue()
}

// itStubNAS listens on 127.0.0.1 and answers CoA-Requests with the given code.
// It returns the port it bound and a channel counting received requests.
func itStubNAS(t *testing.T, secret []byte, respondWith radius.Code) (int, <-chan struct{}) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("stub NAS listen: %v", err)
	}
	received := make(chan struct{}, 64)

	done := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		_ = conn.Close()
	})

	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				select {
				case <-done:
					return
				default:
					return
				}
			}
			req, err := radius.Parse(buf[:n], secret)
			if err != nil {
				continue
			}
			select {
			case received <- struct{}{}:
			default:
			}
			resp := req.Response(respondWith)
			encoded, err := resp.Encode()
			if err != nil {
				continue
			}
			_, _ = conn.WriteToUDP(encoded, addr)
		}
	}()

	return conn.LocalAddr().(*net.UDPAddr).Port, received
}

// ── INT-FUP-001 ─────────────────────────────────────────────────────────────

// TestFR_FUP_001_FUPScanner_EnqueuesCoAOn100Pct verifies a session at or above its FUP
// threshold enqueues exactly one CoA task on the network_commands queue and
// flips fup_active.
//
// INT-FUP-001 | FR-FUP-001
func TestFR_FUP_001_FUPScanner_EnqueuesCoAOn100Pct(t *testing.T) {
	_, client, inspector := itQueue(t)

	const threshold = int64(1_771_674_009_600) // 1.65 TB
	db := &itFUPDB{
		aboveFUP: []SessionStats{{
			SubscriberID: 42,
			Username:     "heavy@isp",
			NasIP:        "10.10.0.1",
			FUPThreshold: threshold,
			BytesUsed:    threshold + 1,
		}},
	}

	scanner := NewScanner(db, client)
	if err := scanner.scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	pending := itPendingTasks(t, inspector, QueueNetCommands)
	if len(pending) != 1 {
		t.Fatalf("%s queue: want 1 task, got %d", QueueNetCommands, len(pending))
	}
	if pending[0].Type != TaskTypeCoA {
		t.Errorf("task type: want %q, got %q", TaskTypeCoA, pending[0].Type)
	}

	var payload CoAPayload
	if err := json.Unmarshal(pending[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal CoA payload: %v", err)
	}
	if payload.SubscriberID != 42 {
		t.Errorf("payload subscriber_id: want 42, got %d", payload.SubscriberID)
	}
	if payload.NasIP != "10.10.0.1" {
		t.Errorf("payload nas_ip: want 10.10.0.1, got %q", payload.NasIP)
	}
	if !db.fupActiveOn[42] {
		t.Error("expected fup_active to be set for the breaching subscriber")
	}
}

// TestFR_FUP_001_FUPScanner_SkipsUnlimitedAndAlreadyThrottled verifies unlimited plans and
// already-throttled sessions do not generate CoA traffic.
//
// INT-FUP-001 (supporting) | FR-FUP-001
func TestFR_FUP_001_FUPScanner_SkipsUnlimitedAndAlreadyThrottled(t *testing.T) {
	_, client, inspector := itQueue(t)

	db := &itFUPDB{
		aboveFUP: []SessionStats{
			{SubscriberID: 1, Username: "unlimited@isp", FUPThreshold: 0, BytesUsed: 9_999_999_999},
			{SubscriberID: 2, Username: "throttled@isp", FUPThreshold: 100, BytesUsed: 500, FUPActive: true},
		},
	}

	scanner := NewScanner(db, client)
	if err := scanner.scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if pending := itPendingTasks(t, inspector, QueueNetCommands); len(pending) != 0 {
		t.Errorf("want no CoA tasks, got %d", len(pending))
	}
}

// ── INT-FUP-002 ─────────────────────────────────────────────────────────────

// TestFR_FUP_004_FUPScanner_Warns80Pct verifies an 80%-of-quota session enqueues one
// warning notification, and that a repeat scan does not enqueue a second.
//
// INT-FUP-002 | FR-FUP-004
func TestFR_FUP_004_FUPScanner_Warns80Pct(t *testing.T) {
	_, client, inspector := itQueue(t)

	const threshold = int64(3_543_348_019_200) // 3.3 TB
	db := &itFUPDB{
		atWarning: []SessionStats{{
			SubscriberID: 77,
			Username:     "nearly@isp",
			NasIP:        "10.10.0.2",
			FUPThreshold: threshold,
			BytesUsed:    threshold * 82 / 100,
		}},
	}

	before := itCounterValue(t, fupWarningEnqueued)
	scanner := NewScanner(db, client)
	if err := scanner.scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	pending := itPendingTasks(t, inspector, QueueNotifications)
	if len(pending) != 1 {
		t.Fatalf("%s queue: want 1 task, got %d", QueueNotifications, len(pending))
	}
	if pending[0].Type != TaskTypeFUPWarning {
		t.Errorf("task type: want %q, got %q", TaskTypeFUPWarning, pending[0].Type)
	}

	var payload WarningPayload
	if err := json.Unmarshal(pending[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal warning payload: %v", err)
	}
	if payload.SubscriberID != 77 || payload.Username != "nearly@isp" {
		t.Errorf("payload: got %+v", payload)
	}
	if payload.PctUsed < FUPWarningPct || payload.PctUsed >= 100 {
		t.Errorf("pct_used: want between %d and 99, got %d", FUPWarningPct, payload.PctUsed)
	}
	if got := itCounterValue(t, fupWarningEnqueued); got != before+1 {
		t.Errorf("fup_warning_enqueued_total: want +1, got %v", got-before)
	}

	// The scanner runs every 10s; the idempotency key must stop the same
	// subscriber being warned again on the next tick.
	if err := scanner.scan(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	pending = itPendingTasks(t, inspector, QueueNotifications)
	if len(pending) != 1 {
		t.Errorf("idempotency key failed: want 1 task after rescan, got %d", len(pending))
	}
	if got := itCounterValue(t, fupWarningEnqueued); got != before+1 {
		t.Errorf("rescan must not count a second enqueue, delta %v", got-before)
	}
}

// TestFR_FUP_004_FUPScanner_WarningTaskIDIsPerQuotaCycle verifies the idempotency key
// distinguishes subscribers and quota cycles.
//
// INT-FUP-002 (supporting) | FR-FUP-004
func TestFR_FUP_004_FUPScanner_WarningTaskIDIsPerQuotaCycle(t *testing.T) {
	if a, b := WarningTaskID(1, 100), WarningTaskID(2, 100); a == b {
		t.Errorf("different subscribers must get different task IDs, both %q", a)
	}
	if a, b := WarningTaskID(1, 100), WarningTaskID(1, 200); a == b {
		t.Errorf("different quota cycles must get different task IDs, both %q", a)
	}
}

// ── INT-FUP-003 ─────────────────────────────────────────────────────────────

// TestFR_FUP_002_CoATask_RetriesOnNAK verifies a CoA-NAK from the NAS produces an error
// from ProcessTask (which is what drives the Asynq retry) and counts a nak.
//
// INT-FUP-003 | FR-FUP-002
func TestFR_FUP_002_CoATask_RetriesOnNAK(t *testing.T) {
	secret := []byte("coa-secret")
	port, received := itStubNAS(t, secret, radius.CodeCoANAK)

	handler := NewCoAHandler(&itCoADB{
		nasIP:     "127.0.0.1",
		sessionID: "sess-nak-001",
		rateLimit: "10M/10M",
	}, secret)
	handler.SetPort(port)

	beforeNAK := itCounterVecValue(t, coaAckTotal, "nak")

	payload, _ := json.Marshal(CoAPayload{SubscriberID: 9, NasIP: "127.0.0.1"})
	err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeCoA, payload))

	if err == nil {
		t.Fatal("CoA-NAK must return an error so Asynq retries the task")
	}
	select {
	case <-received:
	default:
		t.Error("stub NAS received no CoA-Request")
	}
	if got := itCounterVecValue(t, coaAckTotal, "nak"); got != beforeNAK+1 {
		t.Errorf("fup_coa_ack_total{result=nak}: want +1, got %v", got-beforeNAK)
	}
}

// TestFR_FUP_002_CoATask_SucceedsOnACK verifies a CoA-ACK completes the task without error.
//
// INT-FUP-003 (supporting) | FR-FUP-002
func TestFR_FUP_002_CoATask_SucceedsOnACK(t *testing.T) {
	secret := []byte("coa-secret")
	port, _ := itStubNAS(t, secret, radius.CodeCoAACK)

	handler := NewCoAHandler(&itCoADB{
		nasIP:     "127.0.0.1",
		sessionID: "sess-ack-001",
		rateLimit: "10M/10M",
	}, secret)
	handler.SetPort(port)

	beforeACK := itCounterVecValue(t, coaAckTotal, "ack")

	payload, _ := json.Marshal(CoAPayload{SubscriberID: 10, NasIP: "127.0.0.1"})
	if err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeCoA, payload)); err != nil {
		t.Fatalf("CoA-ACK must succeed, got: %v", err)
	}
	if got := itCounterVecValue(t, coaAckTotal, "ack"); got != beforeACK+1 {
		t.Errorf("fup_coa_ack_total{result=ack}: want +1, got %v", got-beforeACK)
	}
}

// TestFR_FUP_002_CoATask_QueueRetriesFailedTask drives the failure through a
// live worker pool and asserts the task is actually re-attempted rather than
// dropped.
//
// INT-FUP-003 | FR-FUP-002
func TestFR_FUP_002_CoATask_QueueRetriesFailedTask(t *testing.T) {
	pool, client, inspector := itQueue(t)

	secret := []byte("coa-secret")
	port, _ := itStubNAS(t, secret, radius.CodeCoANAK)
	handler := NewCoAHandler(&itCoADB{
		nasIP:     "127.0.0.1",
		sessionID: "sess-retry-001",
		rateLimit: "10M/10M",
	}, secret)
	handler.SetPort(port)

	srv := jobqueue.NewServer(pool, jobqueue.Config{
		Concurrency: 1,
		Queues:      map[string]int{QueueNetCommands: 1},
		// The production schedule starts at 10s, which would make this test
		// wait out a real backoff to observe a retry that is not about
		// timing.
		RetryDelay:   func(int) time.Duration { return 50 * time.Millisecond },
		PollInterval: 50 * time.Millisecond,
	})
	mux := jobqueue.NewServeMux()
	mux.Handle(TaskTypeCoA, handler)
	if err := srv.Start(mux); err != nil {
		t.Fatalf("start worker pool: %v", err)
	}
	defer srv.Shutdown()

	payload, _ := json.Marshal(CoAPayload{SubscriberID: 11, NasIP: "127.0.0.1"})
	info, err := client.Enqueue(
		jobqueue.NewTask(TaskTypeCoA, payload),
		jobqueue.Queue(QueueNetCommands),
		jobqueue.MaxRetry(5),
	)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		task, err := inspector.TaskByID(context.Background(), info.ID)
		if err == nil && task != nil && task.RetryCount >= 1 {
			return // the NAK was retried, which is what INT-FUP-003 asserts
		}
		if time.Now().After(deadline) {
			retried := -1
			if err == nil && task != nil {
				retried = task.RetryCount
			}
			t.Fatalf("task was not retried after CoA-NAK (retried=%d, lookup err=%v)", retried, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ── PoD (session-control disconnect) ────────────────────────────────────────

// TestFR_NET_002_PoDTask_SucceedsOnACK verifies a Disconnect-ACK completes the task
// without error.
//
// FR-NET-002
func TestFR_NET_002_PoDTask_SucceedsOnACK(t *testing.T) {
	secret := []byte("pod-secret")
	port, received := itStubNAS(t, secret, radius.CodeDisconnectACK)

	handler := NewPoDHandler(&itCoADB{
		nasIP:     "127.0.0.1",
		sessionID: "sess-pod-ack-001",
	}, secret)
	handler.SetPort(port)

	beforeACK := itCounterVecValue(t, podAckTotal, "ack")

	payload, _ := json.Marshal(PoDPayload{SubscriberID: 20})
	if err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypePoD, payload)); err != nil {
		t.Fatalf("Disconnect-ACK must succeed, got: %v", err)
	}
	select {
	case <-received:
	default:
		t.Error("stub NAS received no Disconnect-Request")
	}
	if got := itCounterVecValue(t, podAckTotal, "ack"); got != beforeACK+1 {
		t.Errorf("fup_pod_ack_total{result=ack}: want +1, got %v", got-beforeACK)
	}
}

// TestFR_NET_002_PoDTask_RetriesOnNAK verifies a Disconnect-NAK produces an error so
// Asynq retries, mirroring the CoA-NAK behaviour.
//
// FR-NET-002
func TestFR_NET_002_PoDTask_RetriesOnNAK(t *testing.T) {
	secret := []byte("pod-secret")
	port, _ := itStubNAS(t, secret, radius.CodeDisconnectNAK)

	handler := NewPoDHandler(&itCoADB{
		nasIP:     "127.0.0.1",
		sessionID: "sess-pod-nak-001",
	}, secret)
	handler.SetPort(port)

	beforeNAK := itCounterVecValue(t, podAckTotal, "nak")

	payload, _ := json.Marshal(PoDPayload{SubscriberID: 21})
	err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypePoD, payload))

	if err == nil {
		t.Fatal("Disconnect-NAK must return an error so Asynq retries the task")
	}
	if got := itCounterVecValue(t, podAckTotal, "nak"); got != beforeNAK+1 {
		t.Errorf("fup_pod_ack_total{result=nak}: want +1, got %v", got-beforeNAK)
	}
}

// TestFR_NET_002_PoDTask_NoLiveSessionSkipsRetry verifies that a subscriber with no open
// session (already disconnected, or the task ran very late) is not retried:
// there is nothing left to disconnect, so retrying can never succeed.
//
// FR-NET-002
func TestFR_NET_002_PoDTask_NoLiveSessionSkipsRetry(t *testing.T) {
	handler := NewPoDHandler(&itNoSessionDB{}, []byte("pod-secret"))

	payload, _ := json.Marshal(PoDPayload{SubscriberID: 22})
	err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypePoD, payload))

	if err == nil {
		t.Fatal("expected an error when there is no live session")
	}
	if !errorIsSkipRetry(err) {
		t.Errorf("a missing session must wrap jobqueue.SkipRetry, got: %v", err)
	}
}

// itNoSessionDB reports every subscriber as having no live session.
// Not internal/db.ErrNotFound: this package cannot import internal/db without
// creating an import cycle, since internal/db already depends on this
// package's CoAQuerier interface. Any error works — PoDHandler only cares
// that GetSubscriberNASSession failed, not which sentinel it returned.
type itNoSessionDB struct{}

func (itNoSessionDB) GetSubscriberNASSession(context.Context, int) (string, string, string, int, error) {
	return "", "", "", 0, errors.New("no active session")
}

// ── INT-FUP-004 ─────────────────────────────────────────────────────────────

// TestFR_FUP_003_DeadLetterMonitor_AlertsOnNonZero verifies an archived (dead-lettered)
// task raises the dead_letter_queue_non_empty alert.
//
// INT-FUP-004 | FR-FUP-003
func TestFR_FUP_003_DeadLetterMonitor_AlertsOnNonZero(t *testing.T) {
	pool, client, inspector := itQueue(t)

	payload, _ := json.Marshal(CoAPayload{SubscriberID: 12, NasIP: "10.0.0.9"})
	info, err := client.Enqueue(jobqueue.NewTask(TaskTypeCoA, payload), jobqueue.Queue(QueueNetCommands))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	itDeadLetter(t, pool, info.ID)

	alerter := &itAlerter{}
	monitor := NewDeadLetterMonitor(inspector, alerter)

	if err := monitor.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}

	if !alerter.WasTriggered("dead_letter_queue_non_empty") {
		t.Error("expected dead_letter_queue_non_empty alert for a dead-lettered task")
	}
}

// TestFR_FUP_003_DeadLetterMonitor_SilentWhenEmpty verifies no alert fires on a clean queue.
//
// INT-FUP-004 (supporting) | FR-FUP-003
func TestFR_FUP_003_DeadLetterMonitor_SilentWhenEmpty(t *testing.T) {
	_, client, inspector := itQueue(t)

	payload, _ := json.Marshal(CoAPayload{SubscriberID: 13, NasIP: "10.0.0.10"})
	if _, err := client.Enqueue(jobqueue.NewTask(TaskTypeCoA, payload), jobqueue.Queue(QueueNetCommands)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	alerter := &itAlerter{}
	monitor := NewDeadLetterMonitor(inspector, alerter)

	if err := monitor.checkOnce(context.Background()); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}

	if alerter.WasTriggered("dead_letter_queue_non_empty") {
		t.Error("no alert expected while the queue has no dead-lettered tasks")
	}
}

// TestFR_FUP_003_DeadLetterMonitor_RunAlertsOnTick verifies the polling loop fires the
// alert without needing the caller to drive checkOnce.
//
// INT-FUP-004 (supporting) | FR-FUP-003
func TestFR_FUP_003_DeadLetterMonitor_RunAlertsOnTick(t *testing.T) {
	pool, client, inspector := itQueue(t)

	payload, _ := json.Marshal(CoAPayload{SubscriberID: 14, NasIP: "10.0.0.11"})
	info, err := client.Enqueue(jobqueue.NewTask(TaskTypeCoA, payload), jobqueue.Queue(QueueNetCommands))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	itDeadLetter(t, pool, info.ID)

	alerter := &itAlerter{}
	monitor := NewDeadLetterMonitor(inspector, alerter)
	monitor.SetInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go monitor.Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for !alerter.WasTriggered("dead_letter_queue_non_empty") {
		if time.Now().After(deadline) {
			t.Fatal("monitor loop did not raise dead_letter_queue_non_empty within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ── INT-NOTIF-005 ───────────────────────────────────────────────────────────

// TestFR_FUP_004_FUPWarningTask_DispatchesWhatsApp verifies the warning task handler
// dispatches template TMPL-001 for the subscriber in the payload.
//
// INT-NOTIF-005 | FR-FUP-004
func TestFR_FUP_004_FUPWarningTask_DispatchesWhatsApp(t *testing.T) {
	notifier := &itNotifier{}
	handler := NewWarningHandler(notifier)

	payload, _ := json.Marshal(WarningPayload{SubscriberID: 55, Username: "nearly@isp", PctUsed: 82})
	if err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeFUPWarning, payload)); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	calls := notifier.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 notification dispatched, got %d", len(calls))
	}
	if calls[0].TemplateID != TemplateFUPWarning {
		t.Errorf("template_id: want %q, got %q", TemplateFUPWarning, calls[0].TemplateID)
	}
	if calls[0].SubscriberID != 55 {
		t.Errorf("subscriber_id: want 55, got %d", calls[0].SubscriberID)
	}
	if calls[0].TriggerEvent != "fup_warning_80pct" {
		t.Errorf("trigger event: want fup_warning_80pct, got %q", calls[0].TriggerEvent)
	}
}

// TestFR_FUP_004_FUPWarningTask_MalformedPayloadSkipsRetry verifies an undecodable payload
// is not retried, since it can never succeed.
//
// INT-NOTIF-005 (supporting) | FR-FUP-004
func TestFR_FUP_004_FUPWarningTask_MalformedPayloadSkipsRetry(t *testing.T) {
	handler := NewWarningHandler(&itNotifier{})

	err := handler.ProcessTask(context.Background(), jobqueue.NewTask(TaskTypeFUPWarning, []byte("{not json")))
	if err == nil {
		t.Fatal("expected an error for a malformed payload")
	}
	if !errorIsSkipRetry(err) {
		t.Errorf("malformed payload must wrap jobqueue.SkipRetry, got: %v", err)
	}
}

func errorIsSkipRetry(err error) bool {
	for e := err; e != nil; {
		if e == jobqueue.SkipRetry { //nolint:errorlint,err113
			return true
		}
		unwrapper, ok := e.(interface{ Unwrap() []error })
		if ok {
			for _, sub := range unwrapper.Unwrap() {
				if errorIsSkipRetry(sub) {
					return true
				}
			}
			return false
		}
		single, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = single.Unwrap()
	}
	return false
}
