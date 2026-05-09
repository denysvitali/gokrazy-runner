// Command runner-init drives a containerised GitHub Actions self-hosted
// runner on a gokrazy appliance.
//
// It reads its configuration from /perm:
//
//   - /perm/runner.env     KEY=VALUE pairs (URL, NAME, LABELS, IMAGE, ...)
//   - /perm/runner.token   one-shot GitHub registration token (chmod 0600)
//
// On boot it waits for /perm to be available, optionally pulls the runner
// image, and runs it via `podman run`. Container state (the registered
// runner identity, the workspace) lives under /perm/runner-data so that
// re-registration is not required across reboots.
//
// The container is expected to consume the standard environment variables
// from the popular myoung34/github-runner image (REPO_URL, RUNNER_NAME,
// RUNNER_TOKEN, LABELS) but the image is configurable via RUNNER_IMAGE in
// /perm/runner.env, so any compatible image can be used.
package main

import (
	"bufio"
	"context"
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
)

const (
	permDir       = "/perm"
	envFile       = "/perm/runner.env"
	tokenFile     = "/perm/runner.token"
	dataDir       = "/perm/runner-data"
	containerName = "gokrazy-runner"

	defaultImage = "docker.io/myoung34/github-runner:latest"

	podmanBinary = "/user/podman"

	backoffMin = 5 * time.Second
	backoffMax = 2 * time.Minute
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

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

	// Best-effort: remove any stale container left from a previous boot.
	rm := exec.CommandContext(ctx, podmanBinary, "rm", "-f", containerName)
	rm.Stdout = os.Stdout
	rm.Stderr = os.Stderr
	_ = rm.Run()

	if err := pullImage(ctx, cfg.Image); err != nil {
		log.Printf("warning: pull %s failed (will try to run from local cache): %v", cfg.Image, err)
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
		"LABELS":          true,
		"IMAGE":           true,
		"RUNNER_IMAGE":    true,
		"RUNNER_TOKEN":    true,
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
		"-v", dataDir + ":/runner",
		"-v", filepath.Join(dataDir, "_work") + ":/runner/_work",
		"-e", "REPO_URL=" + cfg.URL,
		"-e", "RUNNER_NAME=" + cfg.Name,
		"-e", "LABELS=" + cfg.Labels,
		"-e", "RUNNER_WORKDIR=/runner/_work",
		"-e", "DISABLE_AUTO_UPDATE=true",
	}
	if cfg.Token != "" {
		args = append(args, "-e", "RUNNER_TOKEN="+cfg.Token)
	}
	for _, kv := range cfg.Extra {
		args = append(args, "-e", kv)
	}
	args = append(args, cfg.Image)
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
