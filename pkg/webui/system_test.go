package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProcfs returns a readFile hook backed by an in-memory map.
func fakeProcfs(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		body, ok := files[path]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(body), nil
	}
}

func testSystemOptions(files map[string]string) SystemOptions {
	opts := SystemOptions{
		PodmanBinary:  filepath.Join(os.TempDir(), "definitely-not-podman"),
		ContainerName: "gokrazy-runner",
		readFile:      fakeProcfs(files),
		hostname:      func() string { return "test-pi" },
		interfaces:    func() ([]net.Interface, error) { return nil, nil },
		statfs:        func(string) (uint64, uint64, error) { return 0, 0, errors.New("no statfs") },
		readKmsg:      func(int) (string, error) { return "kernel line\n", nil },
		runCmd: func(context.Context, string, ...string) ([]byte, error) {
			return nil, errors.New("podman not available")
		},
	}
	return opts
}

func TestCollectSystemInfo(t *testing.T) {
	opts := testSystemOptions(map[string]string{
		"/proc/uptime":                          "12345.67 98765.43\n",
		"/proc/loadavg":                         "0.52 0.31 0.20 1/210 1234\n",
		"/proc/meminfo":                         "MemTotal:       3999999 kB\nMemFree:         500000 kB\nMemAvailable:   2500000 kB\nBuffers:          10000 kB\n",
		"/proc/sys/kernel/osrelease":            "6.6.51\n",
		"/proc/device-tree/model":               "Raspberry Pi 4 Model B Rev 1.4\x00",
		"/sys/class/thermal/thermal_zone0/temp": "48312\n",
	})
	opts.statfs = func(path string) (uint64, uint64, error) {
		if path == "/perm" {
			return 30 << 30, 20 << 30, nil
		}
		return 1 << 30, 512 << 20, nil
	}
	opts.defaults()

	info := opts.collect(context.Background(), ServerConfig{Version: "1.2.3"})

	if info.Hostname != "test-pi" {
		t.Errorf("hostname = %q", info.Hostname)
	}
	if info.Version != "1.2.3" {
		t.Errorf("version = %q", info.Version)
	}
	// The device-tree model is NUL-terminated; the NUL must not survive.
	if info.Model != "Raspberry Pi 4 Model B Rev 1.4" {
		t.Errorf("model = %q", info.Model)
	}
	if info.Kernel != "6.6.51" {
		t.Errorf("kernel = %q", info.Kernel)
	}
	if info.UptimeSecs != 12345.67 {
		t.Errorf("uptime = %v", info.UptimeSecs)
	}
	if len(info.LoadAvg) != 3 || info.LoadAvg[0] != 0.52 || info.LoadAvg[2] != 0.20 {
		t.Errorf("load average = %v", info.LoadAvg)
	}
	if info.CPUTempC == nil || *info.CPUTempC != 48.312 {
		t.Errorf("cpu temp = %v", info.CPUTempC)
	}
	if info.Memory == nil || info.Memory.TotalBytes != 3999999*1024 {
		t.Fatalf("memory = %+v", info.Memory)
	}
	if info.Memory.AvailableBytes != 2500000*1024 {
		t.Errorf("available = %d", info.Memory.AvailableBytes)
	}
	if len(info.Disks) != 2 {
		t.Fatalf("disks = %+v", info.Disks)
	}
	if info.Disks[1].Path != "/perm" || info.Disks[1].FreeBytes != 20<<30 {
		t.Errorf("perm disk = %+v", info.Disks[1])
	}
}

func TestCollectSystemInfoMissingProcfs(t *testing.T) {
	// A kernel without a thermal zone, meminfo, or loadavg must still yield
	// a usable payload rather than an error or bogus zeroes.
	opts := testSystemOptions(nil)
	opts.defaults()

	info := opts.collect(context.Background(), ServerConfig{})
	if info.CPUTempC != nil {
		t.Errorf("cpu temp = %v, want nil when no thermal zone", info.CPUTempC)
	}
	if info.Memory != nil {
		t.Errorf("memory = %+v, want nil when meminfo is absent", info.Memory)
	}
	if info.LoadAvg != nil {
		t.Errorf("load average = %v, want nil", info.LoadAvg)
	}
	if info.UptimeSecs != 0 {
		t.Errorf("uptime = %v, want 0", info.UptimeSecs)
	}
	if info.Version != "unknown" {
		t.Errorf("version = %q, want unknown", info.Version)
	}
}

