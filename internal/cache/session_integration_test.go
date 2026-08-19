//go:build integration

// Integration tests for the live-session store.
//
// PostgreSQL is real, with the real migrations applied — the whole point of
// this layer is now the SQL (migration 036), so a fake would test nothing.
// Bring the database up with ./scripts/run_db_tests.sh, which sets
// TEST_DB_DSN.
//
// This used to run against a real in-process Redis (miniredis) instead, when
// live session state was a Redis cache. The assertions that were about
// Redis mechanics specifically — key format, key existence, SETNX — are
// gone; what they were really protecting (a session reads back intact, ages
// out when its Accounting-Stop is lost, and never reports a stale
// subscriber as online) is asserted here through the store's own API.
package cache_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/cache"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

const quota3TB = int64(3_543_348_019_200)

const testSubscriberID = 42

func newStore(t *testing.T) (*cache.SessionStore, *pgxpool.Pool) {
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

	// live_sessions.subscriber_id carries an FK to subscribers, so the row
	// has to exist before a session can reference it.
	if _, err := pool.Exec(ctx,
		`TRUNCATE TABLE live_sessions, subscribers RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	seedSubscriber(ctx, t, pool, testSubscriberID)

	return cache.NewSessionStore(pool), pool
}

func seedSubscriber(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO plans (id, name, rate_limit_string, volume_gb, fup_threshold_bytes, price, validity_days)
		VALUES (1, 'Test 100M', '100M/100M', 3300, $1, 999, 30)
		ON CONFLICT (id) DO NOTHING`, quota3TB)
	if err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	_, err = pool.Exec(ctx, `
		INSERT INTO subscribers
			(id, caf_number, username, password_hash, mobile_number, plan_id, status, registered_state)
		VALUES ($1, 'CAF-LIVE-001', 'live@isp', '$2a$12$seedhash', '+919000000001', 1, 'active', 'TN')
		ON CONFLICT (id) DO NOTHING`, id)
	if err != nil {
		t.Fatalf("seed subscriber: %v", err)
	}
}

