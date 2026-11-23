package spec_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// 07_API_OpenAPI_Contract.md is what a partner integrates against, and
// nothing has ever compared it to what the server answers.
//
// The document embeds a real OpenAPI 3.0 document in a fenced block. That
// block is the published contract: an endpoint listed there and not served is
// a partner integration that fails in their environment, after they have
// built against it. It is the one kind of documentation error that costs
// somebody else money.
//
// This check registers the patterns the implementation actually uses on a
// real http.ServeMux and asks it to route a concrete URL for every operation
// the contract declares. Routing it through the real mux rather than
// comparing strings is deliberate: method rules, wildcard precedence and the
// trailing-slash subtree rule are ServeMux's to decide, and a string
// comparison would get them subtly wrong in both directions.
//
// What it cannot see: whether the handler behind a matched pattern does what
// the contract says, and whether registration still happens through literal
// strings in the source. The second is guarded by a floor on the number of
// patterns found — a refactor that moves registration somewhere this cannot
// read fails here rather than quietly checking nothing.

// A registered pattern: `mux.Handle("GET /api/v1/subscribers/{id}", ...)`.
var routePattern = regexp.MustCompile(`"(GET|POST|PUT|PATCH|DELETE|HEAD) (/[^"]*)"`)

// Below this, assume the extraction broke rather than that the API shrank.
const minRoutesFound = 200

type operation struct {
	method, path string
}

// contractOperations reads the OpenAPI document embedded in the contract.
func contractOperations(t *testing.T) []operation {
	t.Helper()
	path := filepath.Join(repoRoot, "specification_docs_v2", "07_API_OpenAPI_Contract.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the API contract: %v", err)
	}

	block := fencedYAML(t, string(body))
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal([]byte(block), &doc); err != nil {
		t.Fatalf("the contract's embedded OpenAPI document does not parse: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("the embedded OpenAPI document declares no paths; this test is asserting nothing")
	}

	methods := map[string]bool{
		"get": true, "post": true, "put": true, "patch": true, "delete": true, "head": true,
	}
	var out []operation
	for p, ops := range doc.Paths {
		for m := range ops {
			if methods[strings.ToLower(m)] {
				out = append(out, operation{strings.ToUpper(m), p})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].method < out[j].method
	})
	return out
}

// fencedYAML returns the first ```yaml block in the document.
func fencedYAML(t *testing.T, body string) string {
	t.Helper()
	const open = "```yaml"
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatal("the API contract no longer embeds a ```yaml block")
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "\n```")
	if j < 0 {
		t.Fatal("the API contract's yaml block is never closed")
	}
	return rest[:j]
}

