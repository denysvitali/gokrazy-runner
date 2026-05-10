// Package tlsconfig manages a per-device self-signed TLS certificate for the
// runner-webui server.
//
// The image baked at /etc/ssl/gokrazy-web.{pem,key.pem} is shared by every
// device built from the same OTA artifact and therefore unsuitable as a
// long-lived TLS identity. This package generates a fresh ECDSA P-256
// certificate on first boot, persists it under /perm/ssl/, and renews it
// automatically before expiry. ResolveConfig always prefers the persistent
// /perm pair over the shared rootfs pair when both are present.
package tlsconfig

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	certificateValidity    = 365 * 24 * time.Hour
	certificateBackdate    = 5 * time.Minute
	certificateRenewWithin = 30 * 24 * time.Hour
)

var (
	rootCertFile                      = "/etc/ssl/gokrazy-web.pem"
	rootKeyFile                       = "/etc/ssl/gokrazy-web.key.pem"
	permCertFile                      = "/perm/ssl/gokrazy-web.pem"
	permKeyFile                       = "/perm/ssl/gokrazy-web.key.pem"
	earliestReasonableCertificateTime = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
)

// Config describes a TLS certificate pair on disk.
type Config struct {
	CertFile           string
	KeyFile            string
	InsecureSkipVerify bool
	MinVersion         uint16
}

// CertificateInfo describes the certificate currently stored at a given pair
// of paths.
type CertificateInfo struct {
	CertFile          string    `json:"cert_file"`
	KeyFile           string    `json:"key_file"`
	Exists            bool      `json:"exists"`
	ValidNow          bool      `json:"valid_now"`
	NeedsRegeneration bool      `json:"needs_regeneration"`
	NotBefore         time.Time `json:"not_before,omitempty"`
	NotAfter          time.Time `json:"not_after,omitempty"`
	CommonName        string    `json:"common_name,omitempty"`
	DNSNames          []string  `json:"dns_names,omitempty"`
	IPAddresses       []string  `json:"ip_addresses,omitempty"`
	FingerprintSHA256 string    `json:"fingerprint_sha256,omitempty"`
	Error             string    `json:"error,omitempty"`
}

// DefaultConfig returns a TLS config pointing at the rootfs cert pair, with
// secure defaults applied.
func DefaultConfig() *Config {
	return &Config{
		CertFile:           rootCertFile,
		KeyFile:            rootKeyFile,
		InsecureSkipVerify: false,
		MinVersion:         tls.VersionTLS12,
	}
}

// PersistentConfig returns a TLS config pointing at the /perm cert pair.
func PersistentConfig() *Config {
	cfg := DefaultConfig()
	return &Config{
		CertFile:           permCertFile,
		KeyFile:            permKeyFile,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		MinVersion:         cfg.MinVersion,
	}
}

// ResolveConfig returns the cert pair that should be used right now.
//
// The persistent /perm pair is always preferred over the rootfs pair: the
// rootfs pair is identical on every device built from the same image and
// must never be used once a per-device cert exists. The rootfs pair only
// serves as a fallback for the very first boot before the perm cert has
// been generated.
func ResolveConfig() *Config {
	cfg := DefaultConfig()
	root := Config{
		CertFile:           rootCertFile,
		KeyFile:            rootKeyFile,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		MinVersion:         cfg.MinVersion,
	}
	perm := Config{
		CertFile:           permCertFile,
		KeyFile:            permKeyFile,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		MinVersion:         cfg.MinVersion,
	}

	if perm.CertificatesExist() {
		return &perm
	}
	if root.CertificatesExist() {
		return &root
	}
	return cfg
}

// NewTLSConfig builds a *tls.Config from the on-disk certificate pair.
func (c *Config) NewTLSConfig() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, err
	}

	minVersion := c.MinVersion
	if minVersion == 0 {
		minVersion = tls.VersionTLS12
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
		ClientAuth:   tls.NoClientCert,
	}

	if !c.InsecureSkipVerify {
		certPool, err := x509.SystemCertPool()
		if err != nil {
			log.Printf("Warning: Failed to load system cert pool: %v", err)
			certPool = x509.NewCertPool()
		}
		tlsConfig.RootCAs = certPool
	}

	return tlsConfig, nil
}

// CertificatesExist reports whether both files in the pair exist on disk.
func (c *Config) CertificatesExist() bool {
	return fileExists(c.CertFile) && fileExists(c.KeyFile)
}

