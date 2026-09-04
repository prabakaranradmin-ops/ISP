package migrations_test

import (
	"bufio"
	"regexp"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/migrations"
)

// goose splits a migration into statements on semicolons, and a dollar-quoted
// body (a PL/pgSQL function, a DO block) is full of them. Unless the statement
// is wrapped in StatementBegin/StatementEnd, goose chops the body into
// fragments — each one looking like a plausible statement, none of them the
// function that was written.
//
// The reason this needs a test rather than a code review is that goose does
// not fail on it. It applies the fragments, records the migration as done, and
// leaves a corrupted function behind; `go build` and `go test` never look at
// SQL, and scripts/verify_migrations.sh needs Docker, which the native Windows
// deployment does not have. On that deployment the first sign of trouble was a
// broken function at install time, on a machine with no toolchain to debug it.
//
// So this asserts the rule directly instead of asking a parser that would
// answer "fine" either way.

var (
	dollarQuote    = regexp.MustCompile(`\$[A-Za-z_]*\$`)
	statementBegin = regexp.MustCompile(`(?i)--\s*\+goose\s+StatementBegin`)
	statementEnd   = regexp.MustCompile(`(?i)--\s*\+goose\s+StatementEnd`)
)

func TestDollarQuotedBodiesAreWrappedForGoose(t *testing.T) {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations embedded — the go:embed pattern has stopped matching")
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			body, err := migrations.FS.ReadFile(name)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			var (
				inBlock  bool
				begins   int
				ends     int
				inQuote  bool
				scanner  = bufio.NewScanner(strings.NewReader(string(body)))
				lineNo   int
				unwrappd []int
			)
			scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

			for scanner.Scan() {
				lineNo++
				line := scanner.Text()

				switch {
				case statementBegin.MatchString(line):
					inBlock, begins = true, begins+1
					continue
				case statementEnd.MatchString(line):
					inBlock, ends = false, ends+1
					continue
				}

				// Each delimiter on the line toggles in/out of a quoted body,
				// so a body opened and closed on one line nets out correctly.
				for range dollarQuote.FindAllString(line, -1) {
					if !inQuote && !inBlock {
						unwrappd = append(unwrappd, lineNo)
					}
					inQuote = !inQuote
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatalf("scan: %v", err)
			}

			if len(unwrappd) > 0 {
				t.Errorf("dollar-quoted body at line(s) %v is not inside a "+
					"-- +goose StatementBegin/StatementEnd pair; goose will split it on "+
					"its semicolons and apply the fragments without reporting an error",
					unwrappd)
			}
			if begins != ends {
				t.Errorf("unbalanced goose annotations: %d StatementBegin, %d StatementEnd", begins, ends)
			}
			if inQuote {
				t.Error("a dollar-quoted body is never closed")
			}
		})
	}
}
