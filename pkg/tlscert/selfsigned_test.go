package tlscert

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestLoadOrGenerate_GeneratesAValidCertificate(t *testing.T) {
	dir := t.TempDir()

	cert, err := LoadOrGenerate(dir, []string{"localhost", "127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("generated certificate has no DER bytes")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse generated certificate: %v", err)
	}
	if err := leaf.VerifyHostname("localhost"); err != nil {
		t.Errorf("certificate does not verify for localhost: %v", err)
	}
	if !leaf.IPAddresses[0].Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("certificate IP SAN = %v, want 127.0.0.1", leaf.IPAddresses)
	}
	if leaf.NotAfter.Before(time.Now().Add(23 * time.Hour)) {
		t.Errorf("certificate NotAfter = %v, expected roughly 24h out", leaf.NotAfter)
	}
}

func TestLoadOrGenerate_PersistsFilesWithNarrowPermissions(t *testing.T) {
	// Windows/NTFS does not honor POSIX mode bits the way os.WriteFile's
	// argument implies — real access control there is a directory-ACL
	// concern for the installer, not something this assertion can observe.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file mode bits are not meaningful on Windows/NTFS")
	}

	dir := t.TempDir()

	if _, err := LoadOrGenerate(dir, []string{"localhost"}, time.Hour); err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}

	for _, name := range []string{certFileName, keyFileName} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s permissions = %v, want 0600", name, info.Mode().Perm())
		}
	}
}

func TestLoadOrGenerate_IsIdempotentAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrGenerate(dir, []string{"localhost"}, time.Hour)
	if err != nil {
		t.Fatalf("first LoadOrGenerate: %v", err)
	}
	second, err := LoadOrGenerate(dir, []string{"localhost"}, time.Hour)
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}

	if len(first.Certificate) == 0 || len(second.Certificate) == 0 {
		t.Fatal("missing certificate bytes")
	}
	firstLeaf, _ := x509.ParseCertificate(first.Certificate[0])
	secondLeaf, _ := x509.ParseCertificate(second.Certificate[0])
	if firstLeaf.SerialNumber.Cmp(secondLeaf.SerialNumber) != 0 {
		t.Error("second call generated a new certificate instead of reading the persisted one — " +
			"a restart would invalidate every client's trust decision")
	}
}

func TestLoadOrGenerate_CorruptExistingFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, certFileName), []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("seed corrupt cert file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, keyFileName), []byte("not a key"), 0o600); err != nil {
		t.Fatalf("seed corrupt key file: %v", err)
	}

	if _, err := LoadOrGenerate(dir, []string{"localhost"}, time.Hour); err == nil {
		t.Fatal("LoadOrGenerate must not silently regenerate over an unreadable existing pair")
	}
}

func TestLoadOrGenerate_ServesARealTLSHandshake(t *testing.T) {
	dir := t.TempDir()
	cert, err := LoadOrGenerate(dir, []string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	client := srv.Client()
	client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // test-only self-signed cert

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("TLS 1.3 request against the generated certificate failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.TLS.Version != tls.VersionTLS13 {
		t.Errorf("negotiated TLS version = %x, want TLS 1.3 (%x)", resp.TLS.Version, tls.VersionTLS13)
	}
}