// servedMux registers every pattern the implementation registers, on a mux
// whose handlers only record that they were reached.
func servedMux(t *testing.T) (*http.ServeMux, map[operation]string) {
	t.Helper()
	skipDir := map[string]bool{
		".git": true, "node_modules": true, "e2e": true, "docs": true,
		"specification_docs_v2": true, "playwright-report": true, "test-results": true,
	}

	mux := http.NewServeMux()
	where := map[operation]string{}
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
		// Test files are excluded: a pattern that only a test registers is
		// not a served route, and counting one would be the circular
		// measurement this package exists to avoid.
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Only the API server's own mux. Every other cmd/ is a separate
		// binary with a separate mux, and mixing them produced a false pass
		// on seven endpoints: cmd/mockgateway registers "GET /", a root
		// catch-all that answers any unmatched GET, so every documented-but-
		// missing GET appeared to be served. A test that pools the routes of
		// unrelated programs measures nothing.
		if rel, err := filepath.Rel(repoRoot, path); err == nil {
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if parts[0] == "cmd" && len(parts) > 1 && parts[1] != "api" {
				return nil
			}
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		rel = filepath.ToSlash(rel)
		for _, m := range routePattern.FindAllStringSubmatch(string(body), -1) {
			op := operation{m[1], m[2]}
			if _, dup := where[op]; dup {
				// Two files registering the same pattern is a panic at
				// startup, not something to assert here — but ServeMux would
				// panic in this test too, so take the first and say so.
				continue
			}
			where[op] = rel
			mux.Handle(m[1]+" "+m[2], http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the repository: %v", err)
	}
	if len(where) < minRoutesFound {
		t.Fatalf("found only %d registered route patterns, expected at least %d. "+
			"Either routes are no longer registered as literal \"METHOD /path\" strings, "+
			"or this walk is reading the wrong tree — either way this test is checking nothing.",
			len(where), minRoutesFound)
	}
	return mux, where
}

// Every operation the contract publishes is routed by the server.
func TestEveryPublishedEndpointIsServed(t *testing.T) {
	mux, _ := servedMux(t)

	for _, op := range contractOperations(t) {
		t.Run(op.method+" "+op.path, func(t *testing.T) {
			req := httptest.NewRequest(op.method, concreteURL(op.path), nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			// Which registered pattern answered. Asking the mux is the only
			// way to tell a route that exists from one that was absorbed by
			// a wildcard or a subtree — and "something answered" is not the
			// claim the contract makes.
			_, matched := mux.Handler(req)

			switch {
			case rec.Code == http.StatusTeapot && sameShape(matched, op):
				return
			case rec.Code == http.StatusTeapot:
				t.Errorf("the contract publishes %s %s, but what answers that URL is %q. "+
					"A client gets a response, from the wrong handler — which is worse "+
					"than a 404, because nothing reports it.", op.method, op.path, matched)
			case rec.Code == http.StatusMethodNotAllowed:
				t.Errorf("the contract publishes %s %s, and the path is served but not for "+
					"that method. A client following the contract gets 405.", op.method, op.path)
			default:
				t.Errorf("the contract publishes %s %s and nothing serves it (%d). "+
					"Either the route is missing or the contract is describing an API we "+
					"do not have — a partner building against this document fails in "+
					"their environment, not ours.", op.method, op.path, rec.Code)
			}
		})
	}
}

// sameShape reports whether the pattern that answered is the operation the
// contract declared, rather than a wildcard or subtree that happened to
// cover the same URL.
func sameShape(matched string, op operation) bool {
	// ServeMux reports "METHOD /path" for a method-specific pattern.
	if m, p, ok := strings.Cut(matched, " "); ok {
		return m == op.method && canonicalPath(p) == canonicalPath(op.path)
	}
	return false
}

// concreteURL turns /api/v1/subscribers/{id}/health into a routable URL.
//
// The parameter's name is not part of the wire contract — only its position
// is — so the contract calling it {subscriber_id} where the code calls it
// {id} is not a difference worth failing over. Substituting a value tests the
// shape, which is the part a client depends on.
func concreteURL(p string) string {
	return regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(p, "1")
}

// The contract is a published subset, so routes absent from it are expected
// and not a failure. But the gap is worth stating: an undocumented route is
// one a partner cannot use and an attacker can still reach, and 07_API is
// the document we hand to a penetration tester as the surface description.
func TestUndocumentedRoutesAreReported(t *testing.T) {
	_, where := servedMux(t)
	declared := map[operation]bool{}
	for _, op := range contractOperations(t) {
		declared[operation{op.method, canonicalPath(op.path)}] = true
	}

	byPrefix := map[string]int{}
	for op := range where {
		if declared[operation{op.method, canonicalPath(op.path)}] {
			continue
		}
		byPrefix[surface(op.path)]++
	}

	keys := make([]string, 0, len(byPrefix))
	for k := range byPrefix {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	total := 0
	for _, k := range keys {
		total += byPrefix[k]
		t.Logf("%-16s %3d routes not in the published contract", k, byPrefix[k])
	}
	t.Logf("%d of %d served routes are undocumented", total, len(where))
}

// canonicalPath erases wildcard names so the two sides compare by shape.
func canonicalPath(p string) string {
	return regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(p, "{}")
}

// surface groups a route by the audience its prefix implies.
func surface(p string) string {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 2)
	switch parts[0] {
	case "api":
		return "/api/v1"
	case "":
		return "/"
	default:
		return "/" + parts[0]
	}
}
