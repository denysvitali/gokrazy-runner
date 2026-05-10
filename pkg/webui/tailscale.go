package webui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// TailscaleAuthKeyFile is the on-device path that cmd/tailscale-init reads
	// at boot to register with the tailnet. Must match TS_AUTH_KEY_PATH in the
	// gokrazy PackageConfig for tailscale-init.
	//
	// Kept as a flat file in /perm/ rather than under /perm/tailscale/ because
	// gokrazy bind-mounts tailscaled's -statedir read-only into other services'
	// namespaces, so the webui can't write inside /perm/tailscale/.
	TailscaleAuthKeyFile = "/perm/tailscale.authkey"

	// MaxTailscaleAuthKeyLength is a generous upper bound for tskey-* values.
	MaxTailscaleAuthKeyLength = 512

	// tailscaleBinary is the on-device path to the tailscale CLI built by
	// gokrazy from tailscale.com/cmd/tailscale.
	tailscaleBinary = "/user/tailscale"
)

// tailscaleCommandContext is overridden in tests.
var tailscaleCommandContext = exec.CommandContext

// ValidateTailscaleAuthKey rejects obviously malformed values before they
// hit /perm or the tailscale CLI.
func ValidateTailscaleAuthKey(authKey string) error {
	if authKey == "" {
		return errors.New("tailscale auth key cannot be empty")
	}
	if strings.TrimSpace(authKey) != authKey {
		return errors.New("tailscale auth key cannot have leading or trailing whitespace")
	}
	if len(authKey) > MaxTailscaleAuthKeyLength {
		return fmt.Errorf("tailscale auth key exceeds maximum length of %d characters", MaxTailscaleAuthKeyLength)
	}
	if strings.ContainsAny(authKey, "\x00\r\n\t ") {
		return errors.New("tailscale auth key contains invalid whitespace or control characters")
	}
	if !strings.HasPrefix(authKey, "tskey-auth-") {
		return errors.New("tailscale auth key must start with tskey-auth-")
	}
	return nil
}

// SaveTailscaleAuthKey persists the auth key in /perm. The file is written
// atomically with mode 0600 so it survives a power-cut mid-write.
func SaveTailscaleAuthKey(path, authKey string) error {
	if err := ValidateTailscaleAuthKey(authKey); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(authKey+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}

// HasTailscaleAuthKey reports whether a non-empty key is stored at path.
func HasTailscaleAuthKey(path string) (bool, error) {
	// #nosec G304 -- caller-controlled path under /perm
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return len(bytes.TrimSpace(data)) > 0, nil
}

// ConfigureTailscale validates the key, persists it, and runs `tailscale up`
// so the device joins the tailnet without waiting for the next reboot.
// On failure the auth key in stdout/stderr is replaced with [redacted].
func ConfigureTailscale(ctx context.Context, path, authKey string) error {
	if err := SaveTailscaleAuthKey(path, authKey); err != nil {
		return fmt.Errorf("save tailscale auth key: %w", err)
	}

	upCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{"up", "--auth-key=" + authKey, "--ssh"}
	if hostname, err := os.Hostname(); err == nil {
		hostname = strings.TrimSpace(hostname)
		if hostname != "" {
			args = append(args, "--hostname="+hostname)
		}
	}

	// #nosec G204 -- tailscaleBinary is a const, args from validated input
	output, err := tailscaleCommandContext(upCtx, tailscaleBinary, args...).CombinedOutput()
	if err != nil {
		details := strings.ReplaceAll(strings.TrimSpace(string(output)), authKey, "[redacted]")
		if details == "" {
			details = err.Error()
		}
		return fmt.Errorf("tailscale up: %s", details)
	}
	return nil
}
