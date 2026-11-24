//go:build integration

package db_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A backup has to be taken by a role that can read the whole database.
//
// scripts/windows/backup.ps1 parsed DB_DSN out of app.env and ran pg_dump as
// the user named there, which is bss_app: the least-privilege role the
// services run as. Migration 019 revokes SELECT on lea_audit_log from it and
// revokes ALL on goose_db_version. pg_dump reads every table it dumps, and a
// missing SELECT is not a warning or a partial dump — it stops with
// "permission denied for table lea_audit_log" and exits non-zero.
//
// So on a standard installation that script could not produce a backup at
// all, while verify_restore.ps1 had always connected as postgres. The two
// halves disagreed about who takes a backup and only the restoring half was
// right. Nobody noticed, because no backup had ever been taken: the installer
// does not schedule one and D:\isp-backups was empty.
//
// This derives the rule rather than asserting a string. If bss_app is ever
// granted SELECT on everything, pg_dump as bss_app becomes legitimate and
// this test stops demanding the superuser — which is the correct behaviour,
// not a loophole.
func TestABackupCannotBeTakenByTheApplicationRole(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	var unreadable []string
	rows, err := pool.Query(ctx, `
		SELECT c.relname
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public'
		   AND c.relkind = 'r'
		   AND NOT has_table_privilege('bss_app', c.oid, 'SELECT')
		 ORDER BY 1`)
	if err != nil {
		if strings.Contains(err.Error(), "bss_app") {
			t.Skip("bss_app does not exist in this database; migration 019 did not run")
		}
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		unreadable = append(unreadable, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(repoRoot(t), "scripts", "windows", "backup.ps1")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read the backup script: %v", err)
	}
	text := string(body)

	// Which role does it run pg_dump as?
	dump := regexp.MustCompile(`pg_dump\.exe'\)\s*` + "`" + `\s*\n\s*-h \$dbHost -p \$dbPort -U (\S+)`).
		FindStringSubmatch(text)
	if dump == nil {
		t.Fatal("cannot tell which role backup.ps1 runs pg_dump as; this test is asserting nothing")
	}
	role := dump[1]

	if len(unreadable) == 0 {
		t.Logf("bss_app can read every table, so either role would work; backup.ps1 uses %s", role)
		return
	}
	if role == "$dbUser" {
		t.Errorf("backup.ps1 runs pg_dump as $dbUser, which is the role in DB_DSN (bss_app), "+
			"and that role cannot read %d table(s): %s.\n"+
			"pg_dump stops on the first one with \"permission denied\" and exits non-zero, "+
			"so this configuration cannot produce a backup at all.",
			len(unreadable), strings.Join(unreadable, ", "))
	}
	if !strings.Contains(text, "postgres_superuser.txt") {
		t.Error("backup.ps1 does not read postgres_superuser.txt, so it has no credential for " +
			"a role that can read the whole database — the same file verify_restore.ps1 uses")
	}
}

// Backup and restore must agree about who they connect as. They did not: the
// restore half was written against the superuser and the backup half against
// whatever was in DB_DSN.
func TestBackupAndRestoreUseTheSameRole(t *testing.T) {
	root := repoRoot(t)
	roleOf := func(name string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, "scripts", "windows", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		roles := map[string]bool{}
		for _, m := range regexp.MustCompile(`-U (\S+)`).FindAllStringSubmatch(string(body), -1) {
			roles[m[1]] = true
		}
		if len(roles) != 1 {
			var all []string
			for r := range roles {
				all = append(all, r)
			}
			t.Fatalf("%s connects as more than one role (%s); say which one takes the backup",
				name, strings.Join(all, ", "))
		}
		for r := range roles {
			return r
		}
		return ""
	}

	if backup, restore := roleOf("backup.ps1"), roleOf("verify_restore.ps1"); backup != restore {
		t.Errorf("backup.ps1 connects as %s and verify_restore.ps1 as %s. A backup one of them "+
			"cannot take is not a backup, and the disagreement is invisible until somebody "+
			"needs the restore.", backup, restore)
	}
}

// repoRoot locates the repository from this package's directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// The manifest query and the verification query must be the same text.
//
// verify_restore.ps1's first check is a string comparison of the counts it
// computes from the restored database against the counts backup.ps1 wrote
// into manifest.json. They are two copies of one SQL statement in two files,
// which is a drift waiting to happen — and it happened immediately: adding
// lea_audit and schema_version to the manifest, and not to the verifier, made
// every restore report "row counts differ".
//
// That failure mode is the bad direction. A verifier that cannot pass gets
// read as "the backup is broken", then as "the verifier is broken", then not
// run at all.
func TestTheManifestAndVerificationQueriesAgree(t *testing.T) {
	root := repoRoot(t)
	extract := func(name string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, "scripts", "windows", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// The counts statement is the here-string that starts with
		// SELECT 'subscribers='.
		m := regexp.MustCompile(`(?s)(SELECT 'subscribers='.*?)\n"@`).FindStringSubmatch(string(body))
		if m == nil {
			t.Fatalf("%s no longer contains a counts query this test can find; it is "+
				"asserting nothing", name)
		}
		// Compare on content, not on indentation.
		return strings.Join(strings.Fields(m[1]), " ")
	}

	backup := extract("backup.ps1")
	verify := extract("verify_restore.ps1")
	if backup != verify {
		t.Errorf("the counts query differs between the two scripts, so every restore "+
			"verification will report a mismatch that is not one.\n  backup.ps1:         %s\n"+
			"  verify_restore.ps1: %s", backup, verify)
	}
}
