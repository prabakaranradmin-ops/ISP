package crypto_test

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

// What a database backup on its own is worth, for encrypted data.
//
// KYC (Aadhaar, PAN) is stored as version-prefixed ciphertext in
// subscriber_kyc. The keys that open it are not in the database — they are a
// 102-byte JSON file on disk, written by cmd/bootstrap into <config>/keys.
// pg_dump cannot see it.
//
// So "we have backups" can be true and useless at the same time: restore the
// dump onto a rebuilt machine, let bootstrap generate a fresh key store, and
// every KYC value is permanently unreadable. Nothing errors during the
// restore. Nothing errors afterwards either, until somebody opens a
// subscriber record — possibly months later, possibly during the audit or the
// LEA request that is the reason KYC is retained at all.
//
// These tests exist because that is easy to say and easy to keep not
// checking. They run against the real file-backed key store, with no database.

// writeKeyStore creates a key store file the way bootstrap does.
func writeKeyStore(t *testing.T, dir, activeVersion string, keys map[string][]byte) string {
	t.Helper()
	encoded := map[string]string{}
	for v, k := range keys {
		encoded[v] = base64.StdEncoding.EncodeToString(k)
	}
	body, err := json.Marshal(map[string]any{
		"active_version": activeVersion,
		"keys":           encoded,
	})
	if err != nil {
		t.Fatalf("marshal key file: %v", err)
	}
	path := filepath.Join(dir, "aes_keys.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return "local:" + path
}

func key(t *testing.T, fill byte) []byte {
	t.Helper()
	k := make([]byte, 32)
	for i := range k {
		k[i] = fill
	}
	return k
}

// The whole point, stated as a test: ciphertext survives a database restore,
// and is worthless without the key file that travelled separately.
func TestRestore_WithoutTheKeyStoreKYCIsUnrecoverable(t *testing.T) {
	const aadhaar = "123456789012"

	// Before the disaster: encrypt as the live system would.
	originalURL := writeKeyStore(t, t.TempDir(), "v1", map[string][]byte{"v1": key(t, 0xA1)})
	originalStore, err := crypto.LoadKeyStore(originalURL)
	if err != nil {
		t.Fatalf("load original key store: %v", err)
	}
	enc, err := crypto.NewAESEncryptor(originalStore)
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	ciphertext, err := enc.Encrypt(aadhaar)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// After: the database is restored (so the ciphertext is back verbatim),
	// but bootstrap generated a new key store on the rebuilt machine.
	rebuiltURL := writeKeyStore(t, t.TempDir(), "v1", map[string][]byte{"v1": key(t, 0xB2)})
	rebuiltStore, err := crypto.LoadKeyStore(rebuiltURL)
	if err != nil {
		t.Fatalf("load rebuilt key store: %v", err)
	}

	if _, err := (crypto.StoreDecryptor{Store: rebuiltStore}).Decrypt(ciphertext); err == nil {
		t.Fatal("KYC decrypted under a key store that never sealed it — " +
			"that would mean the ciphertext is not actually protected")
	}

	// And the same ciphertext with the original key file: readable. This is
	// what a backup has to include for the restore to be worth anything.
	if got, err := (crypto.StoreDecryptor{Store: originalStore}).Decrypt(ciphertext); err != nil {
		t.Errorf("decrypt with the original key store: %v", err)
	} else if got != aadhaar {
		t.Errorf("round trip: got %q, want %q", got, aadhaar)
	}
}

// A retired key version must stay in the file. Rotation re-seals new values
// under the new version and leaves old rows sealed under the old one, so
// pruning the old key is the same disaster arriving one rotation later.
func TestRestore_RetiredKeyVersionsMustStayInTheStore(t *testing.T) {
	const pan = "ABCDE1234F"

	dir := t.TempDir()
	v1 := key(t, 0xC3)
	v2 := key(t, 0xD4)

	// Sealed under v1, before a rotation.
	oldURL := writeKeyStore(t, dir, "v1", map[string][]byte{"v1": v1})
	oldStore, err := crypto.LoadKeyStore(oldURL)
	if err != nil {
		t.Fatalf("load v1 store: %v", err)
	}
	enc, err := crypto.NewAESEncryptor(oldStore)
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	ciphertext, err := enc.Encrypt(pan)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(ciphertext, "v1:") {
		t.Fatalf("ciphertext should carry its key version, got %q", ciphertext)
	}

	// Rotated correctly: v2 active, v1 retained.
	bothURL := writeKeyStore(t, t.TempDir(), "v2", map[string][]byte{"v1": v1, "v2": v2})
	bothStore, err := crypto.LoadKeyStore(bothURL)
	if err != nil {
		t.Fatalf("load rotated store: %v", err)
	}
	if got, err := (crypto.StoreDecryptor{Store: bothStore}).Decrypt(ciphertext); err != nil {
		t.Errorf("a correctly rotated store must still open v1 ciphertext: %v", err)
	} else if got != pan {
		t.Errorf("round trip after rotation: got %q, want %q", got, pan)
	}

	// Rotated destructively: v1 dropped. Every row sealed under it is gone.
	prunedURL := writeKeyStore(t, t.TempDir(), "v2", map[string][]byte{"v2": v2})
	prunedStore, err := crypto.LoadKeyStore(prunedURL)
	if err != nil {
		t.Fatalf("load pruned store: %v", err)
	}
	if _, err := (crypto.StoreDecryptor{Store: prunedStore}).Decrypt(ciphertext); err == nil {
		t.Error("dropping a retired key version left old ciphertext readable, which " +
			"cannot be true — the test fixture is wrong")
	}
}
