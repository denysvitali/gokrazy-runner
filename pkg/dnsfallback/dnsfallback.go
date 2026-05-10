// Package dnsfallback seeds /etc/resolv.conf with public resolvers when
// the file is missing or has no usable nameserver entries.
//
// On gokrazy DNS comes from DHCP. When the upstream router doesn't hand
// out a DNS server (IPv6-only networks, captive portals, misconfigured
// DHCP), /etc/resolv.conf stays empty and Go's resolver falls back to
// its hardcoded defaults of 127.0.0.1:53 and [::1]:53 — neither of
// which is listening — and every lookup fails with ECONNREFUSED. The
// matching error inside containers running with --network=host is
// EAI_AGAIN ("Resource temporarily unavailable").
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
	if err := os.WriteFile(path, body, 0o644); err != nil { // #nosec G306 -- world-readable matches stock /etc/resolv.conf
		return ActionUnchanged, fmt.Errorf("write %s: %w", path, err)
	}
	return ActionWrote, nil
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
