// Package dnsfallback seeds resolv.conf with public resolvers when
// the file is missing or has no usable nameserver entries.
//
// On gokrazy DNS comes from DHCP. When the upstream router doesn't hand
// out a DNS server (IPv6-only networks, captive portals, misconfigured
// DHCP), resolv.conf stays empty and Go's resolver falls back to
// its hardcoded defaults of 127.0.0.1:53 and [::1]:53 — neither of
// which is listening — and every lookup fails with ECONNREFUSED. The
// matching error inside containers running with --network=host is
// EAI_AGAIN ("Resource temporarily unavailable").
//
// On gokrazy the canonical DNS file is /tmp/resolv.conf: /etc/resolv.conf
// is a baked-in symlink to /tmp/resolv.conf, and /tmp/resolv.conf is a
// symlink to /proc/net/pnp until gokrazy/dhcp replaces it via renameio
// with a real file. Writing through the symlink chain via os.WriteFile
// follows down to /proc/net/pnp, which is read-only for userspace and
// returns EIO. Ensure therefore writes atomically by creating a temp
// file alongside the target and renaming over it — that replaces the
// symlink with a real file, exactly like gokrazy/dhcp does.
//
// Ensure runs once at process start. It never overwrites a populated
// resolv.conf, so DHCP- or Tailscale-supplied nameservers always win.
package dnsfallback

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Action describes what Ensure did.
type Action int

const (
	// ActionUnchanged means resolv.conf already had at least one
	// nameserver and was left alone.
	ActionUnchanged Action = iota
	// ActionWrote means the file was missing/empty and the fallback
	// was written.
	ActionWrote
)

// DefaultNameservers is the fallback list used by callers that don't
// want to pick their own. Cloudflare + Quad9: both run anycast,
// support DoT/DoH if needed downstream, and are reachable from most
// consumer ISPs.
var DefaultNameservers = []string{"1.1.1.1", "9.9.9.9"}

// Ensure makes sure path contains at least one usable nameserver. If
// it already does, the file is left unchanged. If it is missing,
// empty, or has only comments, the fallback nameservers are written.
func Ensure(path string, nameservers []string) (Action, error) {
	if path == "" {
		return ActionUnchanged, errors.New("dnsfallback: empty path")
	}
	if len(nameservers) == 0 {
		return ActionUnchanged, errors.New("dnsfallback: no nameservers provided")
	}

	raw, err := os.ReadFile(path) // #nosec G304 -- caller-controlled path
	switch {
	case err == nil:
		if hasNameserver(raw) {
			return ActionUnchanged, nil
		}
	case errors.Is(err, os.ErrNotExist):
		// fall through and write
	default:
		return ActionUnchanged, fmt.Errorf("read %s: %w", path, err)
	}

	body := render(nameservers)
	if err := writeAtomic(path, body); err != nil {
		return ActionUnchanged, fmt.Errorf("write %s: %w", path, err)
	}
	return ActionWrote, nil
}

// writeAtomic writes body to a temp file in path's directory and renames
// it over path. This replaces a symlink at path with a real file, which
// is what we want on gokrazy where /tmp/resolv.conf (and through it
// /etc/resolv.conf) starts out as a symlink to /proc/net/pnp.
func writeAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".resolv.conf.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil { // #nosec G302 -- world-readable matches stock resolv.conf
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func render(nameservers []string) []byte {
	var b strings.Builder
	b.WriteString("# Fallback resolvers seeded by gokrazy-runner.\n")
	b.WriteString("# DHCP or Tailscale may overwrite this file at runtime.\n")
	for _, ns := range nameservers {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		fmt.Fprintf(&b, "nameserver %s\n", ns)
	}
	return []byte(b.String())
}

func hasNameserver(b []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "nameserver") && fields[1] != "" {
			return true
		}
	}
	return false
}
