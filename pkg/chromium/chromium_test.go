package chromium

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocate_ExplicitPathReturnedWhenItExists(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "my-browser.exe")
	if err := os.WriteFile(exe, []byte("not a real browser"), 0o755); err != nil { //nolint:gosec
		t.Fatalf("write fake exe: %v", err)
	}

	got, err := Locate(exe)
	if err != nil {
		t.Fatalf("Locate(%q) = %v, want nil error", exe, err)
	}
	if got != exe {
		t.Errorf("Locate(%q) = %q, want it returned unchanged", exe, got)
	}
}

func TestLocate_ExplicitPathMissingErrorsRatherThanFallingBack(t *testing.T) {
	// An operator who set CHROMIUM_PATH to a typo'd path must see that
	// error, not have it silently discarded in favour of whatever
	// auto-detection happens to find on the machine.
	missing := filepath.Join(t.TempDir(), "does-not-exist.exe")

	_, err := Locate(missing)
	if err == nil {
		t.Fatal("Locate() on a missing explicit path: got nil error, want one")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("a missing explicit path should report why that path failed, not the auto-detection ErrNotFound")
	}
}

func TestLocate_AutoDetectFindsSomethingOrReportsErrNotFound(t *testing.T) {
	// This can't assert a specific outcome without control over the test
	// machine's installed browsers, but it must do one of exactly two
	// things: return a path that really exists, or return ErrNotFound. Any
	// other error (a typo in a candidate list, a panic) is a bug.
	got, err := Locate("")
	switch {
	case err == nil:
		if _, statErr := os.Stat(got); statErr != nil {
			t.Errorf("Locate(\"\") returned %q but it does not exist: %v", got, statErr)
		}
	case errors.Is(err, ErrNotFound):
		// No browser on this machine — a legitimate outcome, not a test
		// failure.
	default:
		t.Errorf("Locate(\"\") = %v, want either nil or ErrNotFound", err)
	}
}
