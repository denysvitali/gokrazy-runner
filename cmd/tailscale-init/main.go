// Command tailscale-init brings the device up on the user's tailnet on every
// boot. It is a one-shot service: gokrazy starts it, it execs `tailscale up`,
// and exits.
//
// Configuration is picked up from /perm so the runtime image stays
// secret-free. The auth key file path, hostname, and any extra `tailscale up`
// arguments are passed in via env vars set in the gokrazy PackageConfig:
//
//   - TS_AUTH_KEY_PATH      path to the auth key (default /perm/tailscale/authkey)
//   - TS_HOSTNAME           --hostname value passed to `tailscale up`
//   - TS_TAILSCALE_UP_ARGS  extra args (whitespace-separated) for `tailscale up`
//
// Re-running `tailscale up` on every boot is idempotent: tailscaled persists
// its state in /perm/tailscale (configured at the package level) and a
// reusable auth key just refreshes prefs. With a single-use key the second
// boot will fail to re-auth, but the persisted node state keeps the device
// connected.
package main

import (
	"log"
	"os"
	"os/exec"
	"strings"
)

const (
	defaultAuthKeyPath = "/perm/tailscale/authkey"
	defaultHostname    = "gokrazy-runner"
	tailscaleBinary    = "/user/tailscale"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	authKeyPath := getenv("TS_AUTH_KEY_PATH", defaultAuthKeyPath)
	hostname := getenv("TS_HOSTNAME", defaultHostname)
	extraArgs := strings.Fields(os.Getenv("TS_TAILSCALE_UP_ARGS"))

	// #nosec G304 -- authKeyPath is a controlled config path under /perm
	authKey, err := os.ReadFile(authKeyPath)
	if err != nil {
		log.Printf("tailscale-init: auth key path %q not readable, skipping Tailscale connect: %v", authKeyPath, err)
		return
	}

	key := strings.TrimSpace(string(authKey))
	if key == "" {
		log.Printf("tailscale-init: auth key file %q is empty, skipping Tailscale connect", authKeyPath)
		return
	}

	args := []string{"up", "--auth-key=" + key, "--hostname=" + hostname}
	args = append(args, extraArgs...)

	log.Printf("tailscale-init: running /user/tailscale up (hostname=%s)", hostname)
	// #nosec G204 -- tailscaleBinary is a const, args from trusted on-device config
	cmd := exec.Command(tailscaleBinary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("tailscale-init: tailscale up failed: %v", err)
	}
}

func getenv(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}
