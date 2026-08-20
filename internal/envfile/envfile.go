// Package envfile loads KEY=value pairs from a dotenv-style file into the
// process environment.
//
// Every other environment-configured piece of this system (internal/config,
// cmd/bootstrap) already just reads os.Getenv — that works unchanged when a
// shell has sourced app.env first, which is how the services run under
// Docker Compose and how a developer runs them by hand. A Windows service
// has no shell in its startup path at all: the Service Control Manager
// execs the binary directly with a fixed environment, so app.env (written
// once by cmd/bootstrap and never touched again — see its package doc on
// why regenerating it on every start would be wrong) has to be read by the
// process itself. This package is that read, kept separate from
// internal/config so config.Load stays what it has always been: a reader
// of the environment, not a chooser of where the environment comes from.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Load reads path as KEY=value lines and calls os.Setenv for each key not
// already present in the process environment.
//
// Real environment variables always win over the file. That matters for the
// same reason it matters when a shell sources app.env and a caller has also
// exported an override: a developer or an install script pointing DB_DSN at
// a different database for one run should not be silently overruled by the
// file cmd/bootstrap wrote.
//
// path == "" is a no-op, not an error — the flag/env var selecting it is
// optional, since a Docker Compose or manually-sourced-shell deployment has
// no need for it. A non-empty path that does not exist is treated as a
// misconfiguration and returned as an error, since the caller asked for a
// specific file.
func Load(path string) error {
	if path == "" {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("envfile: open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("envfile: %s:%d: no '=' in %q", path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return fmt.Errorf("envfile: %s:%d: empty key", path, lineNo)
		}

		if _, alreadySet := os.LookupEnv(key); alreadySet {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("envfile: set %s: %w", key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("envfile: read %s: %w", path, err)
	}
	return nil
}
