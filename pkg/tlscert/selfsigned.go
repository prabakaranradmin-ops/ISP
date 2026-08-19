// Package tlscert generates and persists a self-signed TLS certificate for
// the API service to terminate TLS itself.
//
// This stack used to put a Caddy reverse proxy in front of api_service for
// TLS termination (config/caddy/Caddyfile), which was the only externally
// reachable HTTP(S) entry point. A single-machine native install has no
// separate proxy container to run, and Go's own net/http already supports
// serving TLS directly — this package is what api_service now uses instead.
// There is no ACME/Let's Encrypt path here (the Caddyfile's public-FQDN case
// does not apply to a LAN-facing on-prem install), so the certificate is
// self-signed, generated once and reused across restarts.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	certFileName = "server.crt"
	keyFileName  = "server.key"
)

// LoadOrGenerate returns a tls.Certificate for the given hosts, reading it
// back from dir if server.crt/server.key already exist there, or generating
// and persisting a new self-signed pair if not.
//
// An existing key pair is left alone — mirroring the same idempotency
// pkg/crypto's key store depends on — so a service restart does not
// invalidate every client that has already accepted the certificate's
// fingerprint (a self-signed cert has no CA a restart could re-derive trust
// from, so silently rotating it on every start would retrigger a manual
// trust decision each time).
func LoadOrGenerate(dir string, hosts []string, validity time.Duration) (tls.Certificate, error) {
	certPath := filepath.Join(dir, certFileName)
	keyPath := filepath.Join(dir, keyFileName)

	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return cert, nil
	} else if !os.IsNotExist(err) {
		// A key pair that exists but fails to load (corrupt, wrong permissions,
		// mismatched cert/key) is a real problem — generating a fresh one
		// silently would mask it.
		return tls.Certificate{}, fmt.Errorf("tlscert: load existing certificate: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: create cert directory: %w", err)
	}

	certPEM, keyPEM, err := generate(hosts, validity)
	if err != nil {
		return tls.Certificate{}, err
	}

	// Key material first, narrowly permissioned, before the cert: the
	// reverse order would leave a moment where the cert exists without its
	// key, which is a more confusing failure mode to recover from by hand.
	// The 0600 mode is meaningful on the Unix dev/CI path; on the Windows
	// install target, NTFS does not honor POSIX mode bits the same way, so
	// real access control there is a directory-ACL concern for the MSI
	// installer (it installs under Program Files, not a world-writable
	// path), not something this call can enforce on its own.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: write key file: %w", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: write cert file: %w", err)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: load generated certificate: %w", err)
	}
	return cert, nil
}

// generate builds a self-signed ECDSA P-256 certificate valid for hosts.
func generate(hosts []string, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"ISP BSS/OSS (self-signed, local install)"}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, // self-signed leaf acting as its own issuer
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: create certificate: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: marshal private key: %w", err)
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
