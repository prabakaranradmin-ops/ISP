//go:build integration

// The decryption half of a restore check.
//
// Driven by scripts/windows/verify_restore.ps1 against a scratch database
// holding a restored backup, and skipped otherwise so it never runs as part of
// the ordinary suite.
//
// It lives here, using the application's own crypto, rather than being
// reimplemented in the PowerShell that calls it. A second implementation of
// the decryption path would be the piece most likely to be wrong, and it would
// be wrong in the direction that matters: reporting a backup as recoverable
// when it is not.
package db_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

// TestRestoredKYCDecrypts reads the encrypted KYC out of a restored database
// and opens it with the key store that came from the same backup.
//
// This is the check a database-only backup fails. KYC ciphertext restores
// perfectly from pg_dump; the keys are a file on disk that pg_dump never saw.
// Restore the dump beside a freshly generated key store and every Aadhaar and
// PAN is permanently unreadable, with no error at restore time and none
// afterwards until somebody opens a subscriber record.
func TestRestoredKYCDecrypts(t *testing.T) {
	dsn := os.Getenv("RESTORE_CHECK_DSN")
	keyStoreURL := os.Getenv("RESTORE_CHECK_KEYSTORE")
	if dsn == "" || keyStoreURL == "" {
		t.Skip("RESTORE_CHECK_DSN/RESTORE_CHECK_KEYSTORE not set — run via scripts/windows/verify_restore.ps1")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to the restored database: %v", err)
	}
	defer pool.Close()

	store, err := crypto.LoadKeyStore(keyStoreURL)
	if err != nil {
		t.Fatalf("load the backed-up key store: %v", err)
	}
	dec := crypto.StoreDecryptor{Store: store}

	rows, err := pool.Query(ctx, `
		SELECT subscriber_id, COALESCE(aadhaar_encrypted, ''), COALESCE(pan_encrypted, '')
		  FROM kyc_verifications
		 WHERE aadhaar_encrypted IS NOT NULL OR pan_encrypted IS NOT NULL`)
	if err != nil {
		t.Fatalf("read kyc_verifications: %v", err)
	}
	defer rows.Close()

	checked := 0
	for rows.Next() {
		var subscriberID int
		var aadhaar, pan string
		if err := rows.Scan(&subscriberID, &aadhaar, &pan); err != nil {
			t.Fatalf("scan kyc row: %v", err)
		}
		for label, ciphertext := range map[string]string{"aadhaar": aadhaar, "pan": pan} {
			if ciphertext == "" {
				continue
			}
			plain, err := dec.Decrypt(ciphertext)
			if err != nil {
				t.Errorf("subscriber %d %s: %v\n\n"+
					"This backup's key store cannot open its own ciphertext. The database "+
					"restored, but the KYC in it is unrecoverable — check that the keys "+
					"directory in the backup is the one that was live when the dump was taken, "+
					"and that no retired key version was pruned from it.",
					subscriberID, label, err)
				continue
			}
			// Decrypting to empty would satisfy "no error" while proving
			// nothing, so require the plaintext to actually be there.
			if strings.TrimSpace(plain) == "" {
				t.Errorf("subscriber %d %s decrypted to an empty string", subscriberID, label)
			}
			checked++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate kyc rows: %v", err)
	}

	// The caller only invokes this when it has already counted rows to check,
	// so finding none means the query disagrees with that count — which would
	// otherwise pass silently as a vacuous success.
	if checked == 0 {
		t.Fatal("no KYC values were decrypted; the check proved nothing")
	}
	t.Logf("decrypted %d KYC value(s) from the restored database", checked)
}