func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	return true
}

// CurrentTimeCanIssueCertificate guards against persisting certificates
// generated at gokrazy's pre-NTP placeholder clock value.
func CurrentTimeCanIssueCertificate(now time.Time) bool {
	return !now.IsZero() && !now.Before(earliestReasonableCertificateTime)
}

// PersistentCertificateInfo inspects the certificate pair stored under /perm.
func PersistentCertificateInfo(now time.Time) (*CertificateInfo, error) {
	return inspectCertificate(PersistentConfig(), now)
}

// EnsurePersistentSelfSignedCertificate generates the /perm certificate pair
// when missing, expired, or close to expiry. extraHosts are added as SANs in
// addition to localhost, the device hostname, and detected interface IPs.
func EnsurePersistentSelfSignedCertificate(extraHosts []string) (*CertificateInfo, bool, error) {
	now := time.Now()
	info, err := PersistentCertificateInfo(now)
	if err == nil && info.Exists && !info.NeedsRegeneration {
		return info, false, nil
	}

	generated, genErr := GeneratePersistentSelfSignedCertificate(extraHosts)
	if genErr != nil {
		return info, false, genErr
	}
	return generated, true, nil
}

// GeneratePersistentSelfSignedCertificate writes a new self-signed cert pair
// to /perm/ssl, overwriting any existing files.
func GeneratePersistentSelfSignedCertificate(extraHosts []string) (*CertificateInfo, error) {
	return generateSelfSignedCertificate(PersistentConfig(), extraHosts, time.Now())
}

func generateSelfSignedCertificate(cfg *Config, extraHosts []string, now time.Time) (*CertificateInfo, error) {
	if !CurrentTimeCanIssueCertificate(now) {
		return nil, fmt.Errorf("system time %s is too early to issue a persistent certificate", now.UTC().Format(time.RFC3339))
	}

	dnsNames, ipAddresses := certificateNames(extraHosts)
	commonName := "gokrazy-runner"
	if len(dnsNames) > 0 {
		commonName = dnsNames[0]
	}

	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate private key: %w", err)
	}

	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, fmt.Errorf("generate serial number: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"gokrazy-runner"},
			CommonName:   commonName,
		},
		NotBefore:             now.Add(-certificateBackdate).UTC(),
		NotAfter:              now.Add(certificateValidity).UTC(),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	privateKeyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.CertFile), 0700); err != nil {
		return nil, fmt.Errorf("create certificate directory: %w", err)
	}
	if err := writePEMFileAtomic(cfg.KeyFile, 0600, &pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyBytes}); err != nil {
		return nil, fmt.Errorf("write private key: %w", err)
	}
	if err := writePEMFileAtomic(cfg.CertFile, 0644, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return nil, fmt.Errorf("write certificate: %w", err)
	}

	return inspectCertificate(cfg, now)
}

func inspectCertificate(cfg *Config, now time.Time) (*CertificateInfo, error) {
	info := &CertificateInfo{
		CertFile: cfg.CertFile,
		KeyFile:  cfg.KeyFile,
		Exists:   cfg.CertificatesExist(),
	}
	if !info.Exists {
		info.NeedsRegeneration = true
		return info, nil
	}

	certPEM, err := os.ReadFile(cfg.CertFile)
	if err != nil {
		info.Error = err.Error()
		info.NeedsRegeneration = true
		return info, err
	}

	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		err := fmt.Errorf("certificate file does not contain a PEM certificate")
		info.Error = err.Error()
		info.NeedsRegeneration = true
		return info, err
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		info.Error = err.Error()
		info.NeedsRegeneration = true
		return info, err
	}

	fingerprint := x509SHA256Fingerprint(cert.Raw)
	ipAddresses := make([]string, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		ipAddresses = append(ipAddresses, ip.String())
	}
	sort.Strings(ipAddresses)

	info.NotBefore = cert.NotBefore
	info.NotAfter = cert.NotAfter
	info.CommonName = cert.Subject.CommonName
	info.DNSNames = append([]string(nil), cert.DNSNames...)
	info.IPAddresses = ipAddresses
	info.FingerprintSHA256 = fingerprint
	info.ValidNow = !now.Before(cert.NotBefore) && !now.After(cert.NotAfter)
	info.NeedsRegeneration = !info.ValidNow || cert.NotAfter.Sub(now) <= certificateRenewWithin

	if _, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile); err != nil {
		info.Error = err.Error()
		info.ValidNow = false
		info.NeedsRegeneration = true
		return info, err
	}

	return info, nil
}

