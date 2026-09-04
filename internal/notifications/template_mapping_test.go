package notifications

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The template ids the code sends have to be the ones the spec defines, and
// the names have to be the ones migration 050 seeds. Nothing enforced either
// before: notification_templates was empty, so the foreign key that binds
// them had nothing to check against, and the code had drifted far enough that
// a payment receipt sent the id reserved for hard suspension.
//
// These read the migration rather than restating it, so the two cannot be
// edited apart.

var seedLine = regexp.MustCompile(`\('(TMPL-\d{3})',\s*'[a-z]+',\s*'([a-z0-9_]+)'`)

func seededTemplates(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile("../../migrations/050_seed_notification_templates.sql")
	if err != nil {
		t.Fatalf("read migration 050: %v", err)
	}
	// Only the Up half: the Down half names the same ids in a DELETE.
	up := strings.SplitN(string(body), "-- +goose Down", 2)[0]
	got := map[string]string{}
	for _, m := range seedLine.FindAllStringSubmatch(up, -1) {
		got[m[1]] = m[2]
	}
	if len(got) == 0 {
		t.Fatal("parsed no templates out of migration 050 — the seed format changed")
	}
	return got
}

// Every id the WhatsApp client can address must exist in the seed, or its
// notification_log row fails the foreign key at insert time — in production,
// on a message someone was waiting for.
func TestTemplateNamesMatchTheSeededTemplates(t *testing.T) {
	seeded := seededTemplates(t)

	for id, name := range templateNames {
		seededName, ok := seeded[id]
		if !ok {
			t.Errorf("%s is addressable in code but not seeded by migration 050 — "+
				"logging it would violate notification_log's foreign key", id)
			continue
		}
		if seededName != name {
			t.Errorf("%s: code sends Meta template %q, migration 050 seeds %q", id, name, seededName)
		}
	}
	for id := range seeded {
		if _, ok := templateNames[id]; !ok {
			t.Errorf("%s is seeded but unknown to the WhatsApp client", id)
		}
	}
}
