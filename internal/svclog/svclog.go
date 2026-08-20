// Package svclog wires zerolog's global logger from *config.Config.
//
// It was cmd/api and cmd/radiusd's own configureLogging, identical in both,
// until LogFile support (below) needed adding to both at once — the
// duplication was a copy-paste risk from the moment there were two of them,
// and adding a file-with-fallback path made keeping them in sync by hand a
// real chance to get one out of step with the other. Extracted rather than
// re-duplicated.
package svclog

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/config"
)

// Configure sets zerolog's global level and output from cfg. prefix names
// the caller in the one message this can itself only print to stderr (a bad
// LOG_FILE path, before there is anywhere else to say so) — "api" or
// "radiusd".
func Configure(cfg *config.Config, prefix string) {
	zerolog.TimeFieldFormat = time.RFC3339
	if level, err := zerolog.ParseLevel(cfg.LogLevel); err == nil {
		zerolog.SetGlobalLevel(level)
	}

	var out io.Writer = os.Stdout
	if cfg.LogFile != "" {
		// A Windows service has no console for stdout to reach, so
		// LOG_FILE (set by scripts/windows/register_services.ps1) is how
		// its logs become visible at all. Opened append-only: this process
		// restarts far more often than an operator rotates the file, and
		// truncating on every restart would erase exactly the log entries
		// most likely to explain why the previous run stopped.
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // operator-configured path, not user input
		if err != nil {
			// Falls back to stdout rather than failing startup — a bad log
			// path is worth fixing, but should not be why the service
			// itself refuses to run. Under a real Windows service this
			// stderr write goes nowhere either, same as stdout would have;
			// the fallback exists for the interactive/Docker case, where a
			// misconfigured LOG_FILE is still visible on the console.
			fmt.Fprintf(os.Stderr, "%s: cannot open LOG_FILE %s: %v (logging to stdout instead)\n", prefix, cfg.LogFile, err)
		} else {
			out = f
		}
	}

	if cfg.LogFormat != "json" {
		// NoColor when writing to a file: ANSI escapes belong in a
		// terminal, not in a file an operator or Event Viewer's "open log
		// file" link will read as text.
		out = zerolog.ConsoleWriter{Out: out, TimeFormat: time.RFC3339, NoColor: cfg.LogFile != ""}
	}
	log.Logger = log.Output(out)
}