func writePEMFileAtomic(path string, mode os.FileMode, block *pem.Block) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmpFile.Chmod(mode); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := pem.Encode(tmpFile, block); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	renamed = true
	return nil
}

func certificateNames(extraHosts []string) ([]string, []net.IP) {
	dnsSet := map[string]struct{}{}
	ipSet := map[string]net.IP{}

	addHost := func(host string) {
		host = normalizeCertificateHost(host)
		if host == "" {
			return
		}
		if ip := net.ParseIP(host); ip != nil {
			ipSet[ip.String()] = ip
			return
		}
		dnsSet[host] = struct{}{}
	}

	addHost("localhost")
	addHost("gokrazy-runner")
	addHost("gokrazy-runner.local")
	if hostname, err := os.Hostname(); err == nil {
		addHost(hostname)
		if hostname != "" && !strings.HasSuffix(hostname, ".local") {
			addHost(hostname + ".local")
		}
	}
	for _, host := range extraHosts {
		addHost(host)
	}

	for _, ip := range interfaceIPAddresses() {
		ipSet[ip.String()] = ip
	}

	dnsNames := make([]string, 0, len(dnsSet))
	for name := range dnsSet {
		dnsNames = append(dnsNames, name)
	}
	sort.Strings(dnsNames)

	ipKeys := make([]string, 0, len(ipSet))
	for key := range ipSet {
		ipKeys = append(ipKeys, key)
	}
	sort.Strings(ipKeys)

	ipAddresses := make([]net.IP, 0, len(ipKeys))
	for _, key := range ipKeys {
		ipAddresses = append(ipAddresses, ipSet[key])
	}

	return dnsNames, ipAddresses
}

func normalizeCertificateHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" {
		return ""
	}

	if strings.Contains(host, "://") {
		if parsed, err := url.Parse(host); err == nil {
			host = parsed.Host
		}
	}

	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	host = strings.Trim(host, "[]")
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return ""
	}
	if strings.ContainsAny(host, "/\\ \t\r\n") {
		return ""
	}
	return host
}

func interfaceIPAddresses() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}

	ips := make([]net.IP, 0, len(addrs)+2)
	ips = append(ips, net.ParseIP("127.0.0.1"), net.ParseIP("::1"))
	for _, addr := range addrs {
		var ip net.IP
		switch value := addr.(type) {
		case *net.IPNet:
			ip = value.IP
		case *net.IPAddr:
			ip = value.IP
		}
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		ips = append(ips, ip)
	}
	return ips
}

func x509SHA256Fingerprint(raw []byte) string {
	digest := sha256Sum(raw)
	encoded := strings.ToUpper(hex.EncodeToString(digest))
	parts := make([]string, 0, len(encoded)/2)
	for i := 0; i < len(encoded); i += 2 {
		parts = append(parts, encoded[i:i+2])
	}
	return strings.Join(parts, ":")
}

func sha256Sum(raw []byte) []byte {
	digest := sha256.Sum256(raw)
	return digest[:]
}

// LoadOrDefault resolves the active cert pair, validates it, and returns the
// matching *tls.Config. When no usable cert exists the caller should fall
// back to plain HTTP.
func LoadOrDefault() (*tls.Config, bool, error) {
	cfg := ResolveConfig()
	now := time.Now()

	if !CurrentTimeCanIssueCertificate(now) {
		log.Printf("System time %s is too early for reliable TLS certificates", now.UTC().Format(time.RFC3339))
		return nil, false, nil
	}

	if !cfg.CertificatesExist() {
		log.Printf("TLS certificates not found at %s and %s", cfg.CertFile, cfg.KeyFile)
		return nil, false, nil
	}

	info, err := inspectCertificate(cfg, now)
	if err != nil || !info.ValidNow {
		if err != nil {
			log.Printf("TLS certificate at %s is not usable: %v", cfg.CertFile, err)
		} else {
			log.Printf("TLS certificate at %s is not valid for current time %s", cfg.CertFile, now.UTC().Format(time.RFC3339))
		}
		return nil, false, nil
	}

	tlsConfig, err := cfg.NewTLSConfig()
	if err != nil {
		log.Printf("Failed to load TLS configuration: %v", err)
		return nil, false, err
	}

	log.Printf("TLS enabled with certificates from %s", cfg.CertFile)
	return tlsConfig, true, nil
}
