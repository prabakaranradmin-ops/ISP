//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/db"
)

// The monthly partition roll-forward — migration 076.
//
// subscriber_session_history (010), cgnat_allocations (011) and flow_records
// (052) are RANGE partitioned by month with no default partition, so a row
// whose timestamp falls outside every partition is refused: "no partition of
// relation ... found for row". The migrations provision the current month
// plus three; nothing provisioned any more than that, so every deployment had
// roughly four months before RADIUS stopped recording sessions, CGNAT stopped
// recording port-block allocations and NetFlow stopped recording flows.
//
// Authentication is unaffected, which is what makes it bad. The subscriber
// sees nothing, the console sees nothing, and the trail required by FR-NET-001
// — a licence condition, not a feature — simply has a hole in it starting on
// a date nobody wrote down.

// Coverage must be ahead of the writes, always. This is the assertion that
// would have failed on any day between the four-month mark and now.
func TestPartitionCoverageIsAheadOfTheWrites(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)

	cov, err := database.Maintenance().PartitionCoverage(ctx)
	if err != nil {
		t.Fatalf("read partition coverage: %v", err)
	}
	if len(cov) == 0 {
		t.Fatal("no monthly-partitioned tables found; either the schema is not what this " +
			"test was written against, or the query no longer finds them — and an empty " +
			"result here would otherwise pass as 'nothing to check'")
	}

	// The three that exist today must be among them. Named not as the list to
	// maintain — the query derives the list — but so that a schema change
	// that stops a table being partitioned is noticed here rather than in a
	// deployment.
	found := map[string]bool{}
	for _, c := range cov {
		found[c.Table] = true
	}
	for _, want := range []string{
		"subscriber_session_history", "cgnat_allocations", "flow_records",
	} {
		if !found[want] {
			t.Errorf("%s is no longer a monthly-partitioned table, or the coverage query "+
				"cannot see it", want)
		}
	}

	now := time.Now()
	for _, c := range cov {
		months := c.MonthsAhead(now)
		if months < 0 {
			t.Errorf("%s has no partition covering the current month (latest bound %s). "+
				"Every insert into it is being refused right now.",
				c.Table, c.CoveredUntil.Format("2006-01-02"))
			continue
		}
		if months < 1 {
			t.Errorf("%s runs out of partitions when this month ends (%s). The roll-forward "+
				"keeps %d months; this means it is not running or not permitted.",
				c.Table, c.CoveredUntil.Format("2006-01-02"), db.PartitionMonthsAhead)
		}
	}
}

// A pass provisions the runway it promises, and says so from a second read
// rather than from having tried.
func TestAPassLeavesThreeMonthsOfRunway(t *testing.T) {
	ctx := context.Background()
	database, _ := newTestDB(t)

	scanner := db.NewPartitionScanner(database.Maintenance())
	if err := scanner.Once(ctx); err != nil {
		t.Fatalf("one pass: %v", err)
	}

	cov, err := database.Maintenance().PartitionCoverage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, c := range cov {
		if got := c.MonthsAhead(now); got < db.PartitionMonthsAhead {
			t.Errorf("%s has %d months of coverage after a pass, want at least %d "+
				"(covered until %s)",
				c.Table, got, db.PartitionMonthsAhead, c.CoveredUntil.Format("2006-01-02"))
		}
	}

	// Twice over is the same as once: the scanner runs daily and does nothing
	// on all but a few days of the month.
	if err := scanner.Once(ctx); err != nil {
		t.Fatalf("a second pass must be a no-op, got: %v", err)
	}
}

// The role that actually runs this in production can call it.
//
// This is the assertion the feature turns on. The services connect as bss_app
// (migration 019), which holds DML and nothing else: CREATE TABLE ...
// PARTITION OF needs CREATE on the schema and ownership of the parent, and
// bss_app has neither. Before migration 076 made the function SECURITY
// DEFINER and granted EXECUTE, a roll-forward wired into the services would
// have failed on every pass with "must be owner of table" — while the code
// read as though the problem were solved.
//
// Run as the superuser, this proves nothing at all, which is why it assumes
// the role first.
func TestTheApplicationRoleCanProvisionPartitions(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'bss_app')`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Skip("bss_app does not exist in this database; migration 019 did not run")
	}

	// In a transaction that rolls back: the partitions this creates are a
	// year out and have no business surviving the test.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `SET LOCAL ROLE bss_app`); err != nil {
		t.Skipf("cannot assume the bss_app role here: %v", err)
	}

	// Twelve months, well past what exists, so this has to create something
	// rather than find everything already there and succeed vacuously.
	if _, err := tx.Exec(ctx,
		`SELECT create_monthly_partitions('subscriber_session_history', 12)`); err != nil {
		t.Fatalf("bss_app cannot provision partitions: %v\n"+
			"This is what the services run as. Without EXECUTE on a SECURITY DEFINER "+
			"create_monthly_partitions (migration 076), the roll-forward fails on every "+
			"pass and the tables run out of partitions four months after deployment.", err)
	}

	// And it really created them, rather than returning quietly.
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c
		  JOIN pg_namespace ns ON ns.oid = c.relnamespace AND ns.nspname = 'public'
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = 'subscriber_session_history'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 13 {
		t.Errorf("asked for the current month plus twelve and found %d partitions", n)
	}
}

// The widened privilege is bounded. A SECURITY DEFINER function that takes a
// table name has to refuse the names it was not meant to take, because it is
// running as the owner of every table in the database.
func TestProvisioningRefusesAnythingThatIsNotAPartitionedTable(t *testing.T) {
	ctx := context.Background()
	_, pool := newTestDB(t)

	for _, name := range []string{
		"subscribers",     // a real table, not partitioned
		"pg_class",        // a catalogue relation
		"does_not_exist",  // nothing
		"subscribers; --", // not a valid identifier either
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pool.Exec(ctx, `SELECT create_monthly_partitions($1, 1)`, name)
			if err == nil {
				t.Fatalf("create_monthly_partitions(%q) succeeded; it runs as the table "+
					"owner and must refuse anything that is not a partitioned table in "+
					"public", name)
			}
			if !strings.Contains(err.Error(), "is not a partitioned table") {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// MonthsAhead counts whole months, not fractions of one.
//
// It is read by an alert, so "nearly a month left" on the 30th must not read
// as a month: the bound is exclusive, and a partition ending on 1 December
// covers November and nothing more.
func TestMonthsAheadCountsWholeMonths(t *testing.T) {
	nov30 := time.Date(2026, 11, 30, 23, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		bound time.Time
		want  int
	}{
		{"no partitions at all", time.Time{}, -1},
		{"ended last month", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), -1},
		{"covers this month only", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), 0},
		{"covers next month too", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 1},
		{"three months ahead", time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), 3},
		{"across a year boundary", time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC), 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := db.PartitionCoverage{Table: "t", CoveredUntil: tc.bound}
			if got := c.MonthsAhead(nov30); got != tc.want {
				t.Errorf("MonthsAhead = %d, want %d", got, tc.want)
			}
		})
	}
}
