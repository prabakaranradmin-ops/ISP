// Package chromium locates a Chromium-based browser executable for
// billing.InvoicePDFClient to drive over the Chrome DevTools Protocol.
//
// This system used to send invoice HTML to a Gotenberg container, which ran
// its own bundled Chromium inside Docker. The native Windows install has no
// container to run one in, and rather than bundling a second copy of
// Chromium — a few hundred MB, next to a machine that already ships one —
// Locate finds the browser Windows itself provides: Microsoft Edge, present
// on every Windows Server (2019+) and Windows 10/11 install by default since
// 2020. Google Chrome is checked too, for a machine where it happens to be
// installed instead; Locate does not care which one it finds, only that it
// speaks the same DevTools protocol chromedp needs.
package chromium

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ErrNotFound is returned by Locate when no candidate browser exists.
var ErrNotFound = errors.New("chromium: no Chromium-based browser found")

// candidates are checked in order; the first one that exists wins. Absolute
// paths (the Windows install locations) are checked with a stat; bare names
// are resolved against PATH, which is what makes the non-Windows entries
// here work on a developer's Linux or macOS machine — not this package's
// deployment target, but go test ./... should not need Windows to run.
func candidates() []string {
	var paths []string
	// Edge before Chrome: Edge is the one actually guaranteed present on
	// the Windows versions this system targets, so checking it first keeps
	// the common case to one stat call instead of a full walk of both.
	for _, envVar := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
		root := os.Getenv(envVar)
		if root == "" {
			continue
		}
		paths = append(paths,
			filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"),
			filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"),
		)
	}
	paths = append(paths,
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge")
	return paths
}

// Locate returns the path to a Chromium-based browser executable.
//
// explicitPath, when non-empty, is CHROMIUM_PATH from configuration: an
// operator who set it gets exactly that binary or a clear error, never a
// silent fall-through to auto-detection — the same rule an explicitly
// configured ArchiveDir or TLSCertDir already follows in internal/config.
// Leave it empty to auto-detect.
func Locate(explicitPath string) (string, error) {
	if explicitPath != "" {
		if _, err := os.Stat(explicitPath); err != nil {
			return "", fmt.Errorf("chromium: CHROMIUM_PATH %s: %w", explicitPath, err)
		}
		return explicitPath, nil
	}

	for _, candidate := range candidates() {
		if filepath.IsAbs(candidate) {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
			continue
		}
		if resolved, err := exec.LookPath(candidate); err == nil {
			return resolved, nil
		}
	}
	return "", ErrNotFound
}
