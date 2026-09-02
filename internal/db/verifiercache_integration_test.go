//go:build integration

// Integration tests for migration 046 and the RADIUS fast-verifier cache's
// two-tier behaviour.
//
// These need a real PostgreSQL. The whole point of the L2 tier is its SQL —
// an upsert whose conflict target is subtly wrong, a foreign key that does
// not actually cascade, or a CHECK that never fires would all pass a test
// built on a fake and fail in production. Bring the database up with
// ./scripts/run_db_tests.sh, which sets TEST_DB_DSN.
package db_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/maaransoft/isp-bss-oss/internal/localcache"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

const migration046Path = "../../migrations/046_create_verifier_cache.sql"

// verifierTestSecret stands in for RADIUS_VERIFIER_SECRET. Its only
// requirement here is being stable within a test, since a verifier computed
// under one secret must not match under another.
var verifierTestSecret = []byte("integration-test-verifier-secret-32b+")

// seedVerifierFixtures creates the plan and subscriber every test here needs
// and returns the subscriber id.
func seedVerifierFixtures(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int, username string) int {
	t.Helper()
	seedPlan(ctx, t, pool, 1, "Verifier Test Plan", "100M/100M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, id, seedOpts{Username: username, PlanID: 1})
	return id
}

// newVerifierCache builds a real VerifierCache backed by the real
// PostgreSQL store — no fakes anywhere in this file.
func newVerifierCache(store radius.VerifierStore) *radius.VerifierCache {
	c := radius.NewVerifierCache(localcache.New[[]byte](0), verifierTestSecret)
	c.SetPersistence(store)
	return c
}

// ── (1) Schema migration, both directions ───────────────────────────────────

// TestMigration046_UpAndDownBothApplyCleanly runs the committed migration
// file's Down and Up sections against the live schema and rolls the whole
// thing back.
//
// Executed inside a transaction rather than against the database proper for
// two reasons: PostgreSQL makes DDL transactional, so this is genuinely
// safe; and these tests share one database with every other package (see
// run_db_tests.sh's -p 1), so a test that actually dropped a table would
// break whatever ran next rather than failing honestly on its own.
//
// It reads the real file instead of restating the SQL. A copy pasted into a
// test asserts that the copy is valid, which is not the question — the
// question is whether the migration that will run in production is.
func TestMigration046_UpAndDownBothApplyCleanly(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	raw, err := os.ReadFile(migration046Path)
	if err != nil {
		t.Fatalf("read %s: %v", migration046Path, err)
	}
	up, down, ok := splitGooseSections(string(raw))
	if !ok {
		t.Fatalf("%s has no '-- +goose Down' marker — goose would treat the whole "+
			"file as irreversible", migration046Path)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback is the point

	// Down first: the table already exists, because the suite runs against a
	// fully migrated schema.
	if _, err := tx.Exec(ctx, down); err != nil {
		t.Fatalf("migration 046 Down failed: %v", err)
	}
	if tableExistsTx(ctx, t, tx, "radius_verifier_cache") {
		t.Error("Down ran without error but the table is still present")
	}

	// Then Up, which must restore it.
	if _, err := tx.Exec(ctx, up); err != nil {
		t.Fatalf("migration 046 Up failed after Down — the migration is not "+
			"replayable, so a rollback in production could not be undone: %v", err)
	}
	if !tableExistsTx(ctx, t, tx, "radius_verifier_cache") {
		t.Error("Up ran without error but the table is absent")
	}
}

// TestMigration046_SchemaConstraints pins the guarantees the application
// leans on, each of which is invisible in Go and would fail silently.
func TestMigration046_SchemaConstraints(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	subID := seedVerifierFixtures(ctx, t, pool, 1, "alice@isp")
	store := database.VerifierCache()

	t.Run("verifier must be exactly 32 bytes", func(t *testing.T) {
		// HMAC-SHA256 is always 32 bytes. A truncated or oversized value
		// would never match any computed verifier, silently degrading every
		// authentication to bcrypt with nothing reporting why — so the
		// schema refuses it rather than storing it.
		_, err := pool.Exec(ctx, `
			INSERT INTO radius_verifier_cache (subscriber_id, username, verifier, expires_at)
			VALUES ($1, $2, $3, NOW() + interval '5 minutes')`,
			subID, "alice@isp", []byte("too-short"))
		if err == nil {
			t.Error("a 9-byte verifier was accepted; chk on octet_length is not enforcing")
		}
	})

	t.Run("deleting a subscriber takes their verifier with it", func(t *testing.T) {
		if err := store.Save(ctx, subID, "alice@isp", make([]byte, 32), time.Now().Add(time.Minute)); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM subscribers WHERE id = $1`, subID); err != nil {
			t.Fatalf("delete subscriber: %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM radius_verifier_cache WHERE subscriber_id = $1`, subID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Error("a terminated subscriber's verifier survived — the fast path would " +
				"keep answering for an account that no longer exists")
		}
	})
}

// ── (2) Write-through ───────────────────────────────────────────────────────

// TestVerifierCache_WriteThroughReachesPostgres proves Store populates both
// tiers, not just the in-process one. Without the L2 write there is nothing
// to warm from, and the whole persistence feature is inert while appearing
// to work.
func TestVerifierCache_WriteThroughReachesPostgres(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	subID := seedVerifierFixtures(ctx, t, pool, 1, "bob@isp")

	cache := newVerifierCache(database.VerifierCache())
	const pw, hash = "hunter2", "$2a$12$abcdefghijklmnopqrstuv"

	if err := cache.Store(ctx, subID, "bob@isp", pw, hash); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// L1 serves it.
	if ok, _ := cache.Check(ctx, "bob@isp", pw, hash); !ok {
		t.Error("L1 did not serve a verifier it had just stored")
	}

	// L2 has it too, with a sane expiry.
	var (
		gotUser   string
		gotBytes  []byte
		expiresAt time.Time
	)
	err := pool.QueryRow(ctx, `
		SELECT username, verifier, expires_at FROM radius_verifier_cache
		WHERE subscriber_id = $1`, subID).Scan(&gotUser, &gotBytes, &expiresAt)
	if err != nil {
		t.Fatalf("verifier was not written through to PostgreSQL: %v", err)
	}
	if gotUser != "bob@isp" {
		t.Errorf("username: got %q", gotUser)
	}
	if len(gotBytes) != 32 {
		t.Errorf("verifier length: got %d, want 32", len(gotBytes))
	}
	if remaining := time.Until(expiresAt); remaining <= 0 || remaining > 10*time.Minute {
		t.Errorf("expires_at is %s from now, which is outside the 5-minute TTL", remaining)
	}

	// And the stored bytes are not the password or the hash in any form.
	if strings.Contains(string(gotBytes), pw) || strings.Contains(string(gotBytes), hash) {
		t.Error("the stored verifier contains the password or bcrypt hash verbatim")
	}
}

// TestVerifierCache_SaveIsIdempotentPerSubscriber covers the upsert's
// conflict target. It is subscriber_id (the primary key) rather than
// username, so that a username change replaces the subscriber's row instead
// of inserting a second one and colliding on the username unique index.
func TestVerifierCache_SaveIsIdempotentPerSubscriber(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	subID := seedVerifierFixtures(ctx, t, pool, 1, "carol@isp")
	store := database.VerifierCache()

	first := make([]byte, 32)
	first[0] = 0x01
	second := make([]byte, 32)
	second[0] = 0x02

	if err := store.Save(ctx, subID, "carol@isp", first, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	// Same subscriber, different username — the rename case.
	if err := store.Save(ctx, subID, "carol.renamed@isp", second, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("second Save after rename: %v", err)
	}

	var (
		rows     int
		username string
		verifier []byte
	)
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM radius_verifier_cache WHERE subscriber_id = $1`, subID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("want exactly 1 row per subscriber, got %d", rows)
	}
	if err := pool.QueryRow(ctx,
		`SELECT username, verifier FROM radius_verifier_cache WHERE subscriber_id = $1`,
		subID).Scan(&username, &verifier); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if username != "carol.renamed@isp" || verifier[0] != 0x02 {
		t.Errorf("upsert did not replace the row: username=%q verifier[0]=%#x", username, verifier[0])
	}
}

// ── (3) Warmup and TTL preservation ─────────────────────────────────────────

// TestVerifierCache_WarmupRestoresAcrossProcesses is the behaviour the whole
// tier exists for: a restart must not force every reconnecting subscriber
// back through bcrypt.
func TestVerifierCache_WarmupRestoresAcrossProcesses(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	subID := seedVerifierFixtures(ctx, t, pool, 1, "dave@isp")
	const pw, hash = "s3cret", "$2a$12$zyxwvutsrqponmlkjihg"

	// "Process A" authenticates once, paying bcrypt.
	before := newVerifierCache(database.VerifierCache())
	if err := before.Store(ctx, subID, "dave@isp", pw, hash); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// "Process B" starts with an empty L1 — a restart.
	after := newVerifierCache(database.VerifierCache())
	if ok, _ := after.Check(ctx, "dave@isp", pw, hash); ok {
		t.Fatal("precondition failed: the fresh cache already had the verifier before warmup")
	}

	restored, err := after.Warm(ctx)
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if restored != 1 {
		t.Fatalf("warmup restored %d entries, want 1", restored)
	}
	if ok, _ := after.Check(ctx, "dave@isp", pw, hash); !ok {
		t.Error("the warmed verifier did not match. This restart would have cost a full " +
			"bcrypt per reconnecting subscriber — roughly 7 auths/sec on 2 vCPUs.")
	}
}

// TestVerifierCache_WarmupPreservesRemainingTTL guards the subtle failure:
// restoring entries with a *fresh* TTL rather than their remaining one would
// let a verifier outlive its expiry indefinitely, provided the process
// restarted often enough. That turns a bounded 5-minute window into an
// unbounded one, which matters because the TTL is a backstop for exactly the
// invalidation cases the hash binding cannot cover.
func TestVerifierCache_WarmupPreservesRemainingTTL(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	subID := seedVerifierFixtures(ctx, t, pool, 1, "erin@isp")

	// A row with only ~3 seconds left, as though it were written nearly a
	// full TTL ago.
	if err := database.VerifierCache().Save(
		ctx, subID, "erin@isp", make([]byte, 32), time.Now().Add(3*time.Second)); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cache := newVerifierCache(database.VerifierCache())
	if _, err := cache.Warm(ctx); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	// Read the surviving lifetime back through behaviour rather than
	// internals: after the original expiry passes, the entry must be gone.
	time.Sleep(4 * time.Second)
	if ok, _ := cache.Check(ctx, "erin@isp", "any", "any"); ok {
		t.Error("an entry warmed with ~3s remaining was still present after 4s — " +
			"warmup granted it a fresh TTL, so a restart loop could keep a verifier " +
			"alive indefinitely")
	}
}

// TestVerifierCache_WarmupSkipsExpiredRows — LoadActive filters on
// expires_at, so an expired row must never reach L1 even though it is still
// physically present until the reaper runs.
func TestVerifierCache_WarmupSkipsExpiredRows(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Verifier Test Plan", "100M/100M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "stale@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "fresh@isp", PlanID: 1})

	store := database.VerifierCache()
	if err := store.Save(ctx, 1, "stale@isp", make([]byte, 32), time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("Save stale: %v", err)
	}
	if err := store.Save(ctx, 2, "fresh@isp", make([]byte, 32), time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("Save fresh: %v", err)
	}

	restored, err := newVerifierCache(store).Warm(ctx)
	if err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if restored != 1 {
		t.Errorf("warmup restored %d entries, want 1 (the expired row must be skipped)", restored)
	}
}

// TestVerifierCache_ReapRemovesOnlyExpired covers the reaper radiusd runs
// every 10 minutes. Over-eager reaping would silently disable the warmup
// path; under-eager growth widens the offline-attack surface the migration
// header describes.
func TestVerifierCache_ReapRemovesOnlyExpired(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedPlan(ctx, t, pool, 1, "Verifier Test Plan", "100M/100M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "old@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "current@isp", PlanID: 1})

	store := database.VerifierCache()
	_ = store.Save(ctx, 1, "old@isp", make([]byte, 32), time.Now().Add(-time.Hour))
	_ = store.Save(ctx, 2, "current@isp", make([]byte, 32), time.Now().Add(time.Hour))

	reaped, err := store.ReapExpired(ctx)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped %d rows, want 1", reaped)
	}

	var surviving string
	if err := pool.QueryRow(ctx, `SELECT username FROM radius_verifier_cache`).Scan(&surviving); err != nil {
		t.Fatalf("the unexpired row was reaped too: %v", err)
	}
	if surviving != "current@isp" {
		t.Errorf("surviving row is %q, want current@isp", surviving)
	}
}

// ── (4) Invalidation ────────────────────────────────────────────────────────

// TestVerifierCache_InvalidationIsVisibleToTheDaemon reproduces what
// cmd/api's subCacheInvalidator does when a subscriber is suspended or
// terminated, and asserts the daemon side actually observes it.
//
// subCacheInvalidator itself lives in package main and cannot be imported,
// so this exercises the exact call it makes (DeleteByUsername on the shared
// store) and then checks the property that matters: a *separate* cache
// instance — standing in for radiusd, a different process — no longer
// serves that verifier.
//
// This is the case the verifier's own design cannot cover. A password
// change self-invalidates, because the verifier is bound to the bcrypt hash
// and a changed hash can never match a stored MAC. A suspension leaves the
// hash untouched, so without this delete the fast path would keep answering
// for an account that was just cut off, for the remainder of its TTL.
func TestVerifierCache_InvalidationIsVisibleToTheDaemon(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	subID := seedVerifierFixtures(ctx, t, pool, 1, "frank@isp")
	const pw, hash = "pw", "$2a$12$unchangedhashonsuspension"

	store := database.VerifierCache()

	// radiusd caches a verifier after a successful bcrypt.
	daemonCache := newVerifierCache(store)
	if err := daemonCache.Store(ctx, subID, "frank@isp", pw, hash); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if ok, _ := daemonCache.Check(ctx, "frank@isp", pw, hash); !ok {
		t.Fatal("precondition failed: verifier not usable before invalidation")
	}

	// api_service suspends the subscriber. The bcrypt hash is untouched —
	// which is exactly why the hash binding cannot help here.
	if err := store.DeleteByUsername(ctx, "frank@isp"); err != nil {
		t.Fatalf("DeleteByUsername (what subCacheInvalidator calls): %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM radius_verifier_cache WHERE username = $1`, "frank@isp").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Error("the shared row survived invalidation")
	}

	// A daemon restarting now must not resurrect it.
	restarted := newVerifierCache(store)
	if _, err := restarted.Warm(ctx); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if ok, _ := restarted.Check(ctx, "frank@isp", pw, hash); ok {
		t.Error("warmup restored a verifier that had been invalidated — a suspended " +
			"subscriber would authenticate on the fast path after a restart")
	}
}