func TestReadInterfacesSkipsLoopback(t *testing.T) {
	opts := testSystemOptions(nil)
	opts.interfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
			{Name: "eth0", Flags: net.FlagUp},
			{Name: "wlan0"},
		}, nil
	}
	opts.defaults()

	got := opts.readInterfaces()
	if len(got) != 2 {
		t.Fatalf("interfaces = %+v, want eth0 and wlan0", got)
	}
	if got[0].Name != "eth0" || !got[0].Up {
		t.Errorf("eth0 = %+v", got[0])
	}
	if got[1].Name != "wlan0" || got[1].Up {
		t.Errorf("wlan0 = %+v", got[1])
	}
}

func TestReadRunnerStateWithoutPodman(t *testing.T) {
	opts := testSystemOptions(nil)
	opts.defaults()

	state := opts.readRunnerState(context.Background())
	if state.Status != "unknown" {
		t.Errorf("status = %q, want unknown", state.Status)
	}
	if !strings.Contains(state.Detail, "podman not present") {
		t.Errorf("detail = %q", state.Detail)
	}
}

// stubPodman writes an executable stand-in so the os.Stat presence check
// passes; runCmd is still faked, so the script is never executed.
func stubPodman(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestReadRunnerStateParsesPodmanOutput(t *testing.T) {
	tests := []struct {
		name       string
		output     string
		wantStatus string
		wantImage  string
	}{
		{
			name:       "running",
			output:     `[{"Names":["gokrazy-runner"],"Image":"ghcr.io/actions/actions-runner:latest","State":"running","Status":"Up 3 hours","StartedAt":1700000000}]`,
			wantStatus: "running",
			wantImage:  "ghcr.io/actions/actions-runner:latest",
		},
		{
			name:       "exited",
			output:     `[{"Names":["gokrazy-runner"],"Image":"img","State":"exited","Status":"Exited (1) 2 minutes ago"}]`,
			wantStatus: "stopped",
			wantImage:  "img",
		},
		{
			name:       "absent",
			output:     `[]`,
			wantStatus: "absent",
		},
		{
			name:       "unparseable",
			output:     `not json`,
			wantStatus: "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := testSystemOptions(nil)
			opts.PodmanBinary = stubPodman(t)
			opts.runCmd = func(context.Context, string, ...string) ([]byte, error) {
				return []byte(tc.output), nil
			}
			opts.defaults()

			state := opts.readRunnerState(context.Background())
			if state.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (detail %q)", state.Status, tc.wantStatus, state.Detail)
			}
			if tc.wantImage != "" && state.Image != tc.wantImage {
				t.Errorf("image = %q, want %q", state.Image, tc.wantImage)
			}
		})
	}
}

func TestReadRunnerStateCommandFailure(t *testing.T) {
	opts := testSystemOptions(nil)
	opts.PodmanBinary = stubPodman(t)
	opts.runCmd = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("cannot connect to podman"), errors.New("exit status 125")
	}
	opts.defaults()

	state := opts.readRunnerState(context.Background())
	if state.Status != "unknown" {
		t.Errorf("status = %q, want unknown", state.Status)
	}
	// The command's own output is more useful than "exit status 125".
	if state.Detail != "cannot connect to podman" {
		t.Errorf("detail = %q", state.Detail)
	}
}

