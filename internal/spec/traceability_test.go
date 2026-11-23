// Package spec holds checks over the specification documents themselves.
//
// Every requirement in the SRS must either be cited by id somewhere in the
// implementation, or carry a status marker saying it deliberately is not.
// Nothing else is a legitimate third state, and the third state is what this
// test exists to prevent.
//
// It exists because the ledger drifted badly and silently. HANDOFF claimed
// "98 implemented, 1 deferred, 0 outstanding" while the SRS had held 143
// requirements for at least forty commits. Nobody noticed, because nothing
// compared the two — the count was maintained by hand in a document that
// nothing read. Five requirements were parked for external reasons and read
// as pending engineering for months; seven were implemented and read as
// missing because no code mentioned their id.
//
// A number maintained by hand in prose is a number that will be wrong. This
// derives it instead.
package spec_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is two levels up from internal/spec.
const repoRoot = "../.."

var (
	// A requirement row in the SRS: "| FR-AAA-001 | description | ... |".
	// The leading pipe keeps prose mentions out, and \b before FR is what
	// stops the bare prefix matching inside an NFR id such as NFR-AVAIL-001
	// — the trap that made a naive count read 108 instead of 99 back when it
	// was 99. (Deliberately not spelling the inner id out here: the reverse
	// check below reads comments too, and flagged this very sentence.)
	srsRow = regexp.MustCompile(`^\|\s*(FR-([A-Z]+)-\d{3})\s*\|\s*(.+?)\s*\|`)
	// A citation in code, including range notation like FR-NAS-001..004,
	// which otherwise matches only its first id.
	citation = regexp.MustCompile(`\b(FR-([A-Z]+)-(\d{3}))(?:\s*\.\.\s*(\d{3}))?`)
)

// Status markers the SRS uses on a requirement row.
const (
	markerDone      = "✅"  // implemented
	markerParked    = "⏸"  // out of scope for v1, blocked on something external
	markerNotAppl   = "⛔"  // closed by decision, will not be built
	markerStrikeout = "~~" // deferred by decision, e.g. FR-AAA-005 CHAP
)

type requirement struct {
	id, area, text string
}

func readSRS(t *testing.T) []requirement {
	t.Helper()
	path := filepath.Join(repoRoot, "specification_docs_v2", "02_SRS_System_Requirements.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the SRS: %v", err)
	}
	var out []requirement
	seen := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		m := srsRow.FindStringSubmatch(line)
		if m == nil || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, requirement{id: m[1], area: m[2], text: m[3]})
	}
	if len(out) < 100 {
		t.Fatalf("found only %d requirement rows in the SRS; this pattern no longer "+
			"matches how they are written and the test is asserting nothing", len(out))
	}
	return out
}

// citedInCode collects every FR id mentioned anywhere in the implementation.
//
// Generated output is excluded deliberately. e2e/screenshots/index.html
// renders a coverage table listing all 143 ids, so a sweep that walked it
// counted the report as evidence of its own completeness — which is exactly
// the circular measurement this test is meant to replace.
//
// internal/spec is excluded for the same reason, one level in: these files
// name requirement ids while explaining themselves, and a comment in the
// measuring code is not an implementation of the thing being measured. The
// range notation made it concrete — FR-NAS-001..004 appears in a comment
// here, so without this exclusion four requirements would have counted
// themselves cited by the test that checks whether they are. (They are
// genuinely cited, in cmd/api and internal/api; the hole was real and
// unoccupied.)
func citedInCode(t *testing.T) map[string]bool {
	t.Helper()
	skipDir := map[string]bool{
		".git": true, "node_modules": true, "specification_docs_v2": true,
		"docs": true, "screenshots": true, "playwright-report": true,
		"test-results": true,
	}
	selfDir := filepath.Clean(filepath.Join(repoRoot, "internal", "spec"))
	wanted := map[string]bool{
		".go": true, ".sql": true, ".sh": true, ".ts": true, ".html": true, ".ps1": true,
	}

	cited := map[string]bool{}
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable path is not this test's business
		}
		if info.IsDir() {
			if skipDir[info.Name()] || filepath.Clean(path) == selfDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !wanted[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range citation.FindAllStringSubmatch(string(body), -1) {
			cited[m[1]] = true
			if m[4] != "" {
				lo, hi := m[3], m[4]
				for n := atoi(lo); n <= atoi(hi); n++ {
					cited[sprintfFR(m[2], n)] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	return cited
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func sprintfFR(area string, n int) string {
	d := []byte{byte('0' + n/100%10), byte('0' + n/10%10), byte('0' + n%10)}
	return "FR-" + area + "-" + string(d)
}

func marked(text string) (string, bool) {
	switch {
	case strings.Contains(text, markerParked):
		return "out of scope for v1", true
	case strings.Contains(text, markerNotAppl):
		return "not applicable", true
	case strings.Contains(text, markerStrikeout):
		return "deferred by decision", true
	}
	return "", false
}

// TestEveryRequirementIsCitedOrExplicitlyNot is the gate.
func TestEveryRequirementIsCitedOrExplicitlyNot(t *testing.T) {
	reqs := readSRS(t)
	cited := citedInCode(t)

	var orphans []string
	counts := map[string]int{}
	for _, r := range reqs {
		if cited[r.id] {
			counts["cited in code"]++
			continue
		}
		if why, ok := marked(r.text); ok {
			counts[why]++
			continue
		}
		orphans = append(orphans, r.id+"  "+trim(r.text, 70))
	}

	sort.Strings(orphans)
	if len(orphans) > 0 {
		t.Errorf("%d requirement(s) are neither cited in the code nor marked as deliberately "+
			"unbuilt:\n  %s\n\nEither cite the id where it is implemented, or give the SRS row a "+
			"marker: %s implemented, %s out of scope for v1, %s not applicable, or ~~strikethrough~~ "+
			"for deferred by decision. A requirement in neither state is one nobody can account for, "+
			"which is how this project came to believe it had 99 requirements when it had 143.",
			len(orphans), strings.Join(orphans, "\n  "),
			markerDone, markerParked, markerNotAppl)
	}

	t.Logf("%d requirements: %d cited in code, %d out of scope for v1, %d not applicable, %d deferred",
		len(reqs), counts["cited in code"], counts["out of scope for v1"],
		counts["not applicable"], counts["deferred by decision"])
}

// TestNoCitationNamesARequirementThatDoesNotExist catches the other
// direction: a typo in a citation, or an id left behind after a requirement
// was renumbered. Both make the sweep above pass while pointing at nothing.
func TestNoCitationNamesARequirementThatDoesNotExist(t *testing.T) {
	known := map[string]bool{}
	for _, r := range readSRS(t) {
		known[r.id] = true
	}

	var unknown []string
	for id := range citedInCode(t) {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("the code cites %d requirement id(s) the SRS does not define: %s\n"+
			"A citation that names nothing is worse than none: it reads as traceability "+
			"and traces to nothing.", len(unknown), strings.Join(unknown, ", "))
	}
}

func trim(s string, n int) string {
	s = strings.TrimSpace(strings.NewReplacer("*", "", "`", "", "~", "").Replace(s))
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
