// Command runner-init drives a containerised GitHub Actions self-hosted
// runner on a gokrazy appliance.
//
// It reads its configuration from /perm:
//
//   - /perm/runner.env     KEY=VALUE pairs (URL, NAME, LABELS, IMAGE, ...)
//   - /perm/runner.token   one-shot GitHub registration token (chmod 0600)
//
// On boot it waits for /perm to be available, pulls the runner image, refreshes
// the persisted runner binaries from it, and runs the official
// ghcr.io/actions/actions-runner container via
// `podman run`. The official image's entrypoint is overridden with a small
// bash bootstrap that runs config.sh on first boot (passing the registration
// token) and then run.sh; on subsequent boots the persisted .runner config
// in /perm/runner-data lets us skip the registration step entirely.
//
// /perm/runner-data is mounted as the container's /home/runner, so the
// runner's identity, _work directory, and any persistent caches survive
// reboots.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/denysvitali/gokrazy-runner/pkg/dnsfallback"
)

const (
	permDir       = "/perm"
	envFile       = "/perm/runner.env"
	tokenFile     = "/perm/runner.token"
	dataDir       = "/perm/runner-data"
	containerName = "gokrazy-runner"

	// defaultImage is the official self-hosted runner image published by the
	// actions team. Override with IMAGE= in /perm/runner.env.
	defaultImage = "ghcr.io/actions/actions-runner:latest"

	// containerHome is where the official image keeps the runner binaries
	// (config.sh, run.sh) and where the .runner / _work state ends up.
	containerHome = "/home/runner"

	// runnerUID/runnerGID match the `runner` user baked into the official
	// ghcr.io/actions/actions-runner image. The container is privileged with
	// no user-namespace remap, so host and container UIDs are identical;
	// /perm/runner-data must be owned by 1001:1001 for the runner user to
	// be able to cd into the bind-mounted /home/runner.
	runnerUID = 1001
	runnerGID = 1001

	podmanBinary = "/user/podman"

	backoffMin = 5 * time.Second
	backoffMax = 2 * time.Minute
)