// putSession writes a session the way radiusd does at Accounting-Start, then
// applies the byte counters an Interim-Update would carry. Two steps because
// that is the real shape of the write path: Put has no octets to record yet.
func putSession(ctx context.Context, t *testing.T, store *cache.SessionStore, in, out int64) {
	t.Helper()
	err := store.Put(ctx, radius.LiveSession{
		SessionID:    "sess-live-001",
		SubscriberID: testSubscriberID,
		NasIP:        "10.10.0.1",
		AssignedIP:   "100.64.12.7",
		SpeedProfile: "100M/100M",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if in != 0 || out != 0 {
		if err := store.UpdateOctets(ctx, "sess-live-001", in, out); err != nil {
			t.Fatalf("UpdateOctets: %v", err)
		}
	}
}

// setQuotaAndAge sets the plan quota and back-dates the row, which no
// production path does — bytes_total is a plan attribute the daemon does not
// carry, and started_at/updated_at are always now() on the write path.
func setQuotaAndAge(ctx context.Context, t *testing.T, pool *pgxpool.Pool, quota int64, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		UPDATE live_sessions
		SET bytes_total = $2, started_at = now() - ($3 * interval '1 second')
		WHERE subscriber_id = $1`, testSubscriberID, quota, age.Seconds())
	if err != nil {
		t.Fatalf("set quota/age: %v", err)
	}
}

// TestSessionStore_RoundTrip verifies a session survives storage and reads back
// with its derived usage figures intact.
func TestSessionStore_RoundTrip(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 2_000_000_000_000, 834_678_415_360)
	setQuotaAndAge(ctx, t, pool, quota3TB, 3*time.Hour+12*time.Minute)

	got, err := store.Get(ctx, testSubscriberID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("want a session, got nil")
	}
	if got.SessionID != "sess-live-001" || got.NasIP != "10.10.0.1" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if want := int64(2_000_000_000_000 + 834_678_415_360); got.BytesUsed() != want {
		t.Errorf("bytes_used: want %d, got %d", want, got.BytesUsed())
	}
	if pct := got.PctUsed(); pct != 80 {
		t.Errorf("pct_used: want 80, got %d", pct)
	}
}

// TestSessionStore_OfflineIsNotAnError verifies a missing row reads as offline
// rather than failing the caller.
func TestSessionStore_OfflineIsNotAnError(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	got, err := store.Get(ctx, 999)
	if err != nil {
		t.Fatalf("a missing session must not error, got: %v", err)
	}
	if got != nil {
		t.Errorf("want nil for an offline subscriber, got %+v", got)
	}

	summary, err := store.GetActiveSession(ctx, 999)
	if err != nil || summary != nil {
		t.Errorf("health view: want (nil, nil), got (%+v, %v)", summary, err)
	}
	portalSession, err := store.Portal().GetActiveSession(ctx, 999)
	if err != nil || portalSession != nil {
		t.Errorf("portal view: want (nil, nil), got (%+v, %v)", portalSession, err)
	}
}

// TestSessionStore_HealthView verifies the health projection.
func TestSessionStore_HealthView(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 2_000_000_000_000, 834_678_415_360)
	// Half a minute past 3h12m, not exactly on the boundary: SessionAge
	// truncates to whole minutes, so an exact 3h12m back-date flips between
	// "3h11m" and "3h12m" on sub-second jitter across the DB round trip.
	setQuotaAndAge(ctx, t, pool, quota3TB, 3*time.Hour+12*time.Minute+30*time.Second)

	summary, err := store.GetActiveSession(ctx, testSubscriberID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}
	if summary == nil {
		t.Fatal("want a summary, got nil")
	}
	if summary.SessionID != "sess-live-001" {
		t.Errorf("session_id: got %q", summary.SessionID)
	}
	if summary.AssignedIP != "100.64.12.7" {
		t.Errorf("assigned_ip: got %q", summary.AssignedIP)
	}
	if summary.BytesTotal != quota3TB {
		t.Errorf("bytes_total: want %d, got %d", quota3TB, summary.BytesTotal)
	}
	if summary.PctUsed != 80 {
		t.Errorf("pct_used: want 80, got %d", summary.PctUsed)
	}
	if summary.SessionAge != "3h12m" {
		t.Errorf("session_age: want 3h12m, got %q", summary.SessionAge)
	}
}

// TestSessionStore_PortalView verifies the portal projection reports usage in GB.
func TestSessionStore_PortalView(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 2_000_000_000_000, 834_678_415_360)
	setQuotaAndAge(ctx, t, pool, quota3TB, time.Hour)
	if _, err := pool.Exec(ctx,
		`UPDATE live_sessions SET fup_throttled = TRUE WHERE subscriber_id = $1`,
		testSubscriberID); err != nil {
		t.Fatalf("set fup_throttled: %v", err)
	}

	got, err := store.Portal().GetActiveSession(ctx, testSubscriberID)
	if err != nil {
		t.Fatalf("portal GetActiveSession: %v", err)
	}
	if got == nil {
		t.Fatal("want a session, got nil")
	}
	if !got.FUPThrottled {
		t.Error("fup_throttled must carry through to the portal view")
	}
	if got.BytesIn != 2_000_000_000_000 || got.BytesOut != 834_678_415_360 {
		t.Errorf("octets: got in=%d out=%d", got.BytesIn, got.BytesOut)
	}
	// 2_834_678_415_360 bytes is ~2640.0 GiB of a 3300 GiB quota.
	if got.GBIncluded.IntPart() != 3300 {
		t.Errorf("gb_included: want 3300, got %s", got.GBIncluded)
	}
	if got.GBUsed.IntPart() != 2640 {
		t.Errorf("gb_used: want 2640, got %s", got.GBUsed)
	}
	if got.PctUsed < 79.9 || got.PctUsed > 80.1 {
		t.Errorf("pct_used: want ~80, got %v", got.PctUsed)
	}
}

// TestSessionStore_UnlimitedPlan verifies an unlimited plan never reports a
// percentage, which would otherwise divide by zero.
func TestSessionStore_UnlimitedPlan(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 2_000_000_000_000, 834_678_415_360)
	setQuotaAndAge(ctx, t, pool, 0, time.Hour) // 0 = unlimited

	summary, err := store.GetActiveSession(ctx, testSubscriberID)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}
	if summary.PctUsed != 0 {
		t.Errorf("an unlimited plan must report 0%%, got %d", summary.PctUsed)
	}

	portalSession, err := store.Portal().GetActiveSession(ctx, testSubscriberID)
	if err != nil {
		t.Fatalf("portal GetActiveSession: %v", err)
	}
	if portalSession.PctUsed != 0 {
		t.Errorf("an unlimited plan must report 0%%, got %v", portalSession.PctUsed)
	}
}

// TestSessionStore_Delete verifies a manual disconnect clears the session.
func TestSessionStore_Delete(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 0, 0)
	if err := store.Delete(ctx, testSubscriberID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := store.Get(ctx, testSubscriberID)
	if err != nil || got != nil {
		t.Errorf("want (nil, nil) after delete, got (%+v, %v)", got, err)
	}
}

// TestSessionStore_DeleteBySessionID verifies the Accounting-Stop path, which
// keys on Acct-Session-Id rather than subscriber id — the daemon does not
// re-resolve which subscriber a stop record belongs to.
func TestSessionStore_DeleteBySessionID(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 0, 0)
	if err := store.DeleteBySessionID(ctx, "sess-live-001"); err != nil {
		t.Fatalf("DeleteBySessionID: %v", err)
	}
	got, err := store.Get(ctx, testSubscriberID)
	if err != nil || got != nil {
		t.Errorf("want (nil, nil) after stop, got (%+v, %v)", got, err)
	}
}

// TestSessionStore_StaleSessionReadsAsOffline verifies a session whose
// Accounting-Stop was lost ages out rather than showing a subscriber online
// forever. This is what the Redis TTL used to do for free; on Postgres it is
// an explicit updated_at predicate in the read query, so it needs a test of
// its own rather than being a property of the storage engine.
func TestSessionStore_StaleSessionReadsAsOffline(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 0, 0)

	// Back-date updated_at past SessionTTL, standing in for a session that
	// stopped receiving Interim-Updates.
	_, err := pool.Exec(ctx, `
		UPDATE live_sessions
		SET updated_at = now() - ($2 * interval '1 second')
		WHERE subscriber_id = $1`,
		testSubscriberID, (cache.SessionTTL + time.Minute).Seconds())
	if err != nil {
		t.Fatalf("age the row: %v", err)
	}

	got, err := store.Get(ctx, testSubscriberID)
	if err != nil {
		t.Fatalf("Get on a stale session: %v", err)
	}
	if got != nil {
		t.Error("a session past its TTL must read as offline")
	}
}

// TestSessionStore_InterimUpdateRefreshesStaleness verifies an Interim-Update
// brings a nearly-stale session back into the live window — the counterpart
// to the test above, and the reason updated_at is written on every update
// rather than only at start.
func TestSessionStore_InterimUpdateRefreshesStaleness(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 0, 0)
	_, err := pool.Exec(ctx, `
		UPDATE live_sessions
		SET updated_at = now() - ($2 * interval '1 second')
		WHERE subscriber_id = $1`,
		testSubscriberID, (cache.SessionTTL + time.Minute).Seconds())
	if err != nil {
		t.Fatalf("age the row: %v", err)
	}

	if err := store.UpdateOctets(ctx, "sess-live-001", 500, 600); err != nil {
		t.Fatalf("UpdateOctets: %v", err)
	}

	got, err := store.Get(ctx, testSubscriberID)
	if err != nil {
		t.Fatalf("Get after refresh: %v", err)
	}
	if got == nil {
		t.Fatal("an Interim-Update must bring a stale session back into the live window")
	}
	if got.BytesIn != 500 || got.BytesOut != 600 {
		t.Errorf("octets: want in=500 out=600, got in=%d out=%d", got.BytesIn, got.BytesOut)
	}
}

// TestSessionStore_ReconnectReplacesPriorSession verifies a subscriber
// reconnecting (new Acct-Session-Id, no Stop for the old one) replaces the
// previous row rather than colliding on the subscriber_id primary key, and
// that the stale byte counters do not carry over into the new session.
func TestSessionStore_ReconnectReplacesPriorSession(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 111, 222)

	err := store.Put(ctx, radius.LiveSession{
		SessionID:    "sess-live-002",
		SubscriberID: testSubscriberID,
		NasIP:        "10.10.0.2",
		AssignedIP:   "100.64.12.8",
		SpeedProfile: "50M/50M",
	})
	if err != nil {
		t.Fatalf("Put on reconnect: %v", err)
	}

	got, err := store.Get(ctx, testSubscriberID)
	if err != nil || got == nil {
		t.Fatalf("Get after reconnect: (%+v, %v)", got, err)
	}
	if got.SessionID != "sess-live-002" {
		t.Errorf("session_id: want the new session, got %q", got.SessionID)
	}
	if got.BytesIn != 0 || got.BytesOut != 0 {
		t.Errorf("a new session must start from zero, got in=%d out=%d", got.BytesIn, got.BytesOut)
	}
}

// TestStalenessSweeper_DeletesAgedRows verifies the sweep actually removes
// what the read predicate already hides — without it the table accumulates
// one permanent row per subscriber that ever connected.
func TestStalenessSweeper_DeletesAgedRows(t *testing.T) {
	store, pool := newStore(t)
	ctx := context.Background()

	putSession(ctx, t, store, 0, 0)
	_, err := pool.Exec(ctx, `
		UPDATE live_sessions
		SET updated_at = now() - ($2 * interval '1 second')
		WHERE subscriber_id = $1`,
		testSubscriberID, (cache.SessionTTL + time.Minute).Seconds())
	if err != nil {
		t.Fatalf("age the row: %v", err)
	}

	sweepCtx, cancel := context.WithCancel(ctx)
	go cache.NewStalenessSweeper(pool, 20*time.Millisecond).Run(sweepCtx)
	defer cancel()

	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM live_sessions WHERE subscriber_id = $1`,
			testSubscriberID).Scan(&n); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweeper did not delete the aged row")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
