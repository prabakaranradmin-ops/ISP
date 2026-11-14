package api_test

import (
	"context"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

// fakeCredentialStore implements api.CredentialQuerier with an in-memory cell.
type fakeCredentialStore struct {
	hash string
	enc  string
}

func (f *fakeCredentialStore) GetPasswordHash(_ context.Context, _ string) (string, error) {
	return f.hash, nil
}
func (f *fakeCredentialStore) GetSubscriberPasswordEnc(_ context.Context, _ int) (string, error) {
	return f.enc, nil
}
func (f *fakeCredentialStore) SetSubscriberPasswordEnc(_ context.Context, _ int, enc string) error {
	f.enc = enc
	return nil
}
func (f *fakeCredentialStore) SetSubscriberCredential(_ context.Context, _ int, hash, enc string) error {
	f.hash, f.enc = hash, enc
	return nil
}

func newCredentialHandler(t *testing.T, store *fakeCredentialStore) *api.Handler {
	t.Helper()
	ks, err := crypto.NewInMemoryKeyStore(map[string][]byte{"v1": make([]byte, 32)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	enc, err := crypto.NewAESEncryptor(ks)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewHandler(api.HandlerDeps{
		KeyStore: ks, SecretEncryptor: enc, Credentials: store,
	})
}

// TestStoreAndRevealRoundTrip: a captured password reveals back to the same
// plaintext, and is not stored in the clear.
func TestStoreAndRevealRoundTrip(t *testing.T) {
	store := &fakeCredentialStore{}
	h := newCredentialHandler(t, store)
	ctx := context.Background()

	const pw = "Rb-s3cret-99"
	if err := h.StoreSubscriberPasswordEnc(ctx, 7, pw); err != nil {
		t.Fatalf("store: %v", err)
	}
	if store.enc == "" || store.enc == pw {
		t.Fatalf("password was not encrypted at rest: %q", store.enc)
	}

	got, stored, err := h.RevealSubscriberPassword(ctx, 7)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if !stored || got != pw {
		t.Fatalf("reveal round-trip: stored=%v got=%q want %q", stored, got, pw)
	}
}

// TestRevealNotCaptured: a subscriber with no stored copy reports stored=false
// rather than erroring.
func TestRevealNotCaptured(t *testing.T) {
	h := newCredentialHandler(t, &fakeCredentialStore{})
	_, stored, err := h.RevealSubscriberPassword(context.Background(), 7)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if stored {
		t.Error("stored should be false when no password_enc is present")
	}
}

// TestResetWritesVerifiableHashAndRevealableCopy: after a reset, the stored hash
// verifies against the new password and the stored copy reveals to it.
func TestResetWritesVerifiableHashAndRevealableCopy(t *testing.T) {
	store := &fakeCredentialStore{}
	h := newCredentialHandler(t, store)
	ctx := context.Background()

	const pw = "new-pass-123"
	if err := h.ResetSubscriberPassword(ctx, 7, pw); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(store.hash), []byte(pw)); err != nil {
		t.Errorf("stored hash does not verify against the new password: %v", err)
	}
	got, err := crypto.Decrypt(store.enc, mustKeyStore(t))
	if err != nil {
		t.Fatalf("decrypt stored copy: %v", err)
	}
	if got != pw {
		t.Errorf("revealable copy = %q, want %q", got, pw)
	}
}

// TestResetRejectsTooShort: a too-short password is refused as a validation
// error, so the console can show it rather than a 500.
func TestResetRejectsTooShort(t *testing.T) {
	h := newCredentialHandler(t, &fakeCredentialStore{})
	err := h.ResetSubscriberPassword(context.Background(), 7, "x")
	if err == nil {
		t.Fatal("want an error for a too-short password")
	}
}

func mustKeyStore(t *testing.T) crypto.KeyStore {
	t.Helper()
	ks, err := crypto.NewInMemoryKeyStore(map[string][]byte{"v1": make([]byte, 32)}, "v1")
	if err != nil {
		t.Fatal(err)
	}
	return ks
}
