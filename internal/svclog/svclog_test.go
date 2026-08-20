package svclog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/config"
)

func TestConfigure_LogFileReceivesOutput(t *testing.T) {
	path := filepath.Join(tempDir(t), "svc.log")
	Configure(&config.Config{LogLevel: "info", LogFormat: "json", LogFile: path}, "test")

	log.Info().Msg("hello from the test")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "hello from the test") {
		t.Errorf("log file contents = %q, want it to contain the logged message", data)
	}
}

func TestConfigure_LogFileAppendsAcrossCalls(t *testing.T) {
	// A service restart must not erase the entries explaining why the
	// previous run stopped — Configure is what a fresh process calls on
	// every start, so it has to open in append mode, not truncate.
	path := filepath.Join(tempDir(t), "svc.log")

	Configure(&config.Config{LogLevel: "info", LogFormat: "json", LogFile: path}, "test")
	log.Info().Msg("first run")

	Configure(&config.Config{LogLevel: "info", LogFormat: "json", LogFile: path}, "test")
	log.Info().Msg("second run")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(data), "first run") || !strings.Contains(string(data), "second run") {
		t.Errorf("log file contents = %q, want both runs' entries present", data)
	}
}

func TestConfigure_UnwritableLogFileFallsBackWithoutPanicking(t *testing.T) {
	// A directory that does not exist can never be opened as a file — this
	// stands in for whatever bad LOG_FILE an operator might set, and the
	// point under test is that Configure survives it rather than the
	// service failing to start over a logging misconfiguration.
	badPath := filepath.Join(tempDir(t), "no-such-dir", "svc.log")

	Configure(&config.Config{LogLevel: "info", LogFormat: "json", LogFile: badPath}, "test")
	log.Info().Msg("should not panic")

	if _, err := os.Stat(badPath); err == nil {
		t.Error("expected the bad LOG_FILE path to remain unwritten, found a file there")
	}
}

// tempDir is t.TempDir() with best-effort cleanup instead of asserted
// cleanup. Configure never closes the file it opens — correctly: a live
// service's log file stays open for the process's whole lifetime, there is
// no "done with it" moment to close on — so on Windows, where an open
// file's directory entry cannot be removed, t.TempDir()'s own RemoveAll
// cleanup would fail the test over exactly the behaviour being verified.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "svclog-test-*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
