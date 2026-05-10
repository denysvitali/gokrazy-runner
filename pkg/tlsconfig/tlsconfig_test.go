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

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.CertFile != "/etc/ssl/gokrazy-web.pem" {
		t.Errorf("CertFile = %q, want /etc/ssl/gokrazy-web.pem", cfg.CertFile)
	}
	if cfg.KeyFile != "/etc/ssl/gokrazy-web.key.pem" {
		t.Errorf("KeyFile = %q, want /etc/ssl/gokrazy-web.key.pem", cfg.KeyFile)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify must default to false")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS 1.2", cfg.MinVersion)
	}
}

func TestResolveConfigPrefersPermOverRoot(t *testing.T) {
	// Even when both pairs exist, perm wins — the rootfs cert is shared
	// across every device and must never take precedence.
	tmpDir := t.TempDir()
	setTestTLSPaths(t, filepath.Join(tmpDir, "root"), filepath.Join(tmpDir, "perm"))
	writeTestCertPair(t, rootCertFile, rootKeyFile)
	writeTestCertPair(t, permCertFile, permKeyFile)

	cfg := ResolveConfig()
	if cfg.CertFile != permCertFile {
		t.Errorf("CertFile = %q, want %q", cfg.CertFile, permCertFile)
	}
}

func TestResolveConfigFallsBackToRootWhenNoPermPair(t *testing.T) {
	tmpDir := t.TempDir()
	setTestTLSPaths(t, filepath.Join(tmpDir, "root"), filepath.Join(tmpDir, "perm"))
	writeTestCertPair(t, rootCertFile, rootKeyFile)

	cfg := ResolveConfig()
	if cfg.CertFile != rootCertFile {
		t.Errorf("CertFile = %q, want %q", cfg.CertFile, rootCertFile)
	}
}

func TestResolveConfigUsesPermAlone(t *testing.T) {
	tmpDir := t.TempDir()
	setTestTLSPaths(t, filepath.Join(tmpDir, "root"), filepath.Join(tmpDir, "perm"))
	writeTestCertPair(t, permCertFile, permKeyFile)

	cfg := ResolveConfig()
	if cfg.CertFile != permCertFile {
		t.Errorf("CertFile = %q, want %q", cfg.CertFile, permCertFile)
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

func TestGeneratePersistentSelfSignedCertificate(t *testing.T) {
	tmpDir := t.TempDir()
	setTestTLSPaths(t, filepath.Join(tmpDir, "root"), filepath.Join(tmpDir, "perm"))

	now := time.Date(2026, time.May, 6, 8, 0, 0, 0, time.UTC)
	info, err := generateSelfSignedCertificate(PersistentConfig(), []string{"runner.local", "192.168.1.10"}, now)
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}
	if info.CertFile != permCertFile {
		t.Fatalf("CertFile = %q, want %q", info.CertFile, permCertFile)
	}
	if !info.Exists || !info.ValidNow || info.NeedsRegeneration {
		t.Fatalf("info = %+v, want usable", info)
	}
	if info.NotAfter.Before(now.Add(11 * 30 * 24 * time.Hour)) {
		t.Fatalf("NotAfter %s is too soon", info.NotAfter)
	}
	if info.NotAfter.After(now.Add(366 * 24 * time.Hour)) {
		t.Fatalf("NotAfter %s is too far in the future", info.NotAfter)
	}
	if !containsString(info.DNSNames, "runner.local") {
		t.Fatalf("DNSNames = %v, want runner.local", info.DNSNames)
	}
	if !containsString(info.IPAddresses, "192.168.1.10") {
		t.Fatalf("IPAddresses = %v, want 192.168.1.10", info.IPAddresses)
	}
	keyInfo, err := os.Stat(permKeyFile)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	if keyInfo.Mode().Perm() != 0600 {
		t.Fatalf("key mode = %v, want 0600", keyInfo.Mode().Perm())
	}
}

func TestEnsurePersistentSelfSignedCertificateRegeneratesNearExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	setTestTLSPaths(t, filepath.Join(tmpDir, "root"), filepath.Join(tmpDir, "perm"))

	// Write a cert that expires in 5 days (well within the 30-day renew window).
	if err := os.MkdirAll(filepath.Dir(permCertFile), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(5 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(permCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(permKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	info, regenerated, err := EnsurePersistentSelfSignedCertificate(nil)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if !regenerated {
		t.Fatalf("expected regeneration; info=%+v", info)
	}
	if info.NeedsRegeneration {
		t.Fatalf("regenerated cert should not need regeneration: %+v", info)
	}
}

func TestEnsurePersistentSelfSignedCertificateNoOpWhenFresh(t *testing.T) {
	tmpDir := t.TempDir()
	setTestTLSPaths(t, filepath.Join(tmpDir, "root"), filepath.Join(tmpDir, "perm"))

	first, _, err := EnsurePersistentSelfSignedCertificate(nil)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	second, regenerated, err := EnsurePersistentSelfSignedCertificate(nil)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if regenerated {
		t.Fatal("second call should not regenerate a fresh cert")
	}
	if first.FingerprintSHA256 != second.FingerprintSHA256 {
		t.Fatalf("fingerprint changed across no-op ensure: %s vs %s", first.FingerprintSHA256, second.FingerprintSHA256)
	}
}

func TestGeneratePersistentSelfSignedCertificateRejectsInvalidClock(t *testing.T) {
	tmpDir := t.TempDir()
	setTestTLSPaths(t, filepath.Join(tmpDir, "root"), filepath.Join(tmpDir, "perm"))

	_, err := generateSelfSignedCertificate(PersistentConfig(), nil, time.Date(1980, time.January, 1, 0, 0, 3, 0, time.UTC))
	if err == nil {
		t.Fatal("expected invalid clock error")
	}
	if fileExists(permCertFile) || fileExists(permKeyFile) {
		t.Fatal("certificate files should not be written when clock is invalid")
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

func TestCurrentTimeCanIssueCertificate(t *testing.T) {
	if CurrentTimeCanIssueCertificate(time.Time{}) {
		t.Error("zero time must not be acceptable")
	}
	if CurrentTimeCanIssueCertificate(time.Date(1970, time.January, 1, 0, 0, 1, 0, time.UTC)) {
		t.Error("epoch time must not be acceptable")
	}
	if !CurrentTimeCanIssueCertificate(time.Date(2025, time.June, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("post-2024 time must be acceptable")
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func setTestTLSPaths(t *testing.T, rootDir, permDir string) {
	t.Helper()
	origRootCert := rootCertFile
	origRootKey := rootKeyFile
	origPermCert := permCertFile
	origPermKey := permKeyFile

	rootCertFile = filepath.Join(rootDir, "ssl", "gokrazy-web.pem")
	rootKeyFile = filepath.Join(rootDir, "ssl", "gokrazy-web.key.pem")
	permCertFile = filepath.Join(permDir, "ssl", "gokrazy-web.pem")
	permKeyFile = filepath.Join(permDir, "ssl", "gokrazy-web.key.pem")

	t.Cleanup(func() {
		rootCertFile = origRootCert
		rootKeyFile = origRootKey
		permCertFile = origPermCert
		permKeyFile = origPermKey
	})
}

func writeTestCertPair(t *testing.T, certPath, keyPath string) {
	t.Helper()
	writeTestFile(t, certPath)
	writeTestFile(t, keyPath)
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