// bootstrap is the in-container entrypoint we feed to `bash -c`. It registers
// the runner on first boot (when no .runner config is present) and then
// hands off to the official run.sh. On subsequent boots the .runner file
// persisted in /perm/runner-data lets us skip config.sh entirely, so the
// (one-shot) registration token is only needed once.
const bootstrap = `set -eu
cd ` + containerHome + `
if [ ! -f .runner ]; then
  if [ -z "${RUNNER_TOKEN:-}" ]; then
    echo "no .runner config and RUNNER_TOKEN is empty" >&2
    exit 1
  fi
  ./config.sh \
    --url "$REPO_URL" \
    --token "$RUNNER_TOKEN" \
    --name "$RUNNER_NAME" \
    --labels "$LABELS" \
    --work "_work" \
    --unattended --replace
fi
exec ./run.sh
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := ensureRuntimeDirs(); err != nil {
		log.Fatalf("preparing runtime dirs: %v", err)
	}

	// /etc/resolv.conf is a symlink to /tmp/resolv.conf on gokrazy, which in
	// turn is a symlink to /proc/net/pnp until gokrazy/dhcp replaces it with
	// a real file. Write through the canonical /tmp/resolv.conf path so the
	// symlink chain stays intact and DHCP can still overwrite our fallback.
	switch action, err := dnsfallback.Ensure("/tmp/resolv.conf", dnsfallback.DefaultNameservers); {
	case err != nil:
		log.Printf("warning: ensure DNS fallback: %v", err)
	case action == dnsfallback.ActionWrote:
		log.Printf("seeded /tmp/resolv.conf with fallback nameservers %v", dnsfallback.DefaultNameservers)
	}

	if err := waitForPerm(ctx); err != nil {
		log.Fatalf("waiting for /perm: %v", err)
	}

	backoff := backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		err := runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("runner exited with error: %v (sleeping %s)", err, backoff)
		} else {
			log.Printf("runner exited cleanly (sleeping %s)", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

func runOnce(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	if err := os.Chown(dataDir, runnerUID, runnerGID); err != nil {
		return fmt.Errorf("chown %s to %d:%d: %w", dataDir, runnerUID, runnerGID, err)
	}

	// Best-effort: remove any stale container left from a previous boot.
	rm := exec.CommandContext(ctx, podmanBinary, "rm", "-f", containerName)
	rm.Stdout = os.Stdout
	rm.Stderr = os.Stderr
	_ = rm.Run()

	if err := refreshRunnerHome(ctx, cfg.Image); err != nil {
		return err
	}

	changed, err := enableAutomaticUpdates(filepath.Join(dataDir, ".runner"))
	if err != nil {
		return fmt.Errorf("enable automatic runner updates: %w", err)
	}
	if changed {
		log.Printf("enabled automatic runner updates in %s", filepath.Join(dataDir, ".runner"))
	}

	args := buildPodmanArgs(cfg)
	log.Printf("starting podman: %s %s", podmanBinary, strings.Join(redactArgs(args), " "))

	cmd := exec.CommandContext(ctx, podmanBinary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

type config struct {
	URL    string
	Name   string
	Labels string
	Image  string
	Token  string
	// Extra is any extra KEY=VALUE pairs from runner.env that should be
	// passed straight through to the container (e.g. ACCESS_TOKEN,
	// EPHEMERAL, RUNNER_GROUP, RUNNER_WORKDIR overrides).
	Extra []string
}

func loadConfig() (*config, error) {
	env, err := parseEnvFile(envFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", envFile, err)
	}

	url := strings.TrimSpace(env["URL"])
	if url == "" {
		url = strings.TrimSpace(env["REPO_URL"])
	}
	if url == "" {
		return nil, fmt.Errorf("URL (or REPO_URL) is required in %s", envFile)
	}
	name := strings.TrimSpace(env["NAME"])
	if name == "" {
		name = strings.TrimSpace(env["RUNNER_NAME"])
	}
	if name == "" {
		hn, _ := os.Hostname()
		if hn == "" {
			hn = "gokrazy-runner"
		}
		name = hn
	}
	labels := strings.TrimSpace(env["LABELS"])
	if labels == "" {
		labels = "self-hosted,linux,arm64,gokrazy"
	}
	image := strings.TrimSpace(env["IMAGE"])
	if image == "" {
		image = strings.TrimSpace(env["RUNNER_IMAGE"])
	}
	if image == "" {
		image = defaultImage
	}

	token, err := readToken(tokenFile)
	if err != nil {
		return nil, err
	}

	reserved := map[string]bool{
		"URL": true, "REPO_URL": true,
		"NAME": true, "RUNNER_NAME": true,
		"LABELS":       true,
		"IMAGE":        true,
		"RUNNER_IMAGE": true,
		"RUNNER_TOKEN": true,
	}
	var extra []string
	for k, v := range env {
		if reserved[k] {
			continue
		}
		extra = append(extra, k+"="+v)
	}

	return &config{
		URL:    url,
		Name:   name,
		Labels: labels,
		Image:  image,
		Token:  token,
		Extra:  extra,
	}, nil
}

func buildPodmanArgs(cfg *config) []string {
	args := []string{
		"run",
		"--rm",
		"--name=" + containerName,
		"--privileged",
		"--network=host",
		"--restart=no",
		"-v", dataDir + ":" + containerHome,
		// USB device nodes (populated on the host by cmd/usbdev-init) and the
		// matching sysfs tree, so probe-rs/nusb-style tools inside the runner
		// can enumerate and open attached probes.
		"-v", "/dev/bus/usb:/dev/bus/usb",
		"-v", "/sys/bus/usb:/sys/bus/usb",
		"-e", "REPO_URL=" + cfg.URL,
		"-e", "RUNNER_NAME=" + cfg.Name,
		"-e", "LABELS=" + cfg.Labels,
	}
	if cfg.Token != "" {
		args = append(args, "-e", "RUNNER_TOKEN="+cfg.Token)
	}
	for _, kv := range cfg.Extra {
		args = append(args, "-e", kv)
	}
	args = append(args,
		"--entrypoint", "/bin/bash",
		cfg.Image,
		"-c", bootstrap,
	)
	return args
}

func redactArgs(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i, a := range out {
		if strings.HasPrefix(a, "RUNNER_TOKEN=") {
			out[i] = "RUNNER_TOKEN=***"
		}
		if strings.HasPrefix(a, "ACCESS_TOKEN=") {
			out[i] = "ACCESS_TOKEN=***"
		}
		if strings.HasPrefix(a, "RUNNER_CFG_PAT=") {
			out[i] = "RUNNER_CFG_PAT=***"
		}
	}
	return out
}

func pullImage(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, podmanBinary, "pull", image)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// refreshRunnerHome pulls image and refreshes the persisted runner installation
// when the pull succeeds. If the registry is temporarily unavailable, an
// existing installation is left untouched; on first boot we still try the
// locally cached image so the appliance can start without registry access.
func refreshRunnerHome(ctx context.Context, image string) error {
	return refreshRunnerHomeWith(ctx, image, pullImage, runnerHomePopulated, populateRunnerHome)
}

// refreshRunnerHomeWith exposes the refresh operations as parameters so the
// success and offline-fallback policy can be tested without invoking podman.
func refreshRunnerHomeWith(
	ctx context.Context,
	image string,
	pull func(context.Context, string) error,
	isPopulated func(string) bool,
	populate func(context.Context, string, bool) error,
) error {
	pullErr := pull(ctx, image)
	populated := isPopulated(dataDir)
	if pullErr != nil && populated {
		log.Printf("warning: pull %s failed; keeping existing runner installation: %v", image, pullErr)
		return nil
	}
	if pullErr != nil {
		log.Printf("warning: pull %s failed; trying the local image cache: %v", image, pullErr)
	}

	if err := populate(ctx, image, populated); err != nil {
		return fmt.Errorf("populate %s from image: %w", dataDir, err)
	}
	return nil
}

func runnerHomePopulated(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "config.sh"))
	return err == nil && !info.IsDir()
}

// populateRunnerHome copies the runner distribution (config.sh, run.sh, bin/,
// externals/, ...) from image into /perm/runner-data. The main `podman run`
// bind-mounts dataDir over /home/runner, which would otherwise shadow every
// newly pulled binary. Copying on each successful pull updates the distribution
// while retaining files that only exist in the persistent directory, notably
// .runner, .credentials, _diag/, and _work/. The copy runs as root; cp -a
// preserves the image's runner:runner ownership (UID 1001), which lines up with
// the host because the container runs without a user-namespace remap.
func populateRunnerHome(ctx context.Context, image string, populated bool) error {
	if populated {
		log.Printf("refreshing runner installation in %s from %s", dataDir, image)
	} else {
		log.Printf("seeding runner installation in %s from %s", dataDir, image)
	}
	args := []string{
		"run", "--rm",
		"--user=0:0",
		"-v", dataDir + ":/runner-target",
		"--entrypoint", "/bin/bash",
		image,
		"-c", "cp -a /home/runner/. /runner-target/",
	}
	cmd := exec.CommandContext(ctx, podmanBinary, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// enableAutomaticUpdates migrates runners that were originally registered
// with --disableupdate. RunnerSettings is stored as JSON in .runner; changing
// the local setting makes the listener advertise disableUpdate=false on its
// next broker request, allowing GitHub to deliver runner updates. RawMessage
// preserves every unrelated value losslessly, including large numeric IDs.
func enableAutomaticUpdates(path string) (bool, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- caller supplies the fixed .runner path
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	var settings map[string]json.RawMessage
	if err := json.Unmarshal(b, &settings); err != nil {
		return false, fmt.Errorf("parse %s: %w", path, err)
	}
	raw, ok := settings["DisableUpdate"]
	if !ok {
		return false, nil
	}
	var disabled bool
	if err := json.Unmarshal(raw, &disabled); err != nil {
		return false, fmt.Errorf("parse DisableUpdate in %s: %w", path, err)
	}
	if !disabled {
		return false, nil
	}

	settings["DisableUpdate"] = json.RawMessage("false")
	updated, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode %s: %w", path, err)
	}
	updated = append(updated, '\n')
	if err := replaceFile(path, updated); err != nil {
		return false, err
	}
	return true, nil
}

// replaceFile atomically rewrites path while preserving its permissions and
// ownership. Ownership matters because runner-init is root but the container
// reads and later updates .runner as UID/GID 1001.
func replaceFile(path string, contents []byte) (retErr error) {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".runner-update-*")
	if err != nil {
		return fmt.Errorf("create temporary runner settings: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod temporary runner settings: %w", err)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		if err := tmp.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			return fmt.Errorf("chown temporary runner settings: %w", err)
		}
	}
	if _, err := tmp.Write(contents); err != nil {
		return fmt.Errorf("write temporary runner settings: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary runner settings: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary runner settings: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// ensureRuntimeDirs creates directories podman expects on a stock filesystem
// but that gokrazy's tmpfs-backed /var doesn't provide on a fresh boot.
// Without /var/tmp, `podman pull` fails with "creating a temporary directory:
// stat /var/tmp: no such file or directory".
func ensureRuntimeDirs() error {
	if err := os.MkdirAll("/var/tmp", 0o1777); err != nil {
		return fmt.Errorf("mkdir /var/tmp: %w", err)
	}
	// Match the sticky bit even if MkdirAll honoured umask.
	if err := os.Chmod("/var/tmp", 0o1777); err != nil {
		return fmt.Errorf("chmod /var/tmp: %w", err)
	}
	return nil
}

func waitForPerm(ctx context.Context) error {
	for {
		if _, err := os.Stat(permDir); err == nil {
			info, err := os.Stat(envFile)
			if err == nil && !info.IsDir() {
				return nil
			}
		}
		log.Printf("waiting for %s to be configured...", envFile)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
}

func readToken(path string) (string, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- fixed path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Token may have been consumed during a previous registration
			// and the runner config is already persisted in /perm/runner-data.
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path) // #nosec G304 -- fixed path
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		out[key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
