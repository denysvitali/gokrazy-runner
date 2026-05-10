package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denysvitali/gokrazy-runner/pkg/ota"
)

// fakeInstaller is a no-op Installer used in tests so we never reach the
// loopback gokrazy /update/ endpoint.
type fakeInstaller struct{}

func (fakeInstaller) InstallRoot(ctx context.Context, r io.Reader, p ota.InstallProgressFunc) error {
	_, _ = io.Copy(io.Discard, r)
	return nil
}

func newOTATestServer(t *testing.T) *Server {
	t.Helper()
	s, _, _ := newTestServer(t)
	mgr, err := ota.NewManager(ota.Options{
		HistoryPath: "", // no persistence in tests
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
		"PARTUUID=12345678-02":            2,
		"PARTUUID=12345678-03":            3,
		"PARTUUID=2e18c40c-02/PARTNROFF=1": 2, // base + offset 1 = part 2
		"/dev/mmcblk0p2":                  2,
		"/dev/sda3":                       3,
	}
	for in, want := range cases {
		if got := parseRootPartition(in); got != want {
			t.Errorf("parseRootPartition(%q)=%d want %d", in, got, want)
		}
	}
}
