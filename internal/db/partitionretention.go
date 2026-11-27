package db

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

// Dropping old monthly partitions, when somebody has decided how long to keep
// them.
//
// subscriber_session_history, cgnat_allocations and flow_records grow without
// bound: the roll-forward in partitionscanner.go adds months at the front and
// nothing has ever removed any from the back. On a real subscriber base
// flow_records alone is the largest table in the database within a year.
//
// **Off unless configured, and that is deliberate.** Dropping a partition
// destroys the CGNAT and session evidence in it, and how long that must be
// kept is a licence condition and a DPDP question, not an engineering
// default. FR-NET-001 requires the allocations to be recorded; nothing in the
// requirements says for how long. A default here would be somebody's guess
// deleting somebody else's evidence, so there is none: with no retention set,
// this reports the growth and drops nothing.
//
// What it will not do even when configured:
//
//   - Drop a partition that overlaps the retention window. Only partitions
//     entirely older than the cutoff go.
//   - Drop the current or any future partition, whatever the arithmetic says.
//     A retention of zero months would otherwise delete the partition being
//     written to.
//   - Drop more than one partition per pass per table, so a first run against
//     years of history cannot hold one long transaction over the largest
//     tables in the database.

// RetentionPolicy is how many months of history each monthly-partitioned
// table keeps. A table absent from the map, or set to zero, is never pruned.
//
// Keyed by table rather than global because the answers differ: CGNAT
// allocations answer a lawful-interception request and session history
// answers a billing dispute, and those are not the same obligation.
type RetentionPolicy map[string]int

// MonthsFor reports the retention for a table and whether one is set at all.
func (p RetentionPolicy) MonthsFor(table string) (int, bool) {
	if p == nil {
		return 0, false
	}
	m, ok := p[table]
	if !ok || m <= 0 {
		return 0, false
	}
	return m, true
}

// partitionSuffix matches the _YYYY_MM a monthly partition's name ends with.
var partitionSuffix = regexp.MustCompile(`_(\d{4})_(\d{2})$`)

// Partition is one child table of a monthly-partitioned parent.
type Partition struct {
	Parent string
	Name   string
	// Month is the first day of the month the partition holds, derived from
	// its name. Zero when the name does not carry one, which is how a
	// default partition or a hand-made child is recognised and left alone.
	Month time.Time
}

// MonthFromName derives the month a partition covers from its name.
//
// The name rather than the bound expression, and only here: the roll-forward
// reads real bounds because it must not trust a convention, but a *drop* must
// be conservative in the other direction. A child whose name does not match
// the convention is not something this code put there, so it is not something
// this code should delete.
func MonthFromName(parent, name string) (time.Time, bool) {
	m := partitionSuffix.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	year, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	if month < 1 || month > 12 || year < 2000 || year > 2200 {
		return time.Time{}, false
	}
	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.Local), true
}

const partitionListQuery = `
SELECT p.relname::text AS parent, c.relname::text AS child
  FROM pg_class p
  JOIN pg_namespace n ON n.oid = p.relnamespace AND n.nspname = 'public'
  JOIN pg_partitioned_table pt ON pt.partrelid = p.oid
  JOIN pg_inherits i ON i.inhparent = p.oid
  JOIN pg_class c ON c.oid = i.inhrelid
 WHERE p.relkind = 'p'
   AND pt.partstrat = 'r'
   AND pt.partnatts = 1
   AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
 ORDER BY p.relname, c.relname`

// ListPartitions returns every child of every monthly-partitioned parent.
func (s *MaintenanceStore) ListPartitions(ctx context.Context) ([]Partition, error) {
	rows, err := s.pool.Query(ctx, partitionListQuery)
	if err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	defer rows.Close()

	var out []Partition
	for rows.Next() {
		var p Partition
		if err := rows.Scan(&p.Parent, &p.Name); err != nil {
			return nil, fmt.Errorf("scan partition: %w", err)
		}
		if month, ok := MonthFromName(p.Parent, p.Name); ok {
			p.Month = month
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list partitions: %w", err)
	}
	return out, nil
}

// DropPartition removes one partition by name.
//
// The name is validated against the catalogue by the caller before it reaches
// here; this quotes it as an identifier regardless, because a DROP built from
// a string is the one place in this package where getting it wrong is
// unrecoverable.
func (s *MaintenanceStore) DropPartition(ctx context.Context, name string) error {
	if !partitionSuffix.MatchString(name) {
		return fmt.Errorf("refusing to drop %q: not a monthly partition name", name)
	}
	if _, err := s.pool.Exec(ctx,
		fmt.Sprintf("DROP TABLE IF EXISTS public.%s", quoteIdent(name))); err != nil {
		return fmt.Errorf("drop partition %s: %w", name, err)
	}
	return nil
}

// quoteIdent quotes an identifier for interpolation into DDL, which cannot
// take a parameter.
func quoteIdent(s string) string {
	return `"` + regexp.MustCompile(`"`).ReplaceAllString(s, `""`) + `"`
}

// DroppablePartitions reports which partitions the policy says are expired,
// as of now.
//
// Returned rather than dropped so a caller can log the decision before acting
// on it, and so a test can check the arithmetic without a DROP.
func DroppablePartitions(all []Partition, policy RetentionPolicy, now time.Time) []Partition {
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	var out []Partition
	for _, p := range all {
		months, set := policy.MonthsFor(p.Parent)
		if !set {
			continue
		}
		if p.Month.IsZero() {
			// A child whose name carries no month: not ours to judge.
			continue
		}
		// The oldest month still kept. A retention of 12 months keeps this
		// month and the eleven before it.
		//
		// This is also what protects the current month, and it is worth being
		// explicit about why rather than adding a second check for it:
		// MonthsFor refuses a retention below one, so months >= 1, so the
		// cutoff is never later than this month, so the partition being
		// written to is never expired. An earlier version had a separate
		// "never the current month" guard here; a negative control showed it
		// could not fire, which makes it dead code asserting a protection it
		// was not providing.
		cutoff := thisMonth.AddDate(0, -(months - 1), 0)
		if !p.Month.Before(cutoff) {
			continue
		}
		out = append(out, p)
	}
	return out
}
