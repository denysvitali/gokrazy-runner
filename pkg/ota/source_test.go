package ota

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type captureInstaller struct {
	done chan []byte
}

func (c captureInstaller) InstallRoot(ctx context.Context, r io.Reader, _ InstallProgressFunc) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.done <- data
	return nil
}

func gzipBytes(t *testing.T, payload string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(payload)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func newTestManager(t *testing.T, inst Installer) *Manager {
	t.Helper()
	dir := t.TempDir()
	mgr, err := NewManager(Options{
		HistoryPath: filepath.Join(dir, "history.json"),
		TokenPath:   filepath.Join(dir, "github.token"),
		Installer:   inst,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return mgr
}

func TestGitHubTokenFileWinsOverEnv(t *testing.T) {
	mgr := newTestManager(t, fakeNoopInstaller{})
	t.Setenv("GITHUB_TOKEN", "from-env")
	if got := mgr.githubToken(); got != "from-env" {
		t.Fatalf("expected env token, got %q", got)
	}
	if err := mgr.SetGitHubToken("  from-file  "); err != nil {
		t.Fatalf("SetGitHubToken: %v", err)
	}
	if got := mgr.githubToken(); got != "from-file" {
		t.Fatalf("expected file token, got %q", got)
	}
	if !mgr.HasGitHubToken() {
		t.Fatal("HasGitHubToken = false")
	}
	info, err := os.Stat(mgr.tokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, want 0600", info.Mode().Perm())
	}
	if err := mgr.SetGitHubToken(""); err != nil {
		t.Fatalf("clear token: %v", err)
	}
	if got := mgr.githubToken(); got != "from-env" {
		t.Fatalf("after clearing expected env fallback, got %q", got)
	}
}

func TestFetchReleasesCachesAndServesStaleOnRateLimit(t *testing.T) {
	var calls atomic.Int64
	body := `[{"tag_name":"v1","published_at":"2026-01-01T00:00:00Z","assets":[{"name":"` + DefaultAssetName + `","browser_download_url":"https://example.invalid/a.gz","size":10}]}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("ETag", `"abc"`)
			_, _ = io.WriteString(w, body)
			return
		}
		// Every later call is rate limited.
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()

	mgr := newTestManager(t, fakeNoopInstaller{})
	mgr.apiURL = srv.URL

	if _, err := mgr.AvailableReleases(context.Background()); err != nil {
		t.Fatalf("first AvailableReleases: %v", err)
	}
	// Within the TTL the API is not contacted at all.
	if _, err := mgr.AvailableReleases(context.Background()); err != nil {
		t.Fatalf("cached AvailableReleases: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 API call while cached, got %d", calls.Load())
	}

	// Force revalidation: the rate-limited response must not lose the cache.
	mgr.mu.Lock()
	mgr.cache.fetchedAt = time.Now().Add(-2 * releaseCacheTTL)
	mgr.mu.Unlock()

	releases, err := mgr.AvailableReleases(context.Background())
	if err != nil {
		t.Fatalf("stale AvailableReleases: %v", err)
	}
	if len(releases) != 1 || releases[0].TagName != "v1" {
		t.Fatalf("unexpected stale releases: %+v", releases)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected revalidation call, got %d", calls.Load())
	}
}

func TestFetchReleasesReportsRateLimitWithoutCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "1800000000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()

	mgr := newTestManager(t, fakeNoopInstaller{})
	mgr.apiURL = srv.URL

	_, err := mgr.AvailableReleases(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if want := "rate limit exhausted"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not mention %q", err, want)
	}
}

func TestStartWithURLInstalls(t *testing.T) {
	payload := "squashfs-bytes"
	asset := gzipBytes(t, payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(asset)
	}))
	defer srv.Close()

	done := make(chan []byte, 1)
	mgr := newTestManager(t, captureInstaller{done: done})

	if _, err := mgr.StartWithURL(context.Background(), srv.URL+"/image.squashfs.gz"); err != nil {
		t.Fatalf("StartWithURL: %v", err)
	}
	select {
	case got := <-done:
		if string(got) != payload {
			t.Fatalf("installed %q, want %q", got, payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("install did not run")
	}
}

func TestStartWithURLRejectsNonHTTP(t *testing.T) {
	mgr := newTestManager(t, fakeNoopInstaller{})
	if _, err := mgr.StartWithURL(context.Background(), "file:///etc/passwd"); err == nil {
		t.Fatal("expected rejection of file:// URL")
	}
	if mgr.Status().State != "idle" {
		t.Fatalf("state = %q, want idle", mgr.Status().State)
	}
}

func TestStartWithFileInstallsAndRemovesSpool(t *testing.T) {
	payload := "uploaded-image"
	spool := filepath.Join(t.TempDir(), "upload.gz")
	if err := os.WriteFile(spool, gzipBytes(t, payload), 0o600); err != nil {
		t.Fatalf("write spool: %v", err)
	}

	done := make(chan []byte, 1)
	mgr := newTestManager(t, captureInstaller{done: done})

	if _, err := mgr.StartWithFile(context.Background(), spool, "upload.gz", 0); err != nil {
		t.Fatalf("StartWithFile: %v", err)
	}
	select {
	case got := <-done:
		if string(got) != payload {
			t.Fatalf("installed %q, want %q", got, payload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("install did not run")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(spool); os.IsNotExist(err) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("spool file was not removed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type fakeNoopInstaller struct{}

func (fakeNoopInstaller) InstallRoot(ctx context.Context, r io.Reader, _ InstallProgressFunc) error {
	_, err := io.Copy(io.Discard, r)
	return err
}
