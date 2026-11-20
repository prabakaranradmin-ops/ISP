//go:build integration

// Append-only staff audit log — FR-OBS-006 | migration 062.
//
// The claim worth proving against a real database is that the table is
// append-only against the role the services run as. Everything else here
// is ordinary filtering, but "append-only" is a privilege-and-RLS fact
// that only Postgres can confirm — and migration 019's own comment
// records that this exact design was once asserted and not enforced.
package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

func auditEntry(actor, action, target string, detail map[string]any) middleware.AuditEntry {
	return middleware.AuditEntry{
		CorrelationID: "corr-" + action, ActorID: actor, ActorRole: "isp_owner",
		Action: action, Target: target, Detail: detail,
	}
}

func TestFR_OBS_006_AuditEntriesAreRecordedAndReadBack(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)
	store := database.Audit()

	if err := store.RecordAudit(ctx, auditEntry("meena", "subscriber.plan_change", "42",
		map[string]any{"new_plan_id": 7, "note": "customer asked"})); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := store.RecordAudit(ctx, auditEntry("ravi", "product.attached", "ROUTER", nil)); err != nil {
		t.Fatalf("record: %v", err)
	}

	all, err := store.ListAudit(ctx, middleware.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 entries, got %d", len(all))
	}
	// Newest first: an audit screen opens on what just happened.
	if all[0].Action != "product.attached" {
		t.Errorf("first entry is %q; the trail is not newest-first", all[0].Action)
	}

	var planChange *middleware.AuditRecord
	for i := range all {
		if all[i].Action == "subscriber.plan_change" {
			planChange = &all[i]
		}
	}
	if planChange == nil {
		t.Fatal("the plan change entry is missing")
	}
	if planChange.ActorID != "meena" || planChange.Target != "42" {
		t.Errorf("entry reads %+v", planChange)
	}
	if planChange.CorrelationID == "" {
		t.Error("the correlation id was not stored")
	}
	// The detail survives the JSONB round trip. A JSON number comes back
	// as float64, which is why the console formats whole numbers as
	// integers rather than printing 7 as 7e+00.
	if got, ok := planChange.Detail["new_plan_id"].(float64); !ok || got != 7 {
		t.Errorf("detail new_plan_id came back as %#v", planChange.Detail["new_plan_id"])
	}
	if planChange.Detail["note"] != "customer asked" {
		t.Errorf("detail note came back as %#v", planChange.Detail["note"])
	}

	t.Run("an entry with no detail is fine", func(t *testing.T) {
		for _, e := range all {
			if e.Action == "product.attached" && len(e.Detail) != 0 {
				t.Errorf("a detail-less entry came back with %v", e.Detail)
			}
		}
	})

	t.Run("an action with no actor is stored as unknown, not empty", func(t *testing.T) {
		// The column is NOT NULL and an empty actor would read as if
		// nobody did it. A run of "unknown" means a path is not setting
		// its subject, which is the bug worth noticing.
		if err := store.RecordAudit(ctx, auditEntry("", "something.happened", "x", nil)); err != nil {
			t.Fatalf("record with no actor: %v", err)
		}
		got, err := store.ListAudit(ctx, middleware.AuditFilter{Actor: "unknown"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 unknown-actor entry, got %d", len(got))
		}
	})

	t.Run("an entry with no action at all is refused", func(t *testing.T) {
		if err := store.RecordAudit(ctx, auditEntry("meena", "   ", "x", nil)); err == nil {
			t.Error("an actionless audit entry was accepted")
		}
	})
}

