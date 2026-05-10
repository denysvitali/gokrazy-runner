package tlsconfig

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func generateTestCertificate(certPath, keyPath string) error {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate private key: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Test gokrazy-runner"},
			CommonName:   "localhost",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	certOut, err := os.Create(certPath)
	if err != nil {
		return fmt.Errorf("create cert file: %w", err)
	}
	defer certOut.Close()
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}

	keyOut, err := os.Create(keyPath)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	defer keyOut.Close()
	privBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("marshal private key: %w", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

func TestResolveConfigPointsAtGokrazyPermPair(t *testing.T) {
	cfg := ResolveConfig()
	if cfg.CertFile != "/perm/ssl/gokrazy-web.pem" {
		t.Errorf("CertFile = %q, want /perm/ssl/gokrazy-web.pem", cfg.CertFile)
	}
	if cfg.KeyFile != "/perm/ssl/gokrazy-web.key.pem" {
		t.Errorf("KeyFile = %q, want /perm/ssl/gokrazy-web.key.pem", cfg.KeyFile)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS 1.2", cfg.MinVersion)
	}
}

func TestCertificatesExist(t *testing.T) {
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "test.pem")
	keyPath := filepath.Join(tmpDir, "test.key.pem")
	cfg := &Config{CertFile: certPath, KeyFile: keyPath}

	if cfg.CertificatesExist() {
		t.Fatal("CertificatesExist must be false when files do not exist")
	}
	if err := generateTestCertificate(certPath, keyPath); err != nil {
		t.Fatalf("generate test cert: %v", err)
	}
	if !cfg.CertificatesExist() {
		t.Fatal("CertificatesExist must be true after writing both files")
	}
	os.Remove(keyPath)
	if cfg.CertificatesExist() {
		t.Fatal("CertificatesExist must be false when key is missing")
	}
}

func TestNewTLSConfigFromGeneratedPair(t *testing.T) {
	tmpDir := t.TempDir()
	certPath := filepath.Join(tmpDir, "test.pem")
	keyPath := filepath.Join(tmpDir, "test.key.pem")
	if err := generateTestCertificate(certPath, keyPath); err != nil {
		t.Fatalf("generate: %v", err)
	}
	cfg := &Config{CertFile: certPath, KeyFile: keyPath, MinVersion: tls.VersionTLS13}
	tlsConfig, err := cfg.NewTLSConfig()
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}
	if tlsConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %d, want TLS 1.3", tlsConfig.MinVersion)
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Errorf("Certificates len = %d, want 1", len(tlsConfig.Certificates))
	}
}

func TestNewTLSConfigMissingCertReturnsError(t *testing.T) {
	cfg := &Config{CertFile: "/nonexistent/cert.pem", KeyFile: "/nonexistent/key.pem"}
	if _, err := cfg.NewTLSConfig(); err == nil {
		t.Fatal("expected error for missing cert")
	}
}
