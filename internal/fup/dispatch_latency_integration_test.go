//go:build integration

// Dispatch-latency measurement for the notification pipeline.
//
// Covers DoD L5-008 and INT-NOTIF-006: a notification must reach its provider
// and be written to notification_log within 5 seconds of the task being
// enqueued.
//
// This is deliberately an end-to-end path rather than a unit test of the
// handler. The budget covers queue wait plus handler execution plus the
// provider round trip, and only a real worker pool dequeuing from the real
// queue tables can produce the first of those three. Calling ProcessTask
// directly would measure the one segment that was never in question.
//
// It is also what holds the queue's LISTEN/NOTIFY wake-up honest: the
// server's poll fallback is two seconds, so if a notification were not
// delivered the queue wait alone would consume most of the 5s budget and
// this test would start failing rather than silently degrading.
//
// Run: go test -tags=integration -run TestFR_NOTIF_009 ./internal/fup/
package fup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"github.com/maaransoft/isp-bss-oss/internal/notifications"
)

// dispatchBudget is L5-008's threshold: dequeue -> sent_at within 5s.
const dispatchBudget = 5 * time.Second

// itLatencyNotifDB is a NotifQuerier that captures the notification_log row
// the WhatsApp client writes, so the test can read the real SentAt value the
// production code stamps rather than a timestamp the test invented.
func (d *itLatencyNotifDB) ListPushTokens(_ context.Context, _ int) ([]string, error) {
	return nil, nil
}

type itLatencyNotifDB struct {
	mu       sync.Mutex
	phone    string
	logged   []notifications.NotificationLog
	loggedCh chan struct{}
	once     sync.Once
}

func (db *itLatencyNotifDB) GetSubscriber(_ context.Context, id int) (*notifications.Subscriber, error) {
	return &notifications.Subscriber{ID: id, MobileNumber: db.phone, DndOptOut: false}, nil
}

func (db *itLatencyNotifDB) CreateNotificationLog(_ context.Context, entry notifications.NotificationLog) error {
	db.mu.Lock()
	db.logged = append(db.logged, entry)
	db.mu.Unlock()
	// Signal once; the test waits on this rather than polling on a sleep loop.
	db.once.Do(func() { close(db.loggedCh) })
	return nil
}

func (db *itLatencyNotifDB) UpdateDeliveryStatus(_ context.Context, _, _ string) error {
	return nil
}

func (db *itLatencyNotifDB) entries() []notifications.NotificationLog {
	db.mu.Lock()
	defer db.mu.Unlock()
	out := make([]notifications.NotificationLog, len(db.logged))
	copy(out, db.logged)
	return out
}

// TestFR_NOTIF_009_DispatchLatencyWithinBudget enqueues a real FUP-warning
// task, lets a real worker pool dequeue it, and measures from enqueue to the
// sent_at written into notification_log by the WhatsApp client.
//
// L5-008 | INT-NOTIF-006 | FR-NOTIF-009 | FR-FUP-004
func TestFR_NOTIF_009_DispatchLatencyWithinBudget(t *testing.T) {
	pool, client, _ := itQueue(t)

	// Stub Meta Graph API. It responds immediately: the point is to measure the
	// pipeline's own overhead, not to simulate a slow provider (that belongs in
	// a timeout test, and a slow stub here would measure the stub).
	var metaHits int
	var metaMu sync.Mutex
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		metaMu.Lock()
		metaHits++
		metaMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.LATENCY-TEST"}]}`))
	}))
	defer meta.Close()

	db := &itLatencyNotifDB{phone: "+919876543210", loggedCh: make(chan struct{})}
	wa := notifications.NewWhatsAppClient("phone-id", "token", db)
	wa.SetBaseURL(meta.URL)

	// The real dispatcher and the real warning handler — no shortcut doubles on
	// the path under measurement.
	handler := NewWarningHandler(notifications.NewDispatcher(db, wa, nil))

	srv := jobqueue.NewServer(pool, jobqueue.Config{
		Concurrency: 1,
		Queues:      map[string]int{QueueNotifications: 1},
	})
	mux := jobqueue.NewServeMux()
	mux.Handle(TaskTypeFUPWarning, handler)
	if err := srv.Start(mux); err != nil {
		t.Fatalf("start worker pool: %v", err)
	}
	defer srv.Shutdown()

	payload, err := json.Marshal(WarningPayload{SubscriberID: 4242, Username: "sub4242", PctUsed: 80})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	enqueuedAt := time.Now()
	if _, err := client.Enqueue(jobqueue.NewTask(TaskTypeFUPWarning, payload), jobqueue.Queue(QueueNotifications)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Wait generously (well past the budget) so a breach is reported as a
	// measured number rather than as a timeout with no figure attached.
	select {
	case <-db.loggedCh:
	case <-time.After(30 * time.Second):
		t.Fatal("no notification_log row written within 30s — the task never completed")
	}

	entries := db.entries()
	if len(entries) != 1 {
		t.Fatalf("want exactly 1 notification_log row, got %d", len(entries))
	}
	entry := entries[0]

	if entry.SentAt.IsZero() {
		t.Fatal("sent_at is zero — nothing to measure, and FR-NOTIF-009 requires it populated")
	}
	if entry.DeliveryStatus != "sent" {
		t.Errorf("delivery_status: want \"sent\", got %q", entry.DeliveryStatus)
	}
	if entry.ProviderMessageID != "wamid.LATENCY-TEST" {
		t.Errorf("provider_message_id: want the stub's ID, got %q", entry.ProviderMessageID)
	}

	// Guard against the test passing because the provider was never called and
	// some other path wrote the row.
	metaMu.Lock()
	hits := metaHits
	metaMu.Unlock()
	if hits != 1 {
		t.Errorf("want exactly 1 call to the Meta stub, got %d", hits)
	}

	latency := entry.SentAt.Sub(enqueuedAt)
	if latency < 0 {
		t.Fatalf("sent_at precedes enqueue by %s — clock or wiring problem, not a latency result", -latency)
	}
	t.Logf("L5-008 dispatch latency: %s (budget %s)", latency, dispatchBudget)
	if latency > dispatchBudget {
		t.Errorf("dispatch latency %s exceeds the %s budget (L5-008)", latency, dispatchBudget)
	}
}

