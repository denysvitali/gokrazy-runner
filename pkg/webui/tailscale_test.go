package webui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateTailscaleAuthKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"empty", "", true},
		{"missing prefix", "not-an-auth-key", true},
		{"plain reusable", "tskey-auth-abc123def456", false},
		{"leading whitespace", " tskey-auth-abc", true},
		{"contains newline", "tskey-auth-abc\n", true},
		{"contains space", "tskey-auth-ab cd", true},
		{"too long", "tskey-auth-" + strings.Repeat("a", MaxTailscaleAuthKeyLength), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTailscaleAuthKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateTailscaleAuthKey(%q) = %v, wantErr=%v", tt.key, err, tt.wantErr)
			}
		})
	}
}

func TestSaveTailscaleAuthKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tailscale", "authkey")
	key := "tskey-auth-test-123abc"

	if err := SaveTailscaleAuthKey(path, key); err != nil {
		t.Fatalf("SaveTailscaleAuthKey: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != key+"\n" {
		t.Fatalf("stored = %q, want %q", string(data), key+"\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 0600", got)
	}
}

func TestSaveTailscaleAuthKeyRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tailscale", "authkey")
	if err := SaveTailscaleAuthKey(path, "junk"); err == nil {
		t.Fatal("SaveTailscaleAuthKey accepted invalid key")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not exist on validation failure: %v", err)
	}
}

func TestHasTailscaleAuthKey(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing")
	if ok, err := HasTailscaleAuthKey(missing); err != nil || ok {
		t.Fatalf("missing: ok=%v err=%v", ok, err)
	}

	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n  \n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if ok, err := HasTailscaleAuthKey(empty); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}

	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("tskey-auth-abc\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if ok, err := HasTailscaleAuthKey(good); err != nil || !ok {
		t.Fatalf("good: ok=%v err=%v", ok, err)
	}
}

func TestConfigureTailscaleRedactsKeyOnFailure(t *testing.T) {
	orig := tailscaleCommandContext
	t.Cleanup(func() { tailscaleCommandContext = orig })

	const key = "tskey-auth-secret-xyz"
	tailscaleCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// /bin/sh -c '...' that prints the joined args (which include the key)
		// to stderr and exits 1, so we can verify ConfigureTailscale redacts it.
		joined := strings.Join(args, " ")
		return exec.CommandContext(ctx, "/bin/sh", "-c", "echo \"failure: "+joined+"\" >&2; exit 1")
	}

	path := filepath.Join(t.TempDir(), "tailscale", "authkey")
	err := ConfigureTailscale(context.Background(), path, key)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("error leaks auth key: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error does not contain [redacted]: %v", err)
	}

	// Auth key must have been persisted before the up attempt.
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if strings.TrimSpace(string(data)) != key {
		t.Fatalf("persisted key = %q, want %q", strings.TrimSpace(string(data)), key)
	}
}
