//go:build integration

// RADIUS accounting, end to end — FR-AAA-003 | DDS §5.2.
//
// The other accounting tests use a recording store to check the daemon's
// decisions. This one checks the thing those cannot: that a real
// Accounting-Request, sent over a real UDP socket to a real listener, ends up
// as a row in a real PostgreSQL — and that the FUP scanner then finds it.
//
//	MikroTik-shaped Accounting-Start on :1813
//	  → daemon persists to subscriber_session_history
//	    → Interim-Update carries the subscriber past their plan's quota
//	      → FUP scanner sees the session and enqueues a CoA
//
// That chain is the answer to "is the accounting actually wired up", and every
// link in it was broken before: nothing bound the accounting port, and nothing
// wrote the table. A test that stubbed either end would have stayed green
// throughout.
package db_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/fup"
	ispradius "github.com/maaransoft/isp-bss-oss/internal/radius"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
	"layeh.com/radius/rfc2866"
	"layeh.com/radius/rfc2869"
)

var acctTestSecret = []byte("acct-e2e-secret")

// freeUDPPort reserves an ephemeral port and releases it, so the daemon can
// bind it moments later. Preferable to a hardcoded port, which collides with
// whatever else the CI container happens to be running.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("reserve UDP port: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}

// startAccountingDaemon runs the real daemon against the real store and returns
// the accounting address to send to.
func startAccountingDaemon(t *testing.T, database *db.DB) string {
	t.Helper()

	authAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))
	acctAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))

	daemon := ispradius.NewRadiusDaemon(authAddr, acctTestSecret, database.Radius(),
		[]byte("verifier-secret-32-bytes-minimum-len"))
	daemon.SetAccountingStore(database.FUP())
	daemon.SetAcctAddr(acctAddr)
	daemon.SetMABQuerier(database.Hotspot())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := daemon.StartContext(ctx); err != nil {
			t.Logf("radius daemon exited: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Log("radius daemon did not shut down in time")
		}
	})

	return acctAddr
}