func newSystemTestServer(t *testing.T, opts SystemOptions) *Server {
	t.Helper()
	dir := t.TempDir()
	pm, err := NewPasswordManager(filepath.Join(dir, "pw.txt"), "", "defaultpw")
	if err != nil {
		t.Fatalf("NewPasswordManager: %v", err)
	}
	if err := pm.Set("correct-horse"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	s, err := NewServer(ServerConfig{
		EnvPath:     filepath.Join(dir, "runner.env"),
		TokenPath:   filepath.Join(dir, "runner.token"),
		KeysPath:    filepath.Join(dir, "authorized_keys"),
		DataDir:     dir,
		PasswordMgr: pm,
		Version:     "test-1.2.3",
		System:      opts,
		Reboot:      func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func doSystem(t *testing.T, s *Server, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := authReq(t, method, target, nil, "correct-horse", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestSystemEndpoint(t *testing.T) {
	opts := testSystemOptions(map[string]string{
		"/proc/uptime": "600.0 100.0\n",
	})
	s := newSystemTestServer(t, opts)

	rr := doSystem(t, s, "GET", "/api/system")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	var info SystemInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if info.Hostname != "test-pi" || info.UptimeSecs != 600 {
		t.Fatalf("unexpected payload: %+v", info)
	}
	if info.Version != "test-1.2.3" {
		t.Errorf("version = %q", info.Version)
	}

	if rr := doSystem(t, s, "POST", "/api/system"); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/system = %d, want 405", rr.Code)
	}
}

func TestSystemEndpointRequiresAuth(t *testing.T) {
	s := newSystemTestServer(t, testSystemOptions(nil))
	req := authReq(t, "GET", "/api/system", nil, "", "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestLogsEndpoint(t *testing.T) {
	opts := testSystemOptions(nil)
	opts.PodmanBinary = stubPodman(t)
	var gotArgs []string
	opts.runCmd = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("runner log line\n"), nil
	}
	s := newSystemTestServer(t, opts)

	rr := doSystem(t, s, "GET", "/api/logs?lines=50")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	if !strings.Contains(rr.Body.String(), "runner log line") {
		t.Errorf("body = %q", rr.Body.String())
	}
	if strings.Join(gotArgs, " ") != "logs --tail 50 gokrazy-runner" {
		t.Errorf("podman args = %v", gotArgs)
	}
}

func TestLogsEndpointClampsLineCount(t *testing.T) {
	opts := testSystemOptions(nil)
	opts.PodmanBinary = stubPodman(t)
	var gotArgs []string
	opts.runCmd = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("ok\n"), nil
	}
	s := newSystemTestServer(t, opts)

	// An unbounded tail would make the device buffer an arbitrarily large
	// log into memory.
	if rr := doSystem(t, s, "GET", "/api/logs?lines=999999"); rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if want := fmt.Sprintf("logs --tail %d gokrazy-runner", maxLogLines); strings.Join(gotArgs, " ") != want {
		t.Errorf("podman args = %v, want %q", gotArgs, want)
	}
}

func TestLogsEndpointKernelSource(t *testing.T) {
	opts := testSystemOptions(nil)
	opts.readKmsg = func(int) (string, error) { return "line1\nline2\nline3\n", nil }
	s := newSystemTestServer(t, opts)

	rr := doSystem(t, s, "GET", "/api/logs?source=kernel&lines=2")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	body := rr.Body.String()
	if strings.Contains(body, "line1") || !strings.Contains(body, "line3") {
		t.Errorf("expected only the last 2 lines, got %q", body)
	}
}

func TestLogsEndpointRejectsBadInput(t *testing.T) {
	s := newSystemTestServer(t, testSystemOptions(nil))

	for _, target := range []string{
		"/api/logs?source=/etc/passwd",
		"/api/logs?lines=0",
		"/api/logs?lines=-5",
		"/api/logs?lines=abc",
	} {
		if rr := doSystem(t, s, "GET", target); rr.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", target, rr.Code)
		}
	}
	if rr := doSystem(t, s, "POST", "/api/logs"); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/logs = %d, want 405", rr.Code)
	}
}

func TestRunnerRestart(t *testing.T) {
	opts := testSystemOptions(nil)
	opts.PodmanBinary = stubPodman(t)
	var gotArgs []string
	opts.runCmd = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	}
	s := newSystemTestServer(t, opts)

	rr := doSystem(t, s, "POST", "/api/runner/restart")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body)
	}
	if strings.Join(gotArgs, " ") != "rm -f gokrazy-runner" {
		t.Errorf("podman args = %v", gotArgs)
	}

	if rr := doSystem(t, s, "GET", "/api/runner/restart"); rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/runner/restart = %d, want 405", rr.Code)
	}
}

func TestRunnerRestartWithoutPodman(t *testing.T) {
	s := newSystemTestServer(t, testSystemOptions(nil))
	rr := doSystem(t, s, "POST", "/api/runner/restart")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestRunnerRestartReportsPodmanFailure(t *testing.T) {
	opts := testSystemOptions(nil)
	opts.PodmanBinary = stubPodman(t)
	opts.runCmd = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("no such container"), errors.New("exit status 1")
	}
	s := newSystemTestServer(t, opts)

	rr := doSystem(t, s, "POST", "/api/runner/restart")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "no such container") {
		t.Errorf("body = %q", rr.Body.String())
	}
}
