package ota

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
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

func (c captureInstaller) Install(ctx context.Context, images Images, _ InstallProgressFunc) error {
	r := images.Root
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
	dir := tempDir(t)
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
	waitForInstallEnd(t, mgr)
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
	waitForInstallEnd(t, mgr)

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

func (fakeNoopInstaller) Install(ctx context.Context, images Images, _ InstallProgressFunc) error {
	r := images.Root
	_, err := io.Copy(io.Discard, r)
	return err
}

// recordingInstaller captures what was streamed to each partition.
type recordingInstaller struct {
	root string
	boot string
	err  error
	done chan struct{}
}

func (r *recordingInstaller) Install(ctx context.Context, images Images, _ InstallProgressFunc) error {
	defer close(r.done)
	body, err := io.ReadAll(images.Root)
	if err != nil {
		return err
	}
	r.root = string(body)
	if images.Boot != nil {
		bootReader, err := images.Boot(ctx)
		if err != nil {
			r.err = err
			return err
		}
		defer bootReader.Close()
		bootBody, err := io.ReadAll(bootReader)
		if err != nil {
			return err
		}
		r.boot = string(bootBody)
	}
	return nil
}

// releaseServer serves a GitHub-shaped release listing plus its assets.
func releaseServer(t *testing.T, withBoot bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/assets/root", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gzipBytes(t, "ROOT-IMAGE"))
	})
	mux.HandleFunc("/assets/boot", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(gzipBytes(t, "BOOT-IMAGE"))
	})
	srv := httptest.NewServer(mux)
	mux.HandleFunc("/repos/test-owner/test-repo/releases", func(w http.ResponseWriter, r *http.Request) {
		assets := []map[string]any{
			{"name": DefaultAssetName, "browser_download_url": srv.URL + "/assets/root", "size": 10},
		}
		if withBoot {
			assets = append(assets, map[string]any{
				"name": DefaultBootAssetName, "browser_download_url": srv.URL + "/assets/boot", "size": 10,
			})
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"tag_name": "2026.1.1", "published_at": "2026-01-01T00:00:00Z", "assets": assets,
		}})
	})
	t.Cleanup(srv.Close)
	return srv
}

// waitForInstallEnd waits until the manager reaches a terminal state *and*
// has finished writing its install history. Polling only the status is not
// enough: recordInstallHistory runs after the status flips, so a test that
// returned at "installed" could still race the history write against its
// own temp-directory cleanup.
func waitForInstallEnd(t *testing.T, mgr *Manager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	terminal := false
	for time.Now().Before(deadline) {
		switch mgr.Status().State {
		case "installed", "failed":
			terminal = true
		}
		if terminal && len(mgr.InstallationHistory()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !terminal {
		t.Fatalf("install did not reach a terminal state; status=%+v", mgr.Status())
	}
}

// tempDir is t.TempDir() without the cleanup failure. The OTA manager writes
// its history from a goroutine the test cannot join, so a stray file landing
// during teardown must not fail an otherwise passing test.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ota-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestInstallStreamsBootPartition is the regression test for a device that
// installed a root filesystem containing /lib/modules/6.18.34-v8 while still
// booting a 6.12.47-v8 kernel: the kernel lives in the boot partition, so an
// update that streams only the root cannot change it.
func TestInstallStreamsBootPartition(t *testing.T) {
	srv := releaseServer(t, true)
	inst := &recordingInstaller{done: make(chan struct{})}

	mgr, err := NewManager(Options{
		Owner:       "test-owner",
		Repo:        "test-repo",
		APIURL:      srv.URL,
		Installer:   inst,
		HistoryPath: filepath.Join(tempDir(t), "history.json"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	st, err := mgr.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-inst.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("install never ran; status=%+v", mgr.Status())
	}
	_ = st
	waitForInstallEnd(t, mgr)

	if inst.root != "ROOT-IMAGE" {
		t.Errorf("root = %q, want ROOT-IMAGE", inst.root)
	}
	if inst.boot != "BOOT-IMAGE" {
		t.Errorf("boot = %q, want BOOT-IMAGE — the kernel would not be updated", inst.boot)
	}
}

// TestInstallWithoutBootAsset covers releases published before boot images
// existed: they must still install, leaving the kernel alone.
func TestInstallWithoutBootAsset(t *testing.T) {
	srv := releaseServer(t, false)
	inst := &recordingInstaller{done: make(chan struct{})}

	mgr, err := NewManager(Options{
		Owner:       "test-owner",
		Repo:        "test-repo",
		APIURL:      srv.URL,
		Installer:   inst,
		HistoryPath: filepath.Join(tempDir(t), "history.json"),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-inst.done:
	case <-time.After(10 * time.Second):
		t.Fatal("install never ran")
	}
	waitForInstallEnd(t, mgr)

	if inst.root != "ROOT-IMAGE" {
		t.Errorf("root = %q, want ROOT-IMAGE", inst.root)
	}
	if inst.boot != "" {
		t.Errorf("boot = %q, want no boot stream", inst.boot)
	}
}
