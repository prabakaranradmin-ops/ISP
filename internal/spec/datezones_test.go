package spec_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A date a person types is a calendar date, and must be parsed in their own
// zone.
//
// Parsed as UTC midnight it becomes 05:30 that morning in IST, and the
// consequences are quiet in both directions:
//
//   - Against a timestamptz range, a single-day filter skips the first five
//     and a half hours of the day and picks up five and a half hours of the
//     next. That was every date search on the audit trail, the authentication
//     log, the payment-gateway log, the income statement and the balance
//     sheet.
//   - Against a DATE column, any deployment west of UTC gets the previous
//     calendar date, because 00:00 UTC is yesterday there. A scheduled plan
//     change for today was refused as already passed.
//
// None of it shows during a working day in India, which is why it survived:
// it was found when three database tests failed on a run that happened to
// start at 04:00 IST, the one window where UTC and IST disagree about the
// date. Thirteen sites had it.
//
// This is a source-level check because the alternative is a test whose result
// depends on the hour it runs at, which is the thing that let this through.

// A plain calendar-date layout, as opposed to a timestamp layout that carries
// its own offset.
const dateLayout = `"2006-01-02"`

var (
	// time.Parse has no zone argument and is therefore UTC.
	bareDateParse = regexp.MustCompile(`time\.Parse\(` + dateLayout)
	// An explicit zone that is the wrong one.
	utcDateParse = regexp.MustCompile(`ParseInLocation\(` + dateLayout + `, [^)]*time\.UTC\)`)
)

func TestNoHumanTypedDateIsParsedAsUTC(t *testing.T) {
	type finding struct{ path, why string }
	var found []finding

	skipDir := map[string]bool{
		".git": true, "node_modules": true, "e2e": true, "docs": true,
		"specification_docs_v2": true, "installer": true,
		"playwright-report": true, "test-results": true,
	}

	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDir[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Test files are exempt: a test may legitimately build a date in a
		// chosen zone to pin behaviour across zones, which is exactly what
		// internal/billing's own date tests do.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		if bareDateParse.Match(body) {
			found = append(found, finding{rel, "time.Parse with a date layout is UTC"})
		}
		if utcDateParse.Match(body) {
			found = append(found, finding{rel, "ParseInLocation with time.UTC"})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}

	sort.Slice(found, func(i, j int) bool { return found[i].path < found[j].path })
	for _, f := range found {
		t.Errorf("%s: %s.\n"+
			"A date somebody types is a calendar date in their own zone. As UTC "+
			"midnight it is 05:30 that morning in IST, so a one-day filter loses the "+
			"first five and a half hours and a DATE column west of UTC gets "+
			"yesterday. Use time.ParseInLocation with time.Local.", f.path, f.why)
	}
}

// The counterpart: the comparison has to happen in one zone. A caller that
// parses in the operator's zone and then compares against a UTC "today" has
// moved the bug rather than fixed it, so the places that take a "today" are
// checked to be passing a local one.
func TestDateComparisonsUseOneZone(t *testing.T) {
	// time.Now() is local; time.Now().UTC() is not. A Normalise-style call
	// given the latter compares a local date against a UTC one.
	utcNow := regexp.MustCompile(`Normalise\(time\.Now\(\)\.UTC\(\)\)`)

	for _, rel := range []string{
		"internal/db/plan_changes.go",
		"internal/api/plan_changes.go",
		"internal/staffui/plan_changes.go",
	} {
		body, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			// Not fatal: the file may be renamed. Say so rather than pass
			// silently, since a check over a missing file checks nothing.
			t.Logf("%s is referenced here but missing: %v", rel, err)
			continue
		}
		if utcNow.Match(body) {
			t.Errorf("%s normalises against time.Now().UTC() while the date it compares "+
				"is parsed in the operator's zone — the two sides disagree about the "+
				"calendar date for five and a half hours a day in IST", rel)
		}
	}
}
