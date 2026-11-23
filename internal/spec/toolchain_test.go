package spec_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Go version is declared in several places and they have disagreed three
// times.
//
// go.mod is the floor. Everything that builds this project has to be at or
// above it, and each place that names a version is a place that can fall
// behind silently:
//
//   - The CI workflow pinned 1.20 while go.mod said 1.22, so `go build`
//     refused in CI.
//   - The Docker images pinned 1.22 while go.mod said 1.23, which made the
//     demo stack unbuildable from 2026-11-16 onward. The images that
//     auto-start with Docker Desktop simply kept serving months-old code,
//     which is worse than failing.
//   - Adding minio-go raised go.mod from 1.23 to 1.26 with nothing else
//     touched, because `go get` edits the directive when a dependency needs
//     a newer toolchain. Nobody writes that change; it arrives.
//
// None of those produced a useful error at the point of the mistake. This
// test is the error.
func TestEverythingBuildsAgainstTheGoVersionInGoMod(t *testing.T) {
	want := goModVersion(t)

	for _, f := range []struct{ path, why string }{
		{"Dockerfile", "the radiusd image"},
		{"Dockerfile.api", "the API image"},
		{"scripts/demo_up.sh", "the demo stack's throwaway build containers"},
	} {
		path := filepath.Join(repoRoot, f.path)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s (%s) is referenced here but missing: %v", f.path, f.why, err)
			continue
		}
		found := goImageVersions(string(body))
		if len(found) == 0 {
			// Not an error on its own — a file may stop naming a Go image —
			// but say so, because a silently unchecked file is how this
			// drifted before.
			t.Logf("%s names no golang image; nothing to check there", f.path)
			continue
		}
		for _, got := range found {
			if !compatible(got, want) {
				t.Errorf("%s (%s) builds on golang:%s while go.mod requires %s.\n"+
					"A build at a lower version fails outright; one at a higher patch is fine. "+
					"Raise the image, or lower go.mod if the dependency that raised it can go.",
					f.path, f.why, got, want)
			}
		}
	}
}

// goModVersion reads the major.minor from go.mod's `go` directive.
func goModVersion(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := regexp.MustCompile(`(?m)^go\s+(\d+)\.(\d+)`).FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("go.mod has no `go` directive this test can read")
	}
	return m[1] + "." + m[2]
}

var golangImage = regexp.MustCompile(`golang:(\d+)\.(\d+)`)

// goImageVersions pulls every `golang:X.Y` out of a build file.
func goImageVersions(body string) []string {
	seen := map[string]bool{}
	for _, m := range golangImage.FindAllStringSubmatch(body, -1) {
		seen[m[1]+"."+m[2]] = true
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// compatible reports whether an image at got can build code requiring want.
func compatible(got, want string) bool {
	gotMaj, gotMin := split(got)
	wantMaj, wantMin := split(want)
	if gotMaj != wantMaj {
		return gotMaj > wantMaj
	}
	return gotMin >= wantMin
}

func split(v string) (int, int) {
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	return atoi(parts[0]), atoi(parts[1])
}

// The CI workflow must not pin a literal version either — it reads go.mod,
// which is the only arrangement that cannot drift.
func TestCIReadsItsGoVersionFromGoMod(t *testing.T) {
	path := filepath.Join(repoRoot, ".github", "workflows", "go.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the workflow: %v", err)
	}
	text := string(body)

	if !strings.Contains(text, "go-version-file: go.mod") {
		t.Error("the workflow should set go-version-file: go.mod so the toolchain cannot " +
			"fall behind the module")
	}
	// A literal pin anywhere is the thing that drifted twice.
	if m := regexp.MustCompile(`go-version(?:-input)?:\s*'?\d+\.\d+`).FindString(text); m != "" {
		t.Errorf("the workflow pins a literal Go version (%q). Use go-version-file: go.mod — "+
			"this pin has fallen behind go.mod twice, and the second time would have failed "+
			"every job on the next push.", strings.TrimSpace(m))
	}
}