// TestFR_NOTIF_009_DispatchLatencyUnderQueueBacklog repeats the measurement
// with a backlog already queued. The single-task case above cannot detect a
// pipeline that meets the budget only when idle, which is the condition under
// which the budget actually matters.
//
// L5-008 | FR-NOTIF-009
func TestFR_NOTIF_009_DispatchLatencyUnderQueueBacklog(t *testing.T) {
	const backlog = 50

	pool, client, _ := itQueue(t)

	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.BACKLOG"}]}`))
	}))
	defer meta.Close()

	db := &itLatencyNotifDB{phone: "+919876543210", loggedCh: make(chan struct{})}
	wa := notifications.NewWhatsAppClient("phone-id", "token", db)
	wa.SetBaseURL(meta.URL)
	handler := NewWarningHandler(notifications.NewDispatcher(db, wa, nil))

	// Queue the backlog before the server starts, so every task is genuinely
	// waiting when the workers come up.
	enqueuedAt := time.Now()
	for i := 0; i < backlog; i++ {
		payload, err := json.Marshal(WarningPayload{SubscriberID: 5000 + i, Username: "sub", PctUsed: 80})
		if err != nil {
			t.Fatalf("marshal payload %d: %v", i, err)
		}
		if _, err := client.Enqueue(jobqueue.NewTask(TaskTypeFUPWarning, payload), jobqueue.Queue(QueueNotifications)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	srv := jobqueue.NewServer(pool, jobqueue.Config{
		Concurrency: 10,
		Queues:      map[string]int{QueueNotifications: 1},
	})
	mux := jobqueue.NewServeMux()
	mux.Handle(TaskTypeFUPWarning, handler)
	if err := srv.Start(mux); err != nil {
		t.Fatalf("start worker pool: %v", err)
	}
	defer srv.Shutdown()

	deadline := time.Now().Add(30 * time.Second)
	for len(db.entries()) < backlog && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	entries := db.entries()
	if len(entries) < backlog {
		t.Fatalf("only %d of %d tasks completed within 30s", len(entries), backlog)
	}

	// The budget must hold for the *worst* task in the batch, not the average —
	// an average would let a long tail hide behind fast siblings.
	var worst time.Duration
	for _, e := range entries {
		if d := e.SentAt.Sub(enqueuedAt); d > worst {
			worst = d
		}
	}
	t.Logf("L5-008 worst-case dispatch latency across %d queued tasks: %s (budget %s)", backlog, worst, dispatchBudget)
	if worst > dispatchBudget {
		t.Errorf("worst-case dispatch latency %s exceeds the %s budget (L5-008)", worst, dispatchBudget)
	}
}
