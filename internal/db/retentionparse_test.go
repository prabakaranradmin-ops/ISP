package db

import (
	"strings"
	"testing"
)

// Parsing PARTITION_RETENTION_MONTHS.
//
// A policy that silently drops the half it could not parse is the dangerous
// kind: an operator who writes
//
//	flow_records=24,cgnat_alocations=24      (note the typo)
//
// would believe CGNAT allocations are kept for two years when in fact they
// are kept forever — or, with the typo in the number instead, deleted after a
// month. Both are wrong in a way nobody discovers until somebody asks for the
// records. So every malformed input is an error, and the service refuses to
// start rather than run a policy it only half understood.

func TestParseRetentionPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want map[string]int
	}{
		"empty is keep everything":      {"", nil},
		"blank is keep everything":      {"   ", nil},
		"one table":                     {"flow_records=24", map[string]int{"flow_records": 24}},
		"several":                       {"flow_records=24,cgnat_allocations=12", map[string]int{"flow_records": 24, "cgnat_allocations": 12}},
		"spaces are tolerated":          {" flow_records = 24 , cgnat_allocations = 12 ", map[string]int{"flow_records": 24, "cgnat_allocations": 12}},
		"a trailing comma is tolerated": {"flow_records=24,", map[string]int{"flow_records": 24}},
		"one month is the floor":        {"flow_records=1", map[string]int{"flow_records": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRetentionPolicy(tc.raw)
			if err != nil {
				t.Fatalf("%q was refused: %v", tc.raw, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%q -> %v, want %v", tc.raw, got, tc.want)
			}
			for table, months := range tc.want {
				if got[table] != months {
					t.Errorf("%s = %d, want %d", table, got[table], months)
				}
			}
		})
	}
}

// Every one of these would, if skipped rather than refused, leave an operator
// believing something false about how long records are kept.
func TestParseRetentionPolicyRefusesAnythingAmbiguous(t *testing.T) {
	for name, raw := range map[string]string{
		"no equals":          "flow_records",
		"no months":          "flow_records=",
		"no table":           "=24",
		"non-numeric":        "flow_records=two years",
		"a date":             "flow_records=2026-01",
		"zero":               "flow_records=0",
		"negative":           "flow_records=-12",
		"implausible":        "flow_records=20260101",
		"one good one bad":   "flow_records=24,cgnat_allocations=forever",
		"duplicate table":    "flow_records=24,flow_records=12",
		"equals in the name": "flow=records=24",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRetentionPolicy(raw)
			if err == nil {
				t.Errorf("%q was accepted as %v", raw, got)
			}
			if got != nil {
				t.Errorf("%q returned a policy alongside its error: %v", raw, got)
			}
		})
	}
}

// "One good one bad" deserves its own assertion: the failure mode this guards
// against is the good half being applied while the bad half is dropped.
func TestOneBadEntryRefusesTheWholePolicy(t *testing.T) {
	got, err := ParseRetentionPolicy("flow_records=24,cgnat_allocations=forever")
	if err == nil {
		t.Fatal("a policy with one unparseable entry was accepted")
	}
	if got != nil {
		t.Fatalf("the parseable half was returned anyway: %v — an operator would see "+
			"flow_records expiring on schedule and conclude the whole policy took effect",
			got)
	}
	// And the error names the entry that was wrong, not just that something
	// was: the operator has to know which line to fix.
	if !strings.Contains(err.Error(), "cgnat_allocations") {
		t.Errorf("the error does not name the offending table: %v", err)
	}
}
