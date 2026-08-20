// Package migrations embeds the SQL migration files so they travel inside
// the binary rather than as a directory beside it.
//
// This exists for the native Windows install: cmd/bootstrap applies
// migrations at first run on a machine that has no Go toolchain, no network
// access and no copy of this repository — only the installed executables.
// Every other path (scripts/demo_up.sh, run_db_tests.sh, verify_migrations.sh)
// still shells out to the goose CLI against this same directory and is
// unaffected; goose ignores a .go file sitting among the .sql ones.
//
// embed.FS cannot reach outside its own directory, which is why this file
// lives here among the migrations rather than somewhere more obvious.
package migrations

import "embed"

// FS holds every migration, for goose.SetBaseFS.
//
// The pattern is deliberately explicit rather than `all:.` — that would
// also embed this file, and anything else that later lands in the
// directory, into every binary that imports the package.
//
//go:embed *.sql
var FS embed.FS
