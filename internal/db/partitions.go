package db

import (
	"context"
	"fmt"
	"time"
)

// Monthly partition roll-forward.
//
// Three tables are RANGE partitioned by month — subscriber_session_history,
// cgnat_allocations and flow_records — and an INSERT with no matching
// partition does not fall back anywhere. PostgreSQL refuses it: "no partition
// of relation ... found for row". create_monthly_partitions() creates the
// current month plus three, and until migration 076 nothing called it after
// the migration that defines it, so every deployment had about four months of
// coverage and then stopped recording sessions, CGNAT allocations and flows.
//
// Which tables need rolling forward is read from the catalogue rather than
// listed here. A list is the thing that gets forgotten: the next time someone
// adds a monthly table, the list is the file they will not think to open, and
// the symptom arrives four months later in somebody else's deployment.

// PartitionCoverage is how far ahead one partitioned table is provisioned.
type PartitionCoverage struct {
	// Table is the partitioned parent, in schema public.
	Table string
	// CoveredUntil is the exclusive upper bound of its latest partition.
	// Zero when the table has no partitions at all, which is the state that
	// makes every insert fail immediately.
	CoveredUntil time.Time
}

// MonthsAhead is how many whole months of coverage remain after now.
//
// Zero means the current month is the last one covered — writes continue
// until the month turns and then stop. Negative means they have already
// stopped.
func (c PartitionCoverage) MonthsAhead(now time.Time) int {
	if c.CoveredUntil.IsZero() {
		return -1
	}
	// Measured in whole months from the start of the current month, matching
	// how the partitions are cut, so this does not read as "nearly a month
	// left" on the 30th.
	thisMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	bound := c.CoveredUntil.In(now.Location())
	months := (bound.Year()-thisMonth.Year())*12 + int(bound.Month()-thisMonth.Month())
	// The bound is exclusive: a partition ending 1 Dec covers November only,
	// which is zero months ahead.
	return months - 1
}

// MaintenanceStore holds the database housekeeping the services run
// themselves rather than leaving to an operator's calendar.
type MaintenanceStore struct{ pool dbPool }

// Maintenance returns the housekeeping store.
func (d *DB) Maintenance() *MaintenanceStore { return &MaintenanceStore{pool: d.pool} }

// partitionCoverageQuery reports every monthly-partitioned table and how far
// it is provisioned.
//
// Restricted to RANGE partitioning on a single date or timestamp column,
// because those are the ones a monthly roll-forward is the right answer for.
// A table partitioned by range over an id would otherwise be handed to
// create_monthly_partitions, which would cut partitions it cannot use.
//
// The upper bound is read from the partition's own bound expression rather
// than from its name: the name is a convention of the function that creates
// them, and a convention is not evidence. PostgreSQL renders a single-column
// range bound as `FOR VALUES FROM ('...') TO ('...')`, and a DEFAULT
// partition renders as `DEFAULT`, which matches nothing here and is skipped —
// correctly, since a default partition would silently absorb every row and
// turn this whole mechanism off.
const partitionCoverageQuery = `
SELECT p.relname::text AS parent,
       max((regexp_match(pg_get_expr(c.relpartbound, c.oid),
                         'TO \(''([^'']+)''\)'))[1]::timestamptz) AS covered_until
  FROM pg_class p
  JOIN pg_namespace n ON n.oid = p.relnamespace AND n.nspname = 'public'
  JOIN pg_partitioned_table pt ON pt.partrelid = p.oid
  JOIN pg_attribute a ON a.attrelid = p.oid
                     AND a.attnum = pt.partattrs[0]
  LEFT JOIN pg_inherits i ON i.inhparent = p.oid
  LEFT JOIN pg_class c ON c.oid = i.inhrelid
 WHERE p.relkind = 'p'
   AND pt.partstrat = 'r'
   AND pt.partnatts = 1
   AND a.atttypid IN ('date'::regtype, 'timestamp'::regtype, 'timestamptz'::regtype)
 GROUP BY p.relname
 ORDER BY p.relname`

// PartitionCoverage reports how far ahead each monthly-partitioned table is
// provisioned.
func (s *MaintenanceStore) PartitionCoverage(ctx context.Context) ([]PartitionCoverage, error) {
	rows, err := s.pool.Query(ctx, partitionCoverageQuery)
	if err != nil {
		return nil, fmt.Errorf("read partition coverage: %w", err)
	}
	defer rows.Close()

	var out []PartitionCoverage
	for rows.Next() {
		var c PartitionCoverage
		var until *time.Time
		if err := rows.Scan(&c.Table, &until); err != nil {
			return nil, fmt.Errorf("scan partition coverage: %w", err)
		}
		if until != nil {
			c.CoveredUntil = *until
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read partition coverage: %w", err)
	}
	return out, nil
}

// EnsureMonthlyPartitions provisions the current month plus monthsAhead for
// one partitioned table.
//
// The table name is not interpolated: create_monthly_partitions takes it as a
// value and quotes it as an identifier, and refuses anything that is not a
// partitioned table in public.
func (s *MaintenanceStore) EnsureMonthlyPartitions(ctx context.Context, table string, monthsAhead int) error {
	if _, err := s.pool.Exec(ctx,
		`SELECT create_monthly_partitions($1, $2)`, table, monthsAhead); err != nil {
		return fmt.Errorf("provision partitions for %s: %w", table, err)
	}
	return nil
}
