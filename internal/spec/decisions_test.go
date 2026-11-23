package spec_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// docs/architecture_decisions.md is a map, and a map whose landmarks have been
// demolished is worse than no map.
//
// The index was written because three decisions were re-raised months after
// they had been settled — once with a reproduction command for a Redis tier
// that had already been deleted. The failure mode it replaces is prose
// nobody checks, so leaving it as prose nobody checks would reproduce it one
// level up. Every path, migration, requirement and commit it names is
// resolved here against the thing it names.
//
// Two errors were found the first time this ran against the freshly written
// index: ADR-001 cited migration 019 for a policy that is in 014, and
// FR-OBS-006 for what is FR-OBS-003.

const adrPath = "docs/architecture_decisions.md"

var (
	// Anything inside backticks. Which of those are claims about the
	// repository is decided below, not here.
	backticked = regexp.MustCompile("`([^`\n]+)`")
	// "migration 062", "migrations 014 and 019".
	migrationRef = regexp.MustCompile(`migrations?\s+(\d{3})(?:\s+and\s+(\d{3}))?`)
	// A requirement id. NFR included: the index cites NFR-PERF-001.
	reqRef = regexp.MustCompile(`\b((?:N?FR)-[A-Z]+-\d{3})\b`)
	// An abbreviated commit sha, as the index writes them.
	shaRef = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	// "## ADR-007 — ..."
	adrHeading = regexp.MustCompile(`(?m)^## ADR-(\d{3}) — (.+)$`)
	// Extensions that make a bare token a claim about a file.
	pathish = map[string]bool{
		".md": true, ".go": true, ".sql": true, ".yml": true, ".yaml": true,
		".sh": true, ".ts": true, ".mod": true, ".json": true, ".ps1": true,
	}
)

func readADR(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot, adrPath))
	if err != nil {
		t.Fatalf("read %s: %v", adrPath, err)
	}
	return string(body)
}

// Every path the index names resolves to something on disk.
//
// A prefix match is accepted, because the index abbreviates spec documents to
// their numbered stem — `specification_docs_v2/08_IDD` for
// 08_IDD_Infrastructure_Design.md. A rename still fails, which is the point.
func TestADRPathsResolve(t *testing.T) {
	for _, m := range backticked.FindAllStringSubmatch(readADR(t), -1) {
		tok := m[1]
		if !looksLikePath(tok) {
			continue
		}
		full := filepath.Join(repoRoot, tok)
		if _, err := os.Stat(full); err == nil {
			continue
		}
		if hits, _ := filepath.Glob(full + "*"); len(hits) > 0 {
			continue
		}
		t.Errorf("the decision index names %q and nothing by that name exists. "+
			"If it was renamed or deleted, the decision that mentions it needs "+
			"revisiting, not the reference patching.", tok)
	}
}

// looksLikePath reports whether a backticked token is a claim about a file.
//
// Identifiers (`archive.Store`, `staffui.sections`), table names, environment
// variables and fragments of prose are not, and must not be: this is the
// difference between a check and a nuisance.
func looksLikePath(tok string) bool {
	if strings.ContainsAny(tok, " \t():‖=") {
		return false
	}
	if strings.Contains(tok, "/") {
		return true
	}
	return pathish[strings.ToLower(filepath.Ext(tok))]
}

// Every "migration NNN" has a migration.
func TestADRMigrationsExist(t *testing.T) {
	for _, m := range migrationRef.FindAllStringSubmatch(readADR(t), -1) {
		for _, num := range []string{m[1], m[2]} {
			if num == "" {
				continue
			}
			hits, _ := filepath.Glob(filepath.Join(repoRoot, "migrations", num+"_*.sql"))
			if len(hits) == 0 {
				t.Errorf("the decision index cites migration %s and migrations/%s_*.sql "+
					"does not exist", num, num)
			}
		}
	}
}

