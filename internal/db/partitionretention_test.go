//go:build integration

package db_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/db"
)

// Which partitions a retention policy says are expired.
//
// This arithmetic deletes CGNAT and session records, and the records it
// deletes are the ones a lawful-interception request asks for. A test that
// only checked the happy case would be worse than none, so what follows is
// mostly the cases where it must refuse: the current month, a future month, a
// table with no policy, a child whose name it does not recognise, and a
// retention of zero.

func ctxBackground() context.Context { return context.Background() }

func part(parent, name string) db.Partition {
	p := db.Partition{Parent: parent, Name: name}
	if m, ok := db.MonthFromName(parent, name); ok {
		p.Month = m
	}
	return p
}

// now is fixed so the arithmetic is not a function of the day the suite runs.
func fixedNow() time.Time {
	return time.Date(2026, 11, 15, 10, 0, 0, 0, time.Local)
}

func names(ps []db.Partition) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return strings.Join(out, ", ")
}

// With no policy, nothing is droppable. This is the configured state of every
// deployment today, and it must stay the safe one.
func TestNoRetentionPolicyDropsNothing(t *testing.T) {
	all := []db.Partition{
		part("subscriber_session_history", "subscriber_session_history_2020_01"),
		part("cgnat_allocations", "cgnat_allocations_2019_06"),
		part("flow_records", "flow_records_2018_12"),
	}

	for name, policy := range map[string]db.RetentionPolicy{
		"nil":               nil,
		"empty":             {},
		"zero months":       {"subscriber_session_history": 0},
		"negative months":   {"subscriber_session_history": -12},
		"a different table": {"some_other_table": 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := db.DroppablePartitions(all, policy, fixedNow()); len(got) != 0 {
				t.Errorf("with %s retention, these were droppable: %s", name, names(got))
			}
		})
	}
}

// The current month is never droppable, whatever the retention says: it is
// the partition being written to.
//
// What provides that is the cutoff arithmetic, not a separate check.
// MonthsFor refuses a retention below one month, so the cutoff can never be
// later than this month. An earlier version of the code had an explicit
// "never the current month" guard as well; removing it did not make this test
// fail, which is how it was found to be unreachable. The protection is real;
// the second guard was not.
func TestTheCurrentAndFutureMonthsAreNeverDroppable(t *testing.T) {
	all := []db.Partition{
		part("flow_records", "flow_records_2026_11"), // the current month
		part("flow_records", "flow_records_2026_12"),
		part("flow_records", "flow_records_2027_01"),
	}

	// Every retention from one month upward, including the one that would
	// otherwise put the cutoff on top of the current month.
	for _, months := range []int{1, 2, 3, 12, 60} {
		if got := db.DroppablePartitions(all, db.RetentionPolicy{"flow_records": months},
			fixedNow()); len(got) != 0 {
			t.Errorf("retention of %d month(s) made these droppable: %s", months, names(got))
		}
	}
}

// A retention of n months keeps this month and the n-1 before it.
func TestRetentionKeepsThisMonthAndTheMonthsBefore(t *testing.T) {
	all := []db.Partition{
		part("flow_records", "flow_records_2026_07"),
		part("flow_records", "flow_records_2026_08"),
		part("flow_records", "flow_records_2026_09"),
		part("flow_records", "flow_records_2026_10"),
		part("flow_records", "flow_records_2026_11"), // current
	}

	for months, wantDropped := range map[int][]string{
		// 1 month keeps November only.
		1: {"flow_records_2026_07", "flow_records_2026_08",
			"flow_records_2026_09", "flow_records_2026_10"},
		// 3 months keeps September, October, November.
		3: {"flow_records_2026_07", "flow_records_2026_08"},
		// 5 months keeps everything here.
		5: nil,
		// More than exists keeps everything.
		24: nil,
	} {
		got := names(db.DroppablePartitions(all, db.RetentionPolicy{"flow_records": months},
			fixedNow()))
		want := strings.Join(wantDropped, ", ")
		if got != want {
			t.Errorf("retention %d months: dropped [%s], want [%s]", months, got, want)
		}
	}
}

// Retention is per table, because the obligations differ: CGNAT allocations
// answer a lawful-interception request and session history answers a billing
// dispute.
func TestRetentionIsPerTable(t *testing.T) {
	all := []db.Partition{
		part("cgnat_allocations", "cgnat_allocations_2026_01"),
		part("subscriber_session_history", "subscriber_session_history_2026_01"),
		part("flow_records", "flow_records_2026_01"),
	}
	policy := db.RetentionPolicy{
		"subscriber_session_history": 3,  // January 2026 is expired
		"cgnat_allocations":          24, // January 2026 is kept
		// flow_records: no policy at all
	}

	got := names(db.DroppablePartitions(all, policy, fixedNow()))
	if got != "subscriber_session_history_2026_01" {
		t.Errorf("dropped [%s]; only the table with a short retention should expire", got)
	}
}

// A child whose name carries no month is left alone. It is not something the
// roll-forward created, so it is not something retention should delete.
func TestAPartitionWithAnUnrecognisedNameIsLeftAlone(t *testing.T) {
	all := []db.Partition{
		part("flow_records", "flow_records_default"),
		part("flow_records", "flow_records_archive_old"),
		part("flow_records", "flow_records_2026_13"), // not a month
		part("flow_records", "flow_records_1999_01"), // out of range
		part("flow_records", "flow_records_2026_01"), // a real one, expired
	}
	policy := db.RetentionPolicy{"flow_records": 3}

	got := names(db.DroppablePartitions(all, policy, fixedNow()))
	if got != "flow_records_2026_01" {
		t.Errorf("dropped [%s]; only the recognisably monthly expired partition should go", got)
	}
}

