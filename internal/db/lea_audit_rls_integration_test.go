//go:build integration

// Proves migration 019's least-privilege bss_app role is what actually
// enforces lea_audit_log's append-only guarantee. Every other test in this
// package connects as the postgres superuser (via TEST_DB_DSN), which
// PostgreSQL's row-level security always exempts regardless of policy —
// so this is the one test that must connect as bss_app specifically, or it
// proves nothing.
package db_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLEAAuditLog_AppRoleBlockedFromUpdateDelete | DoD Phase 1 Step 2 |
// SecD §9.7, DBD §6.5
func TestLEAAuditLog_AppRoleBlockedFromUpdateDelete(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	const probeRole = "bss_app_rls_probe"
	const probePassword = "integration-test-app-role-password"

	// A dedicated member of bss_app, rather than bss_app itself.
	//
	// This used to run ALTER ROLE bss_app WITH PASSWORD. PostgreSQL roles are
	// cluster-wide, not per-database, so that changed the password of the role
	// the live application logs in with — on any machine where the test
	// database shares a cluster with a real one, which is every native install.
	// The damage is invisible until the running services next open a
	// connection: pgxpool keeps the ones it already authenticated, so health
	// checks go on passing while the stack is quietly unable to reconnect.
	// scripts/run_db_tests.sh gave a throwaway container, which is why this was
	// safe when it was written and stopped being safe when the suite grew a
	// second way to run.
	//
	// A member role inherits bss_app's table grants, and RLS policies naming
	// bss_app match it too (policy roles match by membership, not identity), so
	// the privilege surface under test is the same one. NOINHERIT is not set
	// and must not be: inheritance is exactly what makes this equivalent.
	// NOBYPASSRLS is explicit because a role that could bypass RLS would make
	// this test silently vacuous.
	//
	// Not parameterized: CREATE ROLE ... PASSWORD expects a string literal in
	// PostgreSQL's grammar, not a bind parameter — both constants are fixed
	// here, not external input.
	if _, err := pool.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", probeRole)); err != nil {
		t.Fatalf("drop stale probe role: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(
		"CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '%s' IN ROLE bss_app",
		probeRole, probePassword)); err != nil {
		t.Fatalf("create probe role: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), fmt.Sprintf("DROP ROLE IF EXISTS %s", probeRole)); err != nil {
			t.Logf("could not drop probe role %s: %v", probeRole, err)
		}
	})

	appDSN, err := dsnAsRole(os.Getenv("TEST_DB_DSN"), probeRole, probePassword)
	if err != nil {
		t.Fatalf("build bss_app DSN: %v", err)
	}
	appPool, err := pgxpool.New(ctx, appDSN)
	if err != nil {
		t.Fatalf("connect as bss_app: %v", err)
	}
	defer appPool.Close()

	// Seed one row as the superuser — bss_app can only INSERT, which is
	// exercised in its own subtest below rather than relied on here.
	if _, err := pool.Exec(ctx, `
		INSERT INTO lea_audit_log (accessor_identity, accessor_role, queried_public_ip, queried_timestamp, result_row_count)
		VALUES ('rls-test-officer', 'noc_engineer', '203.0.113.9', NOW(), 1)`); err != nil {
		t.Fatalf("seed lea_audit_log row: %v", err)
	}

	t.Run("UPDATE is denied", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `UPDATE lea_audit_log SET accessor_identity = 'tampered' WHERE accessor_identity = 'rls-test-officer'`)
		if err == nil {
			t.Fatal("expected UPDATE to be denied for bss_app, it succeeded")
		}
	})

	t.Run("DELETE is denied", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `DELETE FROM lea_audit_log WHERE accessor_identity = 'rls-test-officer'`)
		if err == nil {
			t.Fatal("expected DELETE to be denied for bss_app, it succeeded")
		}
	})

	t.Run("the seeded row survived untouched", func(t *testing.T) {
		var identity string
		err := pool.QueryRow(ctx, `
			SELECT accessor_identity FROM lea_audit_log
			WHERE accessor_identity IN ('rls-test-officer', 'tampered')
			ORDER BY id DESC LIMIT 1`).Scan(&identity)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if identity != "rls-test-officer" {
			t.Errorf("row was modified despite a denied UPDATE/DELETE: accessor_identity = %q", identity)
		}
	})

	t.Run("SELECT is denied (the app never reads this table back)", func(t *testing.T) {
		var count int
		err := appPool.QueryRow(ctx, `SELECT COUNT(*) FROM lea_audit_log`).Scan(&count)
		if err == nil {
			t.Fatal("expected SELECT to be denied for bss_app, it succeeded")
		}
	})

	t.Run("INSERT still works — the one thing bss_app needs to do", func(t *testing.T) {
		_, err := appPool.Exec(ctx, `
			INSERT INTO lea_audit_log (accessor_identity, accessor_role, queried_public_ip, queried_timestamp, result_row_count)
			VALUES ('rls-test-officer-2', 'noc_engineer', '203.0.113.10', NOW(), 0)`)
		if err != nil {
			t.Fatalf("bss_app should be able to INSERT: %v", err)
		}
	})
}

// dsnAsRole rewrites a postgres:// DSN's user/password, keeping host, port,
// dbname, and query params (e.g. sslmode) unchanged.
func dsnAsRole(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}