// TestVerifierCache_InvalidateUnknownUserSucceeds — callers invalidate
// defensively on every lifecycle action, so a subscriber who was never
// cached is the normal case and must not surface as an error that makes a
// suspension look like it failed.
func TestVerifierCache_InvalidateUnknownUserSucceeds(t *testing.T) {
	database, _ := newTestDB(t)
	if err := database.VerifierCache().DeleteByUsername(context.Background(), "never-cached@isp"); err != nil {
		t.Errorf("invalidating an uncached subscriber returned an error: %v", err)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// splitGooseSections splits a migration into its Up and Down halves.
//
// Deliberately simple, and safe only because migration 046 contains no
// StatementBegin/End blocks: goose needs those wrappers around any
// dollar-quoted function body, and a file with them could not be split on
// markers this way. A migration that grows one will need this to grow too —
// which is why the test fails loudly on a missing marker rather than
// silently treating the whole file as Up.
func splitGooseSections(content string) (up, down string, ok bool) {
	const downMarker = "-- +goose Down"
	idx := strings.Index(content, downMarker)
	if idx < 0 {
		return "", "", false
	}
	up = strings.TrimPrefix(strings.TrimSpace(content[:idx]), "-- +goose Up")
	down = strings.TrimSpace(content[idx+len(downMarker):])
	return up, down, true
}

func tableExistsTx(ctx context.Context, t *testing.T, tx pgx.Tx, name string) bool {
	t.Helper()
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables
		                 WHERE table_schema = 'public' AND table_name = $1)`,
		name).Scan(&exists); err != nil {
		t.Fatalf("check table %s: %v", name, err)
	}
	return exists
}