// MonthFromName is the only thing standing between the DROP and a table it
// should not touch, so its refusals matter more than its successes.
func TestMonthFromNameRefusesAnythingItDoesNotRecognise(t *testing.T) {
	for _, name := range []string{
		"flow_records",
		"flow_records_default",
		"flow_records_2026",
		"flow_records_2026_00",
		"flow_records_2026_13",
		"flow_records_1900_01",
		"flow_records_9999_01",
		"flow_records_202611",
		"flow_records_2026_1",
		"",
	} {
		if _, ok := db.MonthFromName("flow_records", name); ok {
			t.Errorf("%q was read as a monthly partition", name)
		}
	}

	for name, want := range map[string]string{
		"flow_records_2026_01":               "2026-01-01",
		"cgnat_allocations_2026_12":          "2026-12-01",
		"subscriber_session_history_2027_06": "2027-06-01",
	} {
		got, ok := db.MonthFromName("x", name)
		if !ok {
			t.Errorf("%q was not read as a monthly partition", name)
			continue
		}
		if got.Format("2006-01-02") != want {
			t.Errorf("%q -> %s, want %s", name, got.Format("2006-01-02"), want)
		}
	}
}

// Dropping refuses a name that is not a monthly partition, even if a caller
// asks. The catalogue check is upstream; this is the backstop, and a DROP
// built from a string is the one place where being wrong is unrecoverable.
func TestDropRefusesANameThatIsNotAMonthlyPartition(t *testing.T) {
	database, _ := newTestDB(t)
	store := database.Maintenance()

	for _, name := range []string{
		"subscribers",
		"flow_records",
		"flow_records_default",
		`flow_records_2026_01"; DROP TABLE subscribers; --`,
		"",
	} {
		if err := store.DropPartition(ctxBackground(), name); err == nil {
			t.Errorf("DropPartition(%q) was accepted", name)
		}
	}

	// And the real tables are still there.
	var n int
	_, pool := newTestDB(t)
	if err := pool.QueryRow(ctxBackground(),
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema='public' AND table_name='subscribers'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("subscribers is gone")
	}
}

// A real partition drops, and only the one asked for.
func TestDroppingARealPartitionRemovesOnlyThatOne(t *testing.T) {
	database, pool := newTestDB(t)
	store := database.Maintenance()

	// A partition far enough out that nothing else in the suite uses it.
	const name = "flow_records_2031_07"
	if _, err := pool.Exec(ctxBackground(),
		`CREATE TABLE IF NOT EXISTS public.`+name+
			` PARTITION OF public.flow_records FOR VALUES FROM ('2031-07-01') TO ('2031-08-01')`); err != nil {
		t.Fatalf("create the probe partition: %v", err)
	}

	before, err := store.ListPartitions(ctxBackground())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(names(before), name) {
		t.Fatalf("the probe partition is not listed: %s", names(before))
	}

	if err := store.DropPartition(ctxBackground(), name); err != nil {
		t.Fatalf("drop: %v", err)
	}

	after, err := store.ListPartitions(ctxBackground())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names(after), name) {
		t.Error("the partition is still listed after the drop")
	}
	// Every other partition survived.
	if len(after) != len(before)-1 {
		t.Errorf("partition count went from %d to %d; exactly one should have gone",
			len(before), len(after))
	}
	// And the parent is intact.
	var n int
	if err := pool.QueryRow(ctxBackground(),
		`SELECT count(*) FROM information_schema.tables
		  WHERE table_schema='public' AND table_name='flow_records'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("the parent table was dropped with the partition")
	}
}

// ListPartitions excludes a DEFAULT partition, because one would absorb every
// row and turn the whole mechanism off — and it must certainly never be
// dropped as though it held one month.
func TestListPartitionsExcludesADefaultPartition(t *testing.T) {
	database, pool := newTestDB(t)
	store := database.Maintenance()

	if _, err := pool.Exec(ctxBackground(),
		`CREATE TABLE IF NOT EXISTS public.flow_records_probe_default
		   PARTITION OF public.flow_records DEFAULT`); err != nil {
		t.Skipf("cannot create a default partition here: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctxBackground(), `DROP TABLE IF EXISTS public.flow_records_probe_default`)
	})

	got, err := store.ListPartitions(ctxBackground())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(names(got), "probe_default") {
		t.Error("the default partition is listed as a monthly one")
	}
}

// The smallest retention MonthsFor will accept still keeps the current month.
// This is the assertion the removed guard was pretending to be.
func TestTheSmallestAcceptedRetentionStillKeepsTheCurrentMonth(t *testing.T) {
	now := fixedNow()
	current := part("flow_records", "flow_records_2026_11")

	// Below one month is refused outright, so one is the floor.
	if _, set := (db.RetentionPolicy{"flow_records": 0}).MonthsFor("flow_records"); set {
		t.Error("a retention of zero months was accepted; it would expire the partition " +
			"currently being written to")
	}
	if _, set := (db.RetentionPolicy{"flow_records": -1}).MonthsFor("flow_records"); set {
		t.Error("a negative retention was accepted")
	}

	if got := db.DroppablePartitions([]db.Partition{current},
		db.RetentionPolicy{"flow_records": 1}, now); len(got) != 0 {
		t.Errorf("a one-month retention made the current month droppable: %s", names(got))
	}
}
