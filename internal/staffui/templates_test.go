package staffui

import (
	"io/fs"
	"strings"
	"testing"
)

// Every screen's template has to be registered in pageNames, or render()
// logs "unknown template" and the handler answers 500 — at runtime, on the
// one page nobody tested by eye.
//
// This is written from a real miss: the Products screen (FR-BIL-022)
// shipped with its handler, its routes, its role gate and its template
// file all correct, and the one line adding "products" to pageNames
// missing. Every Go test still compiled and passed. The screen 500'd.
//
// Comparing the registry against the embedded directory, in both
// directions, is the check that could not have been fooled: a template
// file nobody registered fails here, and a registered name with no file
// already panics at init (template.Must in pages).

// templateFiles lists the page templates on disk, excluding layout.html,
// which is parsed into every page rather than being a page itself.
func templateFiles(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(assets, "templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".html") {
			continue
		}
		base := strings.TrimSuffix(name, ".html")
		if base == "layout" {
			continue
		}
		out = append(out, base)
	}
	return out
}

func TestEveryPageTemplateIsRegistered(t *testing.T) {
	registered := make(map[string]bool, len(pageNames))
	for _, n := range pageNames {
		if registered[n] {
			t.Errorf("pageNames lists %q twice", n)
		}
		registered[n] = true
	}

	for _, file := range templateFiles(t) {
		if !registered[file] {
			t.Errorf("templates/%s.html exists but is not in pageNames, so rendering it answers 500", file)
		}
	}
}

func TestEveryRegisteredPageHasATemplate(t *testing.T) {
	onDisk := make(map[string]bool)
	for _, f := range templateFiles(t) {
		onDisk[f] = true
	}
	for _, n := range pageNames {
		if !onDisk[n] {
			t.Errorf("pageNames lists %q but templates/%s.html does not exist", n, n)
		}
		// pages is built with template.Must at init, so a missing file
		// would already have panicked — asserting it here names the
		// offender instead of failing the whole package on a panic.
		if _, ok := pages[n]; !ok {
			t.Errorf("%q is in pageNames but was not parsed into pages", n)
		}
	}
}
