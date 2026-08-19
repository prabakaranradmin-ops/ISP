//go:build integration

// Hotspot rate-limit enforcement — FR-HSP-003 | migration 034 | MDS §4.23.
//
// FR-HSP-003 says a hotspot session is shaped by the same plan and policed by
// the same CoA machinery as a PPPoE one. The MAB handler already carries the
// plan's rate limit into the Access-Accept (internal/radius/mab.go), and the
// persistence tests already prove AuthorizeMAC returns it. What neither covers
// is the enforcement half: that a hotspot user who blows through their quota
// actually gets throttled, rather than the FUP scanner quietly stepping over a
// session it does not recognise as one of its own.
//
// So this test drives the whole path against real infrastructure and asserts
// the packet that comes out the far end:
//
//	hotspot device authorised by MAB
//	  → live session over the plan's FUP threshold in PostgreSQL
//	    → FUP scanner enqueues a CoA task through a real queue client
//	      → CoA handler resolves the session and sends a CoA-Request over UDP
//	        → the NAS receives a packet carrying the *throttled* rate limit
//
// The only stand-in is the NAS itself, which answers CoA-ACK. Everything
// before it — the SQL, the queue, the RADIUS encoding, the socket — is real,
// because every defect this test is meant to catch lives in one of those.
package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	"github.com/maaransoft/isp-bss-oss/internal/jobqueue"
	"layeh.com/radius"
)

// hotspotCoASecret is the shared secret both the CoA sender and the stub NAS
// authenticate the exchange with.
var hotspotCoASecret = []byte("hotspot-coa-secret")

// coaCapture is a stub NAS that records the CoA-Requests it receives.
type coaCapture struct {
	port     int
	received chan *radius.Packet
}

// startCoAStubNAS listens on loopback and answers every request with ackCode,
// publishing each parsed request on the returned channel.
func startCoAStubNAS(t *testing.T, secret []byte, ackCode radius.Code) *coaCapture {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("stub NAS listen: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	stub := &coaCapture{
		port:     conn.LocalAddr().(*net.UDPAddr).Port,
		received: make(chan *radius.Packet, 8),
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // the listener was closed by t.Cleanup
			}
			req, err := radius.Parse(buf[:n], secret)
			if err != nil {
				// A parse failure here means the secret or the encoding is
				// wrong, which is precisely a defect this test should surface —
				// but this goroutine cannot call t.Fatalf, so it drops the
				// packet and the assertion below fails on the empty channel.
				continue
			}
			select {
			case stub.received <- req:
			default:
			}
			resp, err := req.Response(ackCode).Encode()
			if err != nil {
				continue
			}
			_, _ = conn.WriteToUDP(resp, addr)
		}
	}()
	return stub
}

// pendingCoATasks lists the queued CoA tasks.
//
// A queue nothing has been enqueued on reads back as an empty list, which is
// precisely the state the under-quota test asserts. (The Asynq version of
// this helper had to special-case that: it reported ErrQueueNotFound rather
// than an empty list when a queue had never received a task.)
func pendingCoATasks(t *testing.T, inspector *jobqueue.Inspector) []jobqueue.PendingTask {
	t.Helper()
	tasks, err := inspector.ListPending(context.Background(), fup.QueueNetCommands)
	if err != nil {
		t.Fatalf("list pending CoA tasks: %v", err)
	}
	return tasks
}

// awaitCoA waits for one captured request.
func (c *coaCapture) awaitCoA(t *testing.T) *radius.Packet {
	t.Helper()
	select {
	case pkt := <-c.received:
		return pkt
	case <-time.After(5 * time.Second):
		t.Fatal("the NAS received no CoA-Request — an over-quota hotspot session was never throttled")
		return nil
	}
}

// containsRateLimit reports whether any Vendor-Specific attribute in pkt
// carries want.
//
// Substring rather than exact decoding of the MikroTik VSA: the point of the
// assertion is which rate string was sent, and matching the bytes inside the
// attribute proves that without this test having to re-implement — and
// therefore be able to agree with — the very encoder it is checking.
func containsRateLimit(pkt *radius.Packet, want string) bool {
	for _, attr := range pkt.Attributes {
		if attr.Type == 26 && bytes.Contains(attr.Attribute, []byte(want)) {
			return true
		}
	}
	return false
}

