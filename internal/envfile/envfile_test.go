package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_EmptyPathIsNoop(t *testing.T) {
	if err := Load(""); err != nil {
		t.Fatalf("Load(\"\") = %v, want nil", err)
	}
}

func TestLoad_MissingFileErrors(t *testing.T) {
	err := Load(filepath.Join(t.TempDir(), "does-not-exist.env"))
	if err == nil {
		t.Fatal("Load() on a missing file: got nil error, want one")
	}
}

func TestLoad_SetsUnsetKeys(t *testing.T) {
	path := writeEnvFile(t, "DB_DSN=postgres://x\nJWT_SECRET=abc123\n")

	t.Setenv("DB_DSN", "")
	os.Unsetenv("DB_DSN")
	t.Setenv("JWT_SECRET", "")
	os.Unsetenv("JWT_SECRET")

	if err := Load(path); err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if got := os.Getenv("DB_DSN"); got != "postgres://x" {
		t.Errorf("DB_DSN = %q, want %q", got, "postgres://x")
	}
	if got := os.Getenv("JWT_SECRET"); got != "abc123" {
		t.Errorf("JWT_SECRET = %q, want %q", got, "abc123")
	}
}

func TestLoad_RealEnvironmentWinsOverFile(t *testing.T) {
	path := writeEnvFile(t, "DB_DSN=from-file\n")
	t.Setenv("DB_DSN", "from-real-environment")

	if err := Load(path); err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}

	if got := os.Getenv("DB_DSN"); got != "from-real-environment" {
		t.Errorf("DB_DSN = %q, want the real environment value to survive unclobbered", got)
	}
}

func TestLoad_SkipsBlankLinesAndComments(t *testing.T) {
	path := writeEnvFile(t, "\n# a comment\n  \nLOG_LEVEL=debug\n# LOG_LEVEL=ignored\n")
	os.Unsetenv("LOG_LEVEL")

	if err := Load(path); err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	if got := os.Getenv("LOG_LEVEL"); got != "debug" {
		t.Errorf("LOG_LEVEL = %q, want %q", got, "debug")
	}
}

func TestLoad_ValueMayContainEquals(t *testing.T) {
	// A DSN's query string ("?sslmode=disable") contains '=' itself, so only
	// the first '=' on the line may split key from value.
	path := writeEnvFile(t, "DB_DSN=postgres://u:p@h/db?sslmode=disable\n")
	os.Unsetenv("DB_DSN")

	if err := Load(path); err != nil {
		t.Fatalf("Load() = %v, want nil", err)
	}
	want := "postgres://u:p@h/db?sslmode=disable"
	if got := os.Getenv("DB_DSN"); got != want {
		t.Errorf("DB_DSN = %q, want %q", got, want)
	}
}

func TestLoad_MalformedLineErrors(t *testing.T) {
	path := writeEnvFile(t, "NOT_A_KEY_VALUE_PAIR\n")
	if err := Load(path); err == nil {
		t.Fatal("Load() on a line with no '=': got nil error, want one")
	}
}

func writeEnvFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write test env file: %v", err)
	}
	return path
}