func TestFR_OBS_006_AuditFiltersByActorActionAndDate(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	store := database.Audit()

	for _, e := range []middleware.AuditEntry{
		auditEntry("meena", "subscriber.plan_change", "1", nil),
		auditEntry("meena", "product.attached", "2", nil),
		auditEntry("ravi", "subscriber.plan_change", "3", nil),
	} {
		if err := store.RecordAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	// One entry backdated, so the date filter has something to exclude.
	if _, err := pool.Exec(ctx, `
		INSERT INTO staff_audit_log (correlation_id, actor_id, actor_role, action, target, created_at)
		VALUES ('old', 'meena', 'isp_owner', 'plan.updated', 'Old', now() - interval '40 days')`); err != nil {
		t.Fatal(err)
	}

	t.Run("by actor", func(t *testing.T) {
		got, err := store.ListAudit(ctx, middleware.AuditFilter{Actor: "ravi"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].ActorID != "ravi" {
			t.Errorf("got %d entries for ravi", len(got))
		}
	})

	t.Run("by action", func(t *testing.T) {
		got, err := store.ListAudit(ctx, middleware.AuditFilter{Action: "subscriber.plan_change"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("got %d plan changes, want 2", len(got))
		}
	})

	t.Run("by actor and action together", func(t *testing.T) {
		got, err := store.ListAudit(ctx, middleware.AuditFilter{
			Actor: "meena", Action: "subscriber.plan_change"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Errorf("got %d, want 1", len(got))
		}
	})

	t.Run("by date, excluding what falls outside", func(t *testing.T) {
		from := time.Now().UTC().AddDate(0, 0, -1)
		got, err := store.ListAudit(ctx, middleware.AuditFilter{From: &from})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Errorf("got %d entries since yesterday, want 3 (the 40-day-old one excluded)", len(got))
		}

		// And the backdated one is findable by widening the window.
		wide := time.Now().UTC().AddDate(0, 0, -60)
		got, err = store.ListAudit(ctx, middleware.AuditFilter{From: &wide})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 4 {
			t.Errorf("got %d entries over 60 days, want 4", len(got))
		}
	})

	t.Run("the count matches the filter", func(t *testing.T) {
		n, err := store.CountAudit(ctx, middleware.AuditFilter{Actor: "meena"})
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Errorf("count says %d for meena, want 3", n)
		}
	})

	t.Run("the filter dropdowns list what is there", func(t *testing.T) {
		actors, err := store.ListAuditActors(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(actors) != 2 {
			t.Errorf("actors are %v, want meena and ravi", actors)
		}
		actions, err := store.ListAuditActions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(actions) != 3 {
			t.Errorf("actions are %v, want three distinct ones", actions)
		}
	})

	t.Run("the limit is capped rather than unbounded", func(t *testing.T) {
		got, err := store.ListAudit(ctx, middleware.AuditFilter{Limit: 100000})
		if err != nil {
			t.Fatal(err)
		}
		// Only 4 rows exist; the point is that an absurd limit is accepted
		// and clamped rather than handed to the database.
		if len(got) != 4 {
			t.Errorf("got %d", len(got))
		}
	})
}

// The append-only claim, against the role the services actually run as.
//
// bss_app holds INSERT and SELECT; UPDATE and DELETE are revoked, and RLS
// denies by default what it has no policy for. Tested by connecting AS
// bss_app rather than as the test's own superuser — against which, as
// migration 019 records, RLS and revokes do nothing at all. That is the
// mistake this test exists to prevent repeating.
func TestFR_OBS_006_TheTrailIsAppendOnlyAgainstTheApplicationRole(t *testing.T) {
	ctx := context.Background()
	database, pool := newTestDB(t)
	if err := database.Audit().RecordAudit(ctx,
		auditEntry("meena", "subscriber.plan_change", "42", nil)); err != nil {
		t.Fatal(err)
	}

	// Does bss_app exist in this database? It is created by migration 019
	// without a password, and the test harness connects as the superuser.
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'bss_app')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Skip("bss_app does not exist in this database; migration 019 did not run")
	}

	// SET ROLE rather than a second connection: it drops the session to
	// bss_app's privileges, which is exactly what the services have, and
	// needs no password.
	//
	// One transaction per statement, and that matters. An earlier version
	// of this test ran all four statements in one transaction — and a
	// failed statement aborts a Postgres transaction, so every statement
	// after the first failure returned "current transaction is aborted"
	// rather than "permission denied". The delete and truncate
	// assertions passed on that error and proved nothing about
	// privileges at all.
	asBssApp := func(t *testing.T, sql string) error {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE bss_app`); err != nil {
			t.Skipf("cannot assume the bss_app role here: %v", err)
		}
		_, err = tx.Exec(ctx, sql)
		return err
	}

	t.Run("it can read", func(t *testing.T) {
		if err := asBssApp(t, `SELECT COUNT(*) FROM staff_audit_log`); err != nil {
			t.Fatalf("bss_app cannot read the trail, which FR-OBS-006 requires: %v", err)
		}
	})

	t.Run("it can append", func(t *testing.T) {
		if err := asBssApp(t, `
			INSERT INTO staff_audit_log (correlation_id, actor_id, action)
			VALUES ('c', 'meena', 'test.append')`); err != nil {
			t.Fatalf("bss_app cannot append to the trail: %v", err)
		}
	})

	// Each of these asserts on the error text as well as its presence, so
	// a transaction-abort or a missing table cannot stand in for a
	// privilege denial.
	t.Run("it cannot change an entry", func(t *testing.T) {
		err := asBssApp(t, `UPDATE staff_audit_log SET actor_id = 'somebody else'`)
		if err == nil {
			t.Fatal("bss_app rewrote an audit entry; the trail is not append-only")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			t.Errorf("the update failed for the wrong reason: %v", err)
		}
	})

	t.Run("it cannot delete an entry", func(t *testing.T) {
		err := asBssApp(t, `DELETE FROM staff_audit_log`)
		if err == nil {
			t.Fatal("bss_app deleted an audit entry; the trail is not append-only")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			t.Errorf("the delete failed for the wrong reason: %v", err)
		}
	})

	t.Run("it cannot truncate the table", func(t *testing.T) {
		err := asBssApp(t, `TRUNCATE staff_audit_log`)
		if err == nil {
			t.Fatal("bss_app truncated the trail; the trail is not append-only")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "permission denied") {
			t.Errorf("the truncate failed for the wrong reason: %v", err)
		}
	})
}
