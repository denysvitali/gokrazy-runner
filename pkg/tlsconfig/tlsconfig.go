// Package tlsconfig loads the per-device self-signed TLS certificate that
// gokrazy generates under /perm/ssl/ when the image is built with
// `Update.TLSCertificateStorage = "perm-self-signed"`.
//
// We deliberately do not generate or rotate this cert ourselves: gokrazy's
// own setupTLS already writes /perm/ssl/gokrazy-web.{pem,key.pem} on first
// boot and reuses it forever after. Two writers on the same files raced and
// caused the cert to flip between gokrazy's 10-year EC cert and our 1-year
// PKCS8 cert across reboots.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"log"
	"os"
)

// Paths managed by gokrazy itself (see github.com/gokrazy/gokrazy/tls.go).
// Kept as vars so tests can override.
var (
	permCertFile = "/perm/ssl/gokrazy-web.pem"
	permKeyFile  = "/perm/ssl/gokrazy-web.key.pem"
)

// Config describes a TLS certificate pair on disk.
type Config struct {
	CertFile           string
	KeyFile            string
	InsecureSkipVerify bool
	MinVersion         uint16
}

// ResolveConfig returns the cert pair runner-webui should serve. It always
// points at the gokrazy-managed /perm pair; callers must check
// CertificatesExist before relying on the paths.
func ResolveConfig() *Config {
	return &Config{
		CertFile:   permCertFile,
		KeyFile:    permKeyFile,
		MinVersion: tls.VersionTLS12,
	}
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
	_, err := os.Stat(path)
	return err == nil
}
