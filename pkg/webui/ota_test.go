package webui

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
	"testing"
	"time"

	"github.com/denysvitali/gokrazy-runner/pkg/ota"
)

// fakeInstaller is a no-op Installer used in tests so we never reach the
// loopback gokrazy /update/ endpoint.
type fakeInstaller struct{}

func (fakeInstaller) Install(ctx context.Context, images ota.Images, p ota.InstallProgressFunc) error {
	r := images.Root
	_, _ = io.Copy(io.Discard, r)
	return nil
}

func newOTATestServer(t *testing.T) *Server {
	t.Helper()
	s, _, _ := newTestServer(t)
	dir := t.TempDir()
	otaUploadDir = dir
	t.Cleanup(func() { otaUploadDir = "/perm" })
	mgr, err := ota.NewManager(ota.Options{
		HistoryPath: "", // no persistence in tests
		TokenPath:   filepath.Join(dir, "github.token"),
		Installer:   fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("ota.NewManager: %v", err)
	}
	s.cfg.OTAMgr = mgr
	return s
}

func TestOTAStatusUnauthenticated(t *testing.T) {
	s := newOTATestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/ota/status", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestOTAStatusServiceUnavailable(t *testing.T) {
	s, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "GET", "/api/ota/status", nil, "correct-horse", ""))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when OTAMgr nil, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestOTAStatusOK(t *testing.T) {
	s := newOTATestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "GET", "/api/ota/status", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp otaStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.CurrentVersion != "test-1.2.3" {
		t.Fatalf("current_version mismatch: %q", resp.CurrentVersion)
	}
	if resp.State != "idle" {
		t.Fatalf("expected idle state, got %q", resp.State)
	}
	// Releases probably fail to fetch in CI; either Releases has data or
	// ReleasesError is set, but the request itself must succeed.
	if resp.ReleasesError == "" && resp.Releases == nil {
		t.Logf("note: no releases and no error (offline?)")
	}
}

func TestOTAInstallMethodNotAllowed(t *testing.T) {
	s := newOTATestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "GET", "/api/ota/install", nil, "correct-horse", ""))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestOTAInstallInvalidBody(t *testing.T) {
	s := newOTATestServer(t)
	req := httptest.NewRequest("POST", "/api/ota/install", strings.NewReader("not-json"))
	req.SetBasicAuth("admin", "correct-horse")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSameReleaseVersion(t *testing.T) {
	if !sameReleaseVersion(" v1 ", "v1") {
		t.Fatal("expected trim-equal")
	}
	if sameReleaseVersion("v1", "v2") {
		t.Fatal("v1 != v2")
	}
}

func TestIsKnownInstalledVersion(t *testing.T) {
	cases := map[string]bool{
		"":                  false,
		"dev":               false,
		goZeroPseudoVersion: false,
		"v1.2.3":            true,
	}
	for in, want := range cases {
		if got := isKnownInstalledVersion(in); got != want {
			t.Errorf("isKnownInstalledVersion(%q)=%v want %v", in, got, want)
		}
	}
}

func TestParseRootPartition(t *testing.T) {
	cases := map[string]int{
		"PARTUUID=12345678-02":             2,
		"PARTUUID=12345678-03":             3,
		"PARTUUID=2e18c40c-02/PARTNROFF=1": 2, // base + offset 1 = part 2
		"/dev/mmcblk0p2":                   2,
		"/dev/sda3":                        3,
	}
	for in, want := range cases {
		if got := parseRootPartition(in); got != want {
			t.Errorf("parseRootPartition(%q)=%d want %d", in, got, want)
		}
	}
}

func TestOTATokenRoundTrip(t *testing.T) {
	s := newOTATestServer(t)

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "POST", "/api/ota/token", map[string]string{"token": "ghp_test"}, "correct-horse", "application/json"))
	if rr.Code != http.StatusOK {
		t.Fatalf("save token: got %d body=%s", rr.Code, rr.Body.String())
	}
	if !s.cfg.OTAMgr.HasGitHubToken() {
		t.Fatal("token was not persisted")
	}

	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "GET", "/api/ota/status", nil, "correct-horse", ""))
	if body := rr.Body.String(); !strings.Contains(body, `"has_github_token":true`) {
		t.Fatalf("status does not report the token: %s", body)
	}
	if strings.Contains(rr.Body.String(), "ghp_test") {
		t.Fatal("status leaked the token")
	}

	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "POST", "/api/ota/token", map[string]string{"token": ""}, "correct-horse", "application/json"))
	if rr.Code != http.StatusOK {
		t.Fatalf("clear token: got %d body=%s", rr.Code, rr.Body.String())
	}
	if s.cfg.OTAMgr.HasGitHubToken() {
		t.Fatal("token was not cleared")
	}
}

func TestOTAUploadInstallsImage(t *testing.T) {
	s := newOTATestServer(t)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("root-image")); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/ota/upload?name=root.squashfs.gz", bytes.NewReader(buf.Bytes()))
	req.SetBasicAuth("admin", "correct-horse")
	req.Header.Set("Content-Type", "application/gzip")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("upload: got %d body=%s", rr.Code, rr.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if s.cfg.OTAMgr.Status().State == "installed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("install did not finish, state=%q", s.cfg.OTAMgr.Status().State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	entries, err := os.ReadDir(otaUploadDir)
	if err != nil {
		t.Fatalf("read spool dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ota-upload-") {
			t.Fatalf("spool file %s was not cleaned up", e.Name())
		}
	}
}

func TestOTAUploadRejectsEmptyBody(t *testing.T) {
	s := newOTATestServer(t)
	req := httptest.NewRequest("POST", "/api/ota/upload", bytes.NewReader(nil))
	req.SetBasicAuth("admin", "correct-horse")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rr.Code, rr.Body.String())
	}
}
