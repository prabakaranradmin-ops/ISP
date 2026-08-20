// Command bootstrap prepares a fresh install: it applies every migration and
// generates the secrets the two services need, then exits.
//
// A separate run-once binary rather than work folded into cmd/api, for two
// reasons that both come down to privilege. It needs PostgreSQL superuser
// credentials — migration 019 creates roles and grants, and only a superuser
// can ALTER ROLE the application account's password — whereas api and
// radiusd deliberately connect as the least-privileged bss_app role and must
// never hold anything stronger. And it runs to completion under the
// installer rather than living as a service, so the superuser DSN exists for
// the seconds this process runs and is never written into a service's
// environment.
//
// Everything here is idempotent, because the installer runs it again on
// every upgrade. Migrations are idempotent through goose's own version
// table; secret generation is gated separately, on the credentials file
// already existing — re-running must never rotate a secret out from under
// services that are already using it.
//
// Usage:
//
//	bootstrap -superuser-dsn <dsn> -config-dir <dir> [-keys-dir <dir>]
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	// Registers the "pgx" driver for database/sql, which goose needs — it
	// takes a *sql.DB rather than the pgxpool the services use.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/maaransoft/isp-bss-oss/migrations"
)

const (
	// credentialsFile holds the generated secrets, in the .env format the
	// services already read. Its existence is what marks an install as
	// already provisioned.
	credentialsFile = "app.env"
	// keyStoreFile is the AES key store (pkg/crypto's "local:" scheme).
	keyStoreFile = "aes_keys.json"

	// secretLength is comfortably above config.minSecretLength (32), which
	// several secrets are validated against at service startup.
	secretLength = 48

	connectTimeout = 30 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		superuserDSN = flag.String("superuser-dsn", "",
			"PostgreSQL DSN with superuser rights, used once for migrations and role setup")
		configDir = flag.String("config-dir", ".",
			"directory the generated credentials file is written to")
		keysDir = flag.String("keys-dir", "",
			"directory the AES key store is written to (defaults to <config-dir>/keys)")
		appUser = flag.String("app-user", "bss_app",
			"the least-privilege role the services connect as (migration 019)")
		dbName = flag.String("db-name", "isp_bss_oss", "application database name")
		dbHost = flag.String("db-host", "127.0.0.1", "host the services will use to reach PostgreSQL")
		dbPort = flag.Int("db-port", 5432, "port the services will use to reach PostgreSQL")
	)
	flag.Parse()

	if *superuserDSN == "" {
		return fmt.Errorf("-superuser-dsn is required")
	}
	if *keysDir == "" {
		*keysDir = filepath.Join(*configDir, "keys")
	}

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	db, err := sql.Open("pgx", *superuserDSN)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close() //nolint:errcheck

	if err := waitForDatabase(ctx, db); err != nil {
		return err
	}
	fmt.Println("bootstrap: PostgreSQL reachable")

	if err := applyMigrations(db); err != nil {
		return err
	}

	provisioned, err := provisionSecrets(ctx, db, secretPaths{
		configDir: *configDir,
		keysDir:   *keysDir,
	}, dbTarget{
		user: *appUser, name: *dbName, host: *dbHost, port: *dbPort,
	})
	if err != nil {
		return err
	}
	if provisioned {
		fmt.Println("bootstrap: secrets generated")
	} else {
		fmt.Println("bootstrap: existing credentials left untouched")
	}

	fmt.Println("bootstrap: complete")
	return nil
}