// TestFR_HSP_003_OverQuotaHotspotSessionIsThrottledByCoA is the end-to-end
// assertion described in this file's header.
func TestFR_HSP_003_OverQuotaHotspotSessionIsThrottledByCoA(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	// A 1 GB hotspot plan that drops to 512k on breach. The throttle string is
	// what the CoA must carry; the full-rate string is what it must NOT, since
	// sending the unthrottled rate would be a CoA that reports success and
	// enforces nothing.
	const (
		fullRate  = "20M/20M"
		throttled = "512k/512k"
		quota     = int64(1_073_741_824)
	)
	seedPlan(ctx, t, pool, 1, "Hotspot_1GB", fullRate, quota, throttled, "49.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "walkup@isp", PlanID: 1})
	seedNAS(ctx, t, pool, 1, "127.0.0.1", true)

	// The device gets onto the network the FR-HSP-002 way: a registered MAC,
	// authorised by MAB with no password.
	hotspotStore := database.Hotspot()
	if _, err := hotspotStore.RegisterDevice(ctx, "AA:BB:CC:00:11:22", 1, "cafe laptop", nil); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	authorised, err := hotspotStore.AuthorizeMAC(ctx, "AA:BB:CC:00:11:22", 1)
	if err != nil || authorised == nil {
		t.Fatalf("the hotspot device must authenticate before it can be throttled: %v", err)
	}
	if authorised.RateLimitStr != fullRate {
		t.Fatalf("hotspot session must start at the plan rate: want %q, got %q", fullRate, authorised.RateLimitStr)
	}

	// It then uses 1.5 GB — comfortably past the plan's threshold. The NAS
	// address is loopback so the CoA the scanner triggers lands on the stub.
	seedSession(ctx, t, pool, 1, "hotspot-sess-001", "127.0.0.1", 1_100_000_000, 500_000_000)

	// ── The scanner notices ─────────────────────────────────────────────────
	if _, err := pool.Exec(ctx, `TRUNCATE TABLE jobqueue_tasks RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate jobqueue_tasks: %v", err)
	}
	taskClient := jobqueue.NewClient(pool)
	defer taskClient.Close() //nolint:errcheck
	inspector := jobqueue.NewInspector(pool)

	fupStore := database.FUP()
	if err := fup.NewScanner(fupStore, taskClient).ScanOnce(ctx); err != nil {
		t.Fatalf("FUP scan: %v", err)
	}

	pending := pendingCoATasks(t, inspector)
	if len(pending) != 1 {
		t.Fatalf("an over-quota hotspot session must enqueue exactly one CoA task, got %d — "+
			"a hotspot session that the FUP scanner skips is an unmetered one", len(pending))
	}

	var payload fup.CoAPayload
	if err := json.Unmarshal(pending[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal CoA payload: %v", err)
	}
	if payload.SubscriberID != 1 {
		t.Errorf("CoA payload subscriber_id: want 1, got %d", payload.SubscriberID)
	}

	// Breaching must have flipped fup_active, which is both what stops the next
	// tick re-sending the same CoA and what makes the throttled rate the one
	// GetSubscriberNASSession returns below.
	assertFUPActive(ctx, t, pool, 1, true)

	// ── The CoA actually goes out ───────────────────────────────────────────
	stub := startCoAStubNAS(t, hotspotCoASecret, radius.CodeCoAACK)

	coaHandler := fup.NewCoAHandler(fupStore, hotspotCoASecret)
	coaHandler.SetPort(stub.port)
	if err := coaHandler.ProcessTask(ctx, jobqueue.NewTask(fup.TaskTypeCoA, pending[0].Payload)); err != nil {
		t.Fatalf("CoA dispatch for an over-quota hotspot session failed: %v", err)
	}

	pkt := stub.awaitCoA(t)
	if pkt.Code != radius.CodeCoARequest {
		t.Errorf("packet code: want CoA-Request, got %v", pkt.Code)
	}
	// Acct-Session-Id (44) identifies which session to re-shape. Without it the
	// NAS has a bandwidth instruction and nobody to apply it to.
	if got := string(pkt.Get(radius.Type(44))); got != "hotspot-sess-001" {
		t.Errorf("CoA Acct-Session-Id: want %q, got %q", "hotspot-sess-001", got)
	}
	if !containsRateLimit(pkt, throttled) {
		t.Errorf("the CoA must carry the plan's FUP throttle rate %q — a CoA without it is an "+
			"enforcement action that enforces nothing", throttled)
	}
	if containsRateLimit(pkt, fullRate) {
		t.Errorf("the CoA must not carry the full rate %q: the subscriber is over quota", fullRate)
	}
}

// TestFR_HSP_003_HotspotSessionUnderQuotaIsLeftAlone is the other half of the
// guarantee. Throttling that fires early is worse than not firing: a café
// hotspot that drops every guest to 512k on connection looks broken, and the
// operator has no way to tell that from a network fault.
func TestFR_HSP_003_HotspotSessionUnderQuotaIsLeftAlone(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	const quota = int64(1_073_741_824)
	seedPlan(ctx, t, pool, 1, "Hotspot_1GB", "20M/20M", quota, "512k/512k", "49.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "light@isp", PlanID: 1})
	seedNAS(ctx, t, pool, 1, "127.0.0.1", true)

	if _, err := database.Hotspot().RegisterDevice(ctx, "AA:BB:CC:00:11:33", 1, "phone", nil); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	// Half the quota: past nothing.
	seedSession(ctx, t, pool, 1, "hotspot-sess-002", "127.0.0.1", 300_000_000, 200_000_000)

	if _, err := pool.Exec(ctx, `TRUNCATE TABLE jobqueue_tasks RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate jobqueue_tasks: %v", err)
	}
	taskClient := jobqueue.NewClient(pool)
	defer taskClient.Close() //nolint:errcheck
	inspector := jobqueue.NewInspector(pool)

	if err := fup.NewScanner(database.FUP(), taskClient).ScanOnce(ctx); err != nil {
		t.Fatalf("FUP scan: %v", err)
	}

	if pending := pendingCoATasks(t, inspector); len(pending) != 0 {
		t.Errorf("a hotspot session under quota must not be throttled, got %d CoA task(s)", len(pending))
	}
	assertFUPActive(ctx, t, pool, 1, false)
}

// TestFR_HSP_003_ThrottledHotspotSessionIsNotReThrottled covers the scanner's
// 10-second tick. Without the fup_active gate every pass would re-send a CoA
// for the same session, and a busy café would generate a CoA storm against its
// own NAS for as long as anyone stayed over quota.
func TestFR_HSP_003_ThrottledHotspotSessionIsNotReThrottled(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	const quota = int64(1_073_741_824)
	seedPlan(ctx, t, pool, 1, "Hotspot_1GB", "20M/20M", quota, "512k/512k", "49.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "heavy@isp", PlanID: 1})
	seedNAS(ctx, t, pool, 1, "127.0.0.1", true)

	if _, err := database.Hotspot().RegisterDevice(ctx, "AA:BB:CC:00:11:44", 1, "tablet", nil); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	seedSession(ctx, t, pool, 1, "hotspot-sess-003", "127.0.0.1", 1_100_000_000, 500_000_000)

	if _, err := pool.Exec(ctx, `TRUNCATE TABLE jobqueue_tasks RESTART IDENTITY`); err != nil {
		t.Fatalf("truncate jobqueue_tasks: %v", err)
	}
	taskClient := jobqueue.NewClient(pool)
	defer taskClient.Close() //nolint:errcheck
	inspector := jobqueue.NewInspector(pool)

	scanner := fup.NewScanner(database.FUP(), taskClient)
	for pass := 1; pass <= 3; pass++ {
		if err := scanner.ScanOnce(ctx); err != nil {
			t.Fatalf("FUP scan pass %d: %v", pass, err)
		}
	}

	if pending := pendingCoATasks(t, inspector); len(pending) != 1 {
		t.Errorf("three scans of one over-quota hotspot session must enqueue one CoA, got %d", len(pending))
	}
}

func assertFUPActive(ctx context.Context, t *testing.T, pool *pgxpool.Pool, subscriberID int, want bool) {
	t.Helper()
	var got bool
	if err := pool.QueryRow(ctx, `SELECT fup_active FROM subscribers WHERE id = $1`, subscriberID).Scan(&got); err != nil {
		t.Fatalf("read fup_active for subscriber %d: %v", subscriberID, err)
	}
	if got != want {
		t.Errorf("subscriber %d fup_active: want %v, got %v", subscriberID, want, got)
	}
}

// Compile-time proof that the store the scanner and CoA sender were handed here
// is the same one cmd/api and cmd/radiusd wire in production, rather than a
// test-only shape that happens to satisfy the interfaces.
var (
	_ fup.FUPQuerier = (*db.FUPStore)(nil)
	_ fup.CoAQuerier = (*db.FUPStore)(nil)
)