// Every requirement id the index cites is a requirement in the SRS.
//
// The index is where a reader goes to find out why something is the way it
// is; sending them to a requirement id that no longer exists is worse than
// sending them nowhere, because they will look for it.
func TestADRRequirementsExist(t *testing.T) {
	known := map[string]bool{}
	for _, r := range readSRS(t) {
		known[r.id] = true
	}
	// readSRS matches FR rows only. NFR rows are written the same way with
	// the NFR prefix, so collect those directly.
	srs, err := os.ReadFile(filepath.Join(repoRoot,
		"specification_docs_v2", "02_SRS_System_Requirements.md"))
	if err != nil {
		t.Fatalf("read the SRS: %v", err)
	}
	for _, m := range regexp.MustCompile(`(?m)^\|\s*(NFR-[A-Z]+-\d{3})\s*\|`).
		FindAllStringSubmatch(string(srs), -1) {
		known[m[1]] = true
	}

	for _, m := range reqRef.FindAllStringSubmatch(readADR(t), -1) {
		if !known[m[1]] {
			t.Errorf("the decision index cites %s, which is not a requirement in the SRS", m[1])
		}
	}
}

// Every commit the index cites is a commit in this repository.
//
// Skipped on a shallow clone, where the object genuinely is not present and
// failing would only teach people to ignore the result.
func TestADRCommitsExist(t *testing.T) {
	if out, err := exec.Command("git", "-C", repoRoot,
		"rev-parse", "--is-shallow-repository").Output(); err != nil {
		t.Skipf("git is unavailable here: %v", err)
	} else if strings.TrimSpace(string(out)) == "true" {
		t.Skip("shallow clone: the cited commits are not in this copy of the history")
	}

	for _, m := range backticked.FindAllStringSubmatch(readADR(t), -1) {
		tok := m[1]
		if !shaRef.MatchString(tok) || len(tok) > 12 {
			continue
		}
		if err := exec.Command("git", "-C", repoRoot,
			"cat-file", "-e", tok+"^{commit}").Run(); err != nil {
			t.Errorf("the decision index cites commit %s, which is not a commit here. "+
				"A rebase or a squash will do this, and it leaves the reasoning "+
				"unreachable.", tok)
		}
	}
}

// Numbering is unique and contiguous, and each entry says why.
//
// A gap means an entry was deleted — and a superseded decision is still worth
// reading, so it should be marked superseded rather than removed. A missing
// "Why" means the entry records what was done without recording what it was
// for, which is the half that stops it being re-argued.
func TestADRIndexIsWellFormed(t *testing.T) {
	body := readADR(t)
	headings := adrHeading.FindAllStringSubmatchIndex(body, -1)
	if len(headings) == 0 {
		t.Fatal("no ADR headings found; this test no longer matches how they are written")
	}

	var nums []int
	seen := map[int]string{}
	for i, h := range headings {
		num, _ := strconv.Atoi(body[h[2]:h[3]])
		title := body[h[4]:h[5]]
		if prev, dup := seen[num]; dup {
			t.Errorf("ADR-%03d is used twice: %q and %q", num, prev, title)
		}
		seen[num] = title
		nums = append(nums, num)

		end := len(body)
		if i+1 < len(headings) {
			end = headings[i+1][0]
		}
		section := body[h[1]:end]
		if !strings.Contains(section, "*Why:*") && !strings.Contains(section, "*Detail:*") {
			t.Errorf("ADR-%03d (%s) records a decision with neither a Why nor a Detail "+
				"pointer. An entry that says only what was chosen will be re-argued.",
				num, title)
		}
		if !regexp.MustCompile(`(?i)\b(accepted|superseded|rejected|proposed)\b`).
			MatchString(section) {
			t.Errorf("ADR-%03d (%s) has no status", num, title)
		}
	}

	sort.Ints(nums)
	for i, n := range nums {
		if want := i + 1; n != want {
			t.Errorf("ADR numbering jumps: expected ADR-%03d, found ADR-%03d. "+
				"Mark a withdrawn decision superseded instead of deleting it.", want, n)
			break
		}
	}
}