// waitForDatabase retries until PostgreSQL answers or ctx expires. The
// installer starts the database service moments earlier, and "not accepting
// connections yet" is the normal state for the first second or two — not a
// failure to report.
func waitForDatabase(ctx context.Context, db *sql.DB) error {
	for {
		if err := db.PingContext(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("PostgreSQL did not become reachable: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// applyMigrations brings the schema up to date from the embedded files.
//
// Idempotent by construction: goose records what it has applied in
// goose_db_version and runs only what is missing, so an installer upgrade
// applies just the new migrations and a re-run of the same version does
// nothing at all.
func applyMigrations(db *sql.DB) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	// Quiet: goose logs one line per already-applied migration, which on an
	// upgrade is dozens of lines saying nothing happened.
	goose.SetLogger(goose.NopLogger())

	before, err := goose.GetDBVersion(db)
	if err != nil {
		return fmt.Errorf("read current schema version: %w", err)
	}

	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	after, err := goose.GetDBVersion(db)
	if err != nil {
		return fmt.Errorf("read resulting schema version: %w", err)
	}

	switch {
	case before == after:
		fmt.Printf("bootstrap: schema already at version %d\n", after)
	default:
		fmt.Printf("bootstrap: schema migrated from version %d to %d\n", before, after)
	}
	return nil
}

type secretPaths struct {
	configDir string
	keysDir   string
}

type dbTarget struct {
	user string
	name string
	host string
	port int
}

// provisionSecrets generates the application's secrets on a first run,
// reporting whether it did anything.
//
// The credentials file's existence is the gate, deliberately separate from
// goose's version table: those two answer different questions. A schema can
// be freshly migrated on an install whose secrets were provisioned months
// ago, and rotating them then would lock out the running services and make
// every AES-encrypted column unreadable.
func provisionSecrets(ctx context.Context, db *sql.DB, paths secretPaths, target dbTarget) (bool, error) {
	credPath := filepath.Join(paths.configDir, credentialsFile)
	keyPath := filepath.Join(paths.keysDir, keyStoreFile)

	credExists, err := fileExists(credPath)
	if err != nil {
		return false, err
	}
	keyExists, err := fileExists(keyPath)
	if err != nil {
		return false, err
	}

	if credExists && keyExists {
		return false, nil
	}
	// One present without the other means a half-finished previous run, or
	// a file removed by hand. Refusing is the safe answer: generating the
	// missing half would pair a fresh key store with an existing database
	// whose encrypted columns only the old keys can read.
	if credExists != keyExists {
		return false, fmt.Errorf(
			"inconsistent install state: %s exists=%v but %s exists=%v — "+
				"restore the missing file from backup rather than letting this regenerate it, "+
				"since a new AES key store cannot decrypt existing data",
			credPath, credExists, keyPath, keyExists)
	}

	appPassword, err := randomSecret()
	if err != nil {
		return false, err
	}

	// Migration 019 creates bss_app without a password, because that file is
	// committed to git. Setting it is this program's job, as the superuser.
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf("ALTER ROLE %s WITH PASSWORD %s", quoteIdent(target.user), quoteLiteral(appPassword)),
	); err != nil {
		return false, fmt.Errorf("set %s password: %w", target.user, err)
	}

	secrets := map[string]string{}
	for _, key := range []string{
		"JWT_SECRET",
		"PORTAL_JWT_SECRET",
		"RADIUS_SECRET",
		"RADIUS_VERIFIER_SECRET",
	} {
		v, err := randomSecret()
		if err != nil {
			return false, err
		}
		secrets[key] = v
	}

	if err := writeKeyStore(keyPath); err != nil {
		return false, err
	}
	if err := writeCredentials(credPath, keyPath, appPassword, secrets, target); err != nil {
		return false, err
	}
	return true, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

// randomSecret returns a URL-safe secret of at least secretLength characters.
func randomSecret() (string, error) {
	buf := make([]byte, secretLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	// RawURLEncoding: no padding and no '+' or '/', so the value needs no
	// quoting in the .env file the services read.
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// writeKeyStore creates the AES key store pkg/crypto reads through its
// "local:" scheme. AES-256 requires exactly 32 bytes.
func writeKeyStore(path string) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fmt.Errorf("generate AES key: %w", err)
	}
	payload, err := json.MarshalIndent(struct {
		ActiveVersion string            `json:"active_version"`
		Keys          map[string]string `json:"keys"`
	}{
		ActiveVersion: "v1",
		Keys:          map[string]string{"v1": base64.StdEncoding.EncodeToString(key)},
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode key store: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create key store directory: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return fmt.Errorf("write key store: %w", err)
	}
	return nil
}

// writeCredentials writes the services' environment file.
func writeCredentials(path, keyPath, appPassword string, secrets map[string]string, target dbTarget) error {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		target.user, appPassword, target.host, target.port, target.name)

	var b strings.Builder
	b.WriteString("# Generated by bootstrap on first install. Do not edit by hand:\n")
	b.WriteString("# the database role's password and the AES key store are paired with\n")
	b.WriteString("# what is already stored in the database, and changing either here\n")
	b.WriteString("# without changing it there locks the services out.\n")
	b.WriteString("#\n")
	b.WriteString("# Integration credentials (WhatsApp, SMS, Razorpay, SMTP) are not\n")
	b.WriteString("# generated — they belong to accounts this installer cannot create.\n")
	b.WriteString("# Add them here; each unset one disables its own feature and is\n")
	b.WriteString("# reported at startup rather than failing it.\n\n")

	fmt.Fprintf(&b, "DB_DSN=%s\n", dsn)
	fmt.Fprintf(&b, "AES_KEY_STORE_URL=local:%s\n", filepath.ToSlash(keyPath))
	for _, key := range []string{
		"JWT_SECRET",
		"PORTAL_JWT_SECRET",
		"RADIUS_SECRET",
		"RADIUS_VERIFIER_SECRET",
	} {
		fmt.Fprintf(&b, "%s=%s\n", key, secrets[key])
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// quoteIdent quotes a SQL identifier. The role name comes from a flag, not
// from user input, but ALTER ROLE takes no parameters so it has to be
// interpolated — quoting it keeps that interpolation safe rather than
// merely unlikely to matter.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral quotes a SQL string literal, for the same reason.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
