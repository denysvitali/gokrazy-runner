package ota

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultTokenPath is where a GitHub token (classic PAT or fine-grained,
	// read-only on public repos) may be dropped to lift the unauthenticated
	// 60 requests/hour/IP limit to 5000/hour. Optional.
	DefaultTokenPath = "/perm/github.token"

	// releaseCacheTTL is how long a successful release listing is reused
	// before GitHub is asked again. The UI polls /api/ota/status, so without
	// this a single open browser tab burns the anonymous budget.
	releaseCacheTTL = 15 * time.Minute

	userAgent = "gokrazy-runner-ota"
)

// githubToken returns the GitHub token to authenticate release API calls and
// asset downloads with, or "" when none is configured. The on-disk token in
// /perm wins over the environment so the image stays secret-free.
func (m *Manager) githubToken() string {
	if data, err := os.ReadFile(m.tokenPath); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token
		}
	}
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

// HasGitHubToken reports whether an API token is configured, so the UI can
// explain rate limiting without ever exposing the token itself.
func (m *Manager) HasGitHubToken() bool { return m.githubToken() != "" }

// SetGitHubToken persists token to the token file (mode 0600). An empty
// token removes the file.
func (m *Manager) SetGitHubToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		if err := os.Remove(m.tokenPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		m.invalidateReleaseCache()
		return nil
	}
	if err := writeFileAtomic(m.tokenPath, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	m.invalidateReleaseCache()
	return nil
}

type releaseCache struct {
	releases  []Release
	etag      string
	fetchedAt time.Time
}

func (m *Manager) invalidateReleaseCache() {
	m.mu.Lock()
	m.cache = releaseCache{}
	m.mu.Unlock()
}

// StartWithURL installs a gzipped squashfs image from an arbitrary URL,
// bypassing the GitHub releases API entirely.
func (m *Manager) StartWithURL(ctx context.Context, rawURL string) (Status, error) {
	rawURL = strings.TrimSpace(rawURL)
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return m.Status(), errors.New("OTA image URL must be an http(s) URL")
	}

	status, err := m.begin("Downloading image from URL")
	if err != nil {
		return status, err
	}
	name := path.Base(parsed.Path)
	if name == "" || name == "/" || name == "." {
		name = m.assetName
	}
	status.Release = "custom URL"
	status.Asset = name
	status.AssetURL = rawURL
	status.State = "downloading"
	status.Phase = "downloading"
	m.set(status)

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Hour)
	go func() {
		defer cancel()
		// A URL-sourced image is a root filesystem only; no boot image.
		m.downloadAndInstall(runCtx, rawURL, 0, nil)
	}()
	return status, nil
}

// StartWithFile installs a gzipped squashfs image that has already been
// spooled to disk (e.g. an upload from the web UI). The file is removed once
// the install finishes, successfully or not.
func (m *Manager) StartWithFile(ctx context.Context, filePath, displayName string, size int64) (Status, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return m.Status(), fmt.Errorf("open uploaded OTA image: %w", err)
	}

	status, err := m.begin("Installing uploaded image")
	if err != nil {
		f.Close()
		return status, err
	}
	if size <= 0 {
		if info, statErr := f.Stat(); statErr == nil {
			size = info.Size()
		}
	}
	if strings.TrimSpace(displayName) == "" {
		displayName = path.Base(filePath)
	}
	status.Release = "uploaded"
	status.Asset = displayName
	status.State = "downloading"
	status.Phase = "downloading"
	status.TotalBytes = size
	m.set(status)

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Hour)
	go func() {
		defer cancel()
		defer os.Remove(filePath)
		defer f.Close()
		// An uploaded or URL-sourced image is a root filesystem only; there
		// is no boot image to pair with it.
		m.installStream(runCtx, f, size, nil)
	}()
	return status, nil
}

// begin claims the manager for a new install, returning an error when one is
// already in flight.
func (m *Manager) begin(message string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.status.State {
	case "checking", "downloading", "installing":
		return m.status, errors.New("OTA installation is already running")
	}
	m.status = Status{
		State:           "checking",
		Phase:           "checking",
		Message:         message,
		StartedAt:       time.Now(),
		ProgressPercent: 2,
	}
	return m.status, nil
}

// writeFileAtomic writes data via a temp file + rename in the same directory.
func writeFileAtomic(dest string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(dest)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dest)
}

func isGitHubHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "github.com" || host == "api.github.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

func setGitHubHeaders(req *http.Request, token string) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}
