package db

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseRetentionPolicy reads PARTITION_RETENTION_MONTHS.
//
// The form is "table=months", comma-separated:
//
//	flow_records=24,cgnat_allocations=24
//
// An empty string is a valid policy meaning "keep everything", because that
// is the configured state of every deployment today and the only safe
// default — see partitionretention.go.
//
// Every other malformed input is an error rather than a skipped entry. A
// retention policy that silently ignores the half it could not parse is how
// an operator ends up believing CGNAT records are being kept for two years
// when the typo in that entry means they are being kept forever, or the typo
// in the other one means they are being deleted after one month. The service
// refuses to start instead.
func ParseRetentionPolicy(raw string) (RetentionPolicy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	policy := RetentionPolicy{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		table, months, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("partition retention: %q is not table=months", entry)
		}
		table = strings.TrimSpace(table)
		if table == "" {
			return nil, fmt.Errorf("partition retention: %q names no table", entry)
		}
		if _, dup := policy[table]; dup {
			// Two answers for one table is not something to resolve by
			// precedence: one of them is wrong and nobody knows which.
			return nil, fmt.Errorf("partition retention: %s is given twice", table)
		}
		n, err := strconv.Atoi(strings.TrimSpace(months))
		if err != nil {
			return nil, fmt.Errorf("partition retention: %s has a non-numeric retention %q",
				table, strings.TrimSpace(months))
		}
		if n < 1 {
			// Zero would expire the partition being written to; negative is
			// a typo. Neither is a way to say "keep everything" — leaving
			// the table out is.
			return nil, fmt.Errorf(
				"partition retention: %s=%d — a retention must be at least 1 month; "+
					"leave the table out to keep everything", table, n)
		}
		if n > 600 {
			// Fifty years. Past this it is a typo, probably a date.
			return nil, fmt.Errorf("partition retention: %s=%d is implausible", table, n)
		}
		policy[table] = n
	}
	if len(policy) == 0 {
		return nil, nil
	}
	return policy, nil
}
