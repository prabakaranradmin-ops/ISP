package nas

import (
	"context"
	"net"
	"testing"

	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

type stubDeviceStore struct {
	devices    []DeviceRow
	profiles   []PlanProfileRow
	replyAttrs []ReplyAttribute
	replyErr   error
}

func (s *stubDeviceStore) ListNASDevices(context.Context) ([]DeviceRow, error) {
	return s.devices, nil
}

func (s *stubDeviceStore) ListEnabledReplyAttributes(context.Context) ([]ReplyAttribute, error) {
	return s.replyAttrs, s.replyErr
}

func (s *stubDeviceStore) ListPlanNASProfiles(context.Context) ([]PlanProfileRow, error) {
	return s.profiles, nil
}

func testKeyStore(t *testing.T) (crypto.KeyStore, *crypto.AESEncryptor) {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	ks, err := crypto.NewInMemoryKeyStore(map[string][]byte{"v1": key}, "v1")
	if err != nil {
		t.Fatalf("build key store: %v", err)
	}
	enc, err := crypto.NewAESEncryptor(ks)
	if err != nil {
		t.Fatalf("build encryptor: %v", err)
	}
	return ks, enc
}

func TestResolver_Refresh_DecryptsRegisteredDevice(t *testing.T) {
	keyStore, encryptor := testKeyStore(t)
	secretCT, err := encryptor.Encrypt("cisco-router-secret")
	if err != nil {
		t.Fatalf("encrypt secret: %v", err)
	}

	store := &stubDeviceStore{
		devices: []DeviceRow{
			{IP: "10.0.0.5", Vendor: "cisco", RadiusSecretEncrypted: secretCT, CoAPort: 3799, PoDPort: 3799},
		},
		profiles: []PlanProfileRow{
			{PlanID: 7, Vendor: "cisco", ProfileName: "PLAN_100M"},
		},
	}

	r := NewResolver(store, keyStore, []byte("global-fallback-secret"), 1700)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	device := r.Resolve("10.0.0.5")
	if device.Vendor != VendorCisco {
		t.Errorf("vendor = %q, want %q", device.Vendor, VendorCisco)
	}
	if string(device.Secret) != "cisco-router-secret" {
		t.Errorf("secret = %q, want the decrypted plaintext", device.Secret)
	}
	if device.CoAPort != 3799 {
		t.Errorf("CoA port = %d, want 3799 (registered override, not the 1700 default)", device.CoAPort)
	}

	if got := r.ResolveProfile(7, VendorCisco); got != "PLAN_100M" {
		t.Errorf("ResolveProfile(7, cisco) = %q, want PLAN_100M", got)
	}
	if got := r.ResolveProfile(7, VendorJuniper); got != "" {
		t.Errorf("ResolveProfile(7, juniper) = %q, want empty (no row for that vendor)", got)
	}
}

// TestResolver_UnregisteredNAS_FallsBackToMikrotikAndGlobalSecret is the
// rollout-safety contract MDS §4.11 documents: an upgraded deployment with
// an empty nas_devices table must behave exactly as it did before this
// package existed.
func TestResolver_UnregisteredNAS_FallsBackToMikrotikAndGlobalSecret(t *testing.T) {
	keyStore, _ := testKeyStore(t)
	store := &stubDeviceStore{}
	r := NewResolver(store, keyStore, []byte("global-fallback-secret"), 1700)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	device := r.Resolve("192.0.2.99")
	if device.Vendor != VendorMikrotik {
		t.Errorf("vendor = %q, want mikrotik fallback", device.Vendor)
	}
	if string(device.Secret) != "global-fallback-secret" {
		t.Errorf("secret = %q, want the configured global fallback", device.Secret)
	}
	if device.CoAPort != 1700 {
		t.Errorf("CoA port = %d, want the default 1700", device.CoAPort)
	}
}

func TestResolver_RADIUSSecret_ImplementsSecretSource(t *testing.T) {
	keyStore, encryptor := testKeyStore(t)
	secretCT, err := encryptor.Encrypt("mikrotik-secret-123")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	store := &stubDeviceStore{
		devices: []DeviceRow{{IP: "203.0.113.4", Vendor: "mikrotik", RadiusSecretEncrypted: secretCT}},
	}
	r := NewResolver(store, keyStore, []byte("fallback"), 1700)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	addr := &net.UDPAddr{IP: net.ParseIP("203.0.113.4"), Port: 1812}
	secret, err := r.RADIUSSecret(context.Background(), addr)
	if err != nil {
		t.Fatalf("RADIUSSecret: %v", err)
	}
	if string(secret) != "mikrotik-secret-123" {
		t.Errorf("secret = %q, want the registered device's decrypted secret", secret)
	}

	// An IP with no registration falls back rather than erroring — a
	// malformed or unknown source must never make the packet server refuse
	// to even attempt verification.
	unknownAddr := &net.UDPAddr{IP: net.ParseIP("198.51.100.1"), Port: 1812}
	fallbackSecret, err := r.RADIUSSecret(context.Background(), unknownAddr)
	if err != nil {
		t.Fatalf("RADIUSSecret for unregistered IP: %v", err)
	}
	if string(fallbackSecret) != "fallback" {
		t.Errorf("secret = %q, want the global fallback", fallbackSecret)
	}
}

func TestResolver_Refresh_SkipsUndecryptableDeviceRatherThanFailingEntirely(t *testing.T) {
	keyStore, encryptor := testKeyStore(t)
	goodCT, err := encryptor.Encrypt("good-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	store := &stubDeviceStore{
		devices: []DeviceRow{
			{IP: "10.0.0.1", Vendor: "mikrotik", RadiusSecretEncrypted: "v99:not-valid-ciphertext"}, //nolint:gosec // test fixture, not a real credential
			{IP: "10.0.0.2", Vendor: "mikrotik", RadiusSecretEncrypted: goodCT},
		},
	}
	r := NewResolver(store, keyStore, []byte("fallback"), 1700)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh should not fail outright when one device's secret is bad: %v", err)
	}

	// The bad row falls back to the global default rather than vanishing
	// silently — still reachable, just on the safe default.
	bad := r.Resolve("10.0.0.1")
	if string(bad.Secret) != "fallback" {
		t.Errorf("undecryptable device secret = %q, want fallback", bad.Secret)
	}

	good := r.Resolve("10.0.0.2")
	if string(good.Secret) != "good-secret" {
		t.Errorf("good device secret = %q, want good-secret", good.Secret)
	}
}