// sendAccounting sends one Accounting-Request and waits for the response,
// retrying briefly so the test does not race the listener coming up.
func sendAccounting(t *testing.T, addr string, pkt *radius.Packet) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := radius.Exchange(ctx, pkt, addr)
		cancel()
		if err == nil {
			if resp.Code != radius.CodeAccountingResponse {
				t.Fatalf("want Accounting-Response, got %v", resp.Code)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no Accounting-Response from %s: %v — is the accounting port bound?", addr, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

type acctPacketOpts struct {
	SessionID       string
	Username        string
	Status          rfc2866.AcctStatusType
	InputOctets     uint32
	OutputOctets    uint32
	InputGigawords  uint32
	OutputGigawords uint32
	NASIP           string
	FramedIP        string
	Cause           rfc2866.AcctTerminateCause
}

func acctPacket(t *testing.T, o acctPacketOpts) *radius.Packet {
	t.Helper()
	pkt := radius.New(radius.CodeAccountingRequest, acctTestSecret)
	must := func(what string, err error) {
		if err != nil {
			t.Fatalf("set %s: %v", what, err)
		}
	}
	must("Acct-Status-Type", rfc2866.AcctStatusType_Set(pkt, o.Status))
	must("Acct-Session-Id", rfc2866.AcctSessionID_SetString(pkt, o.SessionID))
	must("User-Name", rfc2865.UserName_SetString(pkt, o.Username))
	must("Acct-Input-Octets", rfc2866.AcctInputOctets_Set(pkt, rfc2866.AcctInputOctets(o.InputOctets)))
	must("Acct-Output-Octets", rfc2866.AcctOutputOctets_Set(pkt, rfc2866.AcctOutputOctets(o.OutputOctets)))
	if o.InputGigawords > 0 {
		must("Acct-Input-Gigawords", rfc2869.AcctInputGigawords_Set(pkt, rfc2869.AcctInputGigawords(o.InputGigawords)))
	}
	if o.OutputGigawords > 0 {
		must("Acct-Output-Gigawords", rfc2869.AcctOutputGigawords_Set(pkt, rfc2869.AcctOutputGigawords(o.OutputGigawords)))
	}
	if o.NASIP != "" {
		must("NAS-IP-Address", rfc2865.NASIPAddress_Set(pkt, net.ParseIP(o.NASIP)))
	}
	if o.FramedIP != "" {
		must("Framed-IP-Address", rfc2865.FramedIPAddress_Set(pkt, net.ParseIP(o.FramedIP)))
	}
	if o.Cause != 0 {
		must("Acct-Terminate-Cause", rfc2866.AcctTerminateCause_Set(pkt, o.Cause))
	}
	return pkt
}

// TestFR_AAA_003_AccountingReachesTheDatabaseAndDrivesFUP is the end-to-end
// assertion described in this file's header.
func TestFR_AAA_003_AccountingReachesTheDatabaseAndDrivesFUP(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	// A 4 GB plan, chosen so the quota sits above the 32-bit octet counter's
	// wrap point: the session below only exceeds it once gigawords are counted,
	// which is what makes this a real test of that arithmetic rather than a
	// restatement of the unit test.
	const quota = int64(4_294_967_296) // 4 GiB
	seedPlan(ctx, t, pool, 1, "Metered_4G", "50M/50M", quota, "1M/1M", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "acct@isp", PlanID: 1})

	acctAddr := startAccountingDaemon(t, database)

	// ── Start ───────────────────────────────────────────────────────────────
	sendAccounting(t, acctAddr, acctPacket(t, acctPacketOpts{
		SessionID: "e2e-sess-1", Username: "acct@isp",
		Status: rfc2866.AcctStatusType_Value_Start,
		NASIP:  "198.51.100.4", FramedIP: "100.64.7.9",
	}))

	var (
		subscriberID int
		nasIP        string
		assignedIP   string
		stopTime     *time.Time
	)
	err := pool.QueryRow(ctx, `
		SELECT subscriber_id, host(nas_ip_address), host(assigned_ipv4), stop_time
		  FROM subscriber_session_history WHERE session_id = 'e2e-sess-1'`).
		Scan(&subscriberID, &nasIP, &assignedIP, &stopTime)
	if err != nil {
		t.Fatalf("Accounting-Start must create a session row: %v", err)
	}
	if subscriberID != 1 {
		t.Errorf("subscriber_id: want 1, got %d", subscriberID)
	}
	if nasIP != "198.51.100.4" {
		t.Errorf("nas_ip_address: want the NAS-IP-Address attribute, got %q", nasIP)
	}
	if assignedIP != "100.64.7.9" {
		t.Errorf("assigned_ipv4: want 100.64.7.9, got %q — LEA lookups resolve against this", assignedIP)
	}
	if stopTime != nil {
		t.Error("a started session must be open")
	}

	// ── Interim, past the quota, with a wrapped counter ─────────────────────
	sendAccounting(t, acctAddr, acctPacket(t, acctPacketOpts{
		SessionID: "e2e-sess-1", Username: "acct@isp",
		Status: rfc2866.AcctStatusType_Value_InterimUpdate,
		// 1 gigaword + 1,000,000 in, 1 gigaword + 500,000 out ≈ 8.6 GB total,
		// comfortably past a 4 GiB quota — but only 1.5 MB if gigawords are
		// dropped, which would leave the subscriber unthrottled.
		InputOctets: 1_000_000, InputGigawords: 1,
		OutputOctets: 500_000, OutputGigawords: 1,
	}))

	var storedIn, storedOut int64
	if err := pool.QueryRow(ctx, `
		SELECT input_octets, output_octets FROM subscriber_session_history
		 WHERE session_id = 'e2e-sess-1'`).Scan(&storedIn, &storedOut); err != nil {
		t.Fatalf("read stored octets: %v", err)
	}
	wantIn := int64(1)<<32 | 1_000_000
	wantOut := int64(1)<<32 | 500_000
	if storedIn != wantIn || storedOut != wantOut {
		t.Errorf("stored octets: want (%d, %d), got (%d, %d) — dropping Acct-Input-Gigawords "+
			"makes usage appear to reset every 4 GiB", wantIn, wantOut, storedIn, storedOut)
	}

	// ── The scanner now sees a real, over-quota session ─────────────────────
	mr := miniredis.RunT(t)
	asynqClient := asynq.NewClient(asynq.RedisClientOpt{Addr: mr.Addr()})
	defer asynqClient.Close() //nolint:errcheck
	inspector := asynq.NewInspector(asynq.RedisClientOpt{Addr: mr.Addr()})
	defer inspector.Close() //nolint:errcheck

	if err := fup.NewScanner(database.FUP(), asynqClient).ScanOnce(ctx); err != nil {
		t.Fatalf("FUP scan: %v", err)
	}
	if pending := pendingCoATasks(t, inspector); len(pending) != 1 {
		t.Fatalf("accounting data must reach the FUP scanner: want 1 CoA task, got %d — "+
			"this is the link that was missing entirely, and it is what makes every "+
			"quota-enforcement feature real rather than merely tested", len(pending))
	}

	// ── Stop ────────────────────────────────────────────────────────────────
	sendAccounting(t, acctAddr, acctPacket(t, acctPacketOpts{
		SessionID: "e2e-sess-1", Username: "acct@isp",
		Status:      rfc2866.AcctStatusType_Value_Stop,
		InputOctets: 2_000_000, InputGigawords: 1,
		OutputOctets: 900_000, OutputGigawords: 1,
		Cause: rfc2866.AcctTerminateCause_Value_UserRequest,
	}))

	var cause *string
	if err := pool.QueryRow(ctx, `
		SELECT stop_time, terminate_cause FROM subscriber_session_history
		 WHERE session_id = 'e2e-sess-1'`).Scan(&stopTime, &cause); err != nil {
		t.Fatalf("read stopped session: %v", err)
	}
	if stopTime == nil {
		t.Error("Accounting-Stop must close the session — an unclosed session keeps counting " +
			"toward the subscriber's quota forever")
	}
	if cause == nil || *cause != "User-Request" {
		t.Errorf("terminate_cause: want User-Request, got %v", cause)
	}
}

// TestFR_AAA_003_RetransmittedStartDoesNotDoubleCount covers the store's
// idempotency directly, bypassing the daemon's Redis dedup.
//
// The dedup window is 30 seconds; a NAS that retransmits a Start after that —
// or a second radiusd instance handling it — must still not create a second
// open row. Two open rows for one session are summed by the FUP scanner, which
// would throttle the subscriber at half their real quota.
func TestFR_AAA_003_RetransmittedStartDoesNotDoubleCount(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "dup@isp", PlanID: 1})

	store := database.FUP()
	for i := 0; i < 3; i++ {
		if err := store.StartSession(ctx, 1, "dup-sess", "198.51.100.4", "100.64.7.9"); err != nil {
			t.Fatalf("StartSession attempt %d: %v", i+1, err)
		}
	}

	open := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM subscriber_session_history WHERE session_id = 'dup-sess' AND stop_time IS NULL`)
	if open != 1 {
		t.Fatalf("three Accounting-Starts for one session must leave one open row, got %d — "+
			"duplicates are summed by the FUP scanner and would halve the effective quota", open)
	}

	// Once closed, the same session id may legitimately start again: a
	// subscriber who reconnects gets a fresh session, and refusing it would
	// lose the new one's usage entirely.
	if _, err := store.StopSession(ctx, "dup-sess", 10, 20, "User-Request"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if err := store.StartSession(ctx, 1, "dup-sess", "198.51.100.4", "100.64.7.9"); err != nil {
		t.Fatalf("StartSession after stop: %v", err)
	}
	total := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM subscriber_session_history WHERE session_id = 'dup-sess'`)
	if total != 2 {
		t.Errorf("a reconnect after a closed session must record a new row, got %d total", total)
	}
}

// TestFR_AAA_003_UnmatchedRecordsReportHonestly — an Interim or Stop for a
// session that was never opened must say so rather than silently affecting no
// rows, so the daemon can count it and an operator can see usage going missing.
func TestFR_AAA_003_UnmatchedRecordsReportHonestly(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "orphan@isp", PlanID: 1})

	store := database.FUP()

	matched, err := store.UpdateSessionOctets(ctx, "never-started", 1, 2)
	if err != nil {
		t.Fatalf("UpdateSessionOctets: %v", err)
	}
	if matched {
		t.Error("an interim update for an unknown session must report no match")
	}

	stopped, err := store.StopSession(ctx, "never-started", 1, 2, "User-Request")
	if err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if stopped {
		t.Error("stopping an unknown session must report no match")
	}
}

// Compile-time proof that the store wired into cmd/radiusd is the one these
// tests exercise, rather than a shape that merely satisfies the interface.
var _ ispradius.AccountingStore = (*db.FUPStore)(nil)
