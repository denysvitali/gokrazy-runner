package webui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denysvitali/gokrazy-runner/pkg/wifimanager"
)

// fakeWiFiManager is an in-memory WiFiManager for handler tests.
type fakeWiFiManager struct {
	networks  []wifimanager.Network
	scan      []wifimanager.ScanResult
	scanErr   error
	conn      *wifimanager.ConnectionInfo
	connErr   error
	addErr    error
	removeErr error
	noRadio   bool
	added     [][2]string
	removed   []string
	reordered []string
}

func (f *fakeWiFiManager) GetNetworks() []wifimanager.Network { return f.networks }

func (f *fakeWiFiManager) HasRadio() bool { return !f.noRadio }

func (f *fakeWiFiManager) AddNetwork(ssid, password string) error {
	f.added = append(f.added, [2]string{ssid, password})
	if f.addErr != nil {
		return f.addErr
	}
	f.networks = append([]wifimanager.Network{{SSID: ssid, PSK: password}}, f.networks...)
	return nil
}

func (f *fakeWiFiManager) RemoveNetwork(ssid string) error {
	f.removed = append(f.removed, ssid)
	return f.removeErr
}

func (f *fakeWiFiManager) ReorderNetworks(orderedSSIDs []string) error {
	f.reordered = orderedSSIDs
	return nil
}

func (f *fakeWiFiManager) ScanNetworks() ([]wifimanager.ScanResult, error) {
	return f.scan, f.scanErr
}

func (f *fakeWiFiManager) GetCurrentConnection() (*wifimanager.ConnectionInfo, error) {
	return f.conn, f.connErr
}

func newWiFiTestServer(t *testing.T, mgr WiFiManager) (*Server, *PasswordManager) {
	t.Helper()
	dir := t.TempDir()
	pm, err := NewPasswordManager(filepath.Join(dir, "pw.txt"), "", "defaultpw")
	if err != nil {
		t.Fatalf("NewPasswordManager: %v", err)
	}
	if err := pm.Set("correct-horse"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	dataDir := filepath.Join(dir, "runner-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s, err := NewServer(ServerConfig{
		EnvPath:     filepath.Join(dir, "runner.env"),
		TokenPath:   filepath.Join(dir, "runner.token"),
		KeysPath:    filepath.Join(dir, "authorized_keys"),
		DataDir:     dataDir,
		PasswordMgr: pm,
		WiFiMgr:     mgr,
		Reboot:      func(ctx context.Context) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, pm
}

func doWiFi(t *testing.T, s *Server, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := authReq(t, method, target, body, "correct-horse", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", rr.Body.String(), err)
	}
	return out
}

func TestWiFiStatusConnected(t *testing.T) {
	mgr := &fakeWiFiManager{
		networks: []wifimanager.Network{{SSID: "Home", PSK: "password1"}, {SSID: "Open"}},
		conn:     &wifimanager.ConnectionInfo{SSID: "Home", Signal: -55, Interface: "wlan0"},
	}
	s, _ := newWiFiTestServer(t, mgr)

	rr := doWiFi(t, s, "GET", "/api/wifi/status", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body)
	}
	body := decodeBody(t, rr)
	if body["connected"] != true || body["ssid"] != "Home" {
		t.Fatalf("unexpected body: %v", body)
	}

	// The PSK must never be serialised, only its presence.
	if got := rr.Body.String(); strings.Contains(got, "password1") {
		t.Fatalf("response leaked a PSK: %s", got)
	}
	networks := body["networks"].([]any)
	if len(networks) != 2 {
		t.Fatalf("got %d networks, want 2", len(networks))
	}
	first := networks[0].(map[string]any)
	if first["ssid"] != "Home" || first["has_password"] != true {
		t.Fatalf("unexpected saved network: %v", first)
	}
	if second := networks[1].(map[string]any); second["has_password"] != false {
		t.Fatalf("open network reported as having a password: %v", second)
	}
}

func TestWiFiStatusReportsRadioPresence(t *testing.T) {
	// A device whose driver failed to load must be distinguishable from one
	// that is simply not associated, otherwise the UI can only say "scan
	// failed" and the operator has no idea why.
	for _, tc := range []struct{ noRadio, want bool }{{false, true}, {true, false}} {
		mgr := &fakeWiFiManager{noRadio: tc.noRadio, connErr: errors.New("not connected")}
		s, _ := newWiFiTestServer(t, mgr)

		rr := doWiFi(t, s, "GET", "/api/wifi/status", nil)
		if body := decodeBody(t, rr); body["has_radio"] != tc.want {
			t.Errorf("has_radio = %v, want %v", body["has_radio"], tc.want)
		}
	}
}

func TestWiFiStatusNotConnected(t *testing.T) {
	mgr := &fakeWiFiManager{connErr: errors.New("not connected to any Wi-Fi network")}
	s, _ := newWiFiTestServer(t, mgr)

	rr := doWiFi(t, s, "GET", "/api/wifi/status", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if body := decodeBody(t, rr); body["connected"] != false {
		t.Fatalf("connected = %v, want false", body["connected"])
	}
}

func TestWiFiEndpointsWithoutManager(t *testing.T) {
	s, _ := newWiFiTestServer(t, nil)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/wifi/status"},
		{"POST", "/api/wifi/scan"},
		{"POST", "/api/wifi/connect"},
		{"POST", "/api/wifi/forget"},
	} {
		var body any
		if tc.method == "POST" {
			body = map[string]string{"ssid": "Home"}
		}
		rr := doWiFi(t, s, tc.method, tc.path, body)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503", tc.method, tc.path, rr.Code)
		}
	}
}

func TestWiFiScanSorting(t *testing.T) {
	mgr := &fakeWiFiManager{scan: []wifimanager.ScanResult{
		{SSID: "weak", Signal: -80, Encrypted: true},
		{SSID: "Strong", Signal: -40},
		{SSID: "medium", Signal: -60, Encrypted: true},
	}}
	s, _ := newWiFiTestServer(t, mgr)

	tests := map[string][]string{
		"":               {"Strong", "medium", "weak"},
		"?sort=":         {"Strong", "medium", "weak"},
		"?sort=name":     {"medium", "Strong", "weak"},
		"?sort=security": {"medium", "weak", "Strong"},
		"?sort=bogus":    {"Strong", "medium", "weak"},
	}
	for query, want := range tests {
		rr := doWiFi(t, s, "POST", "/api/wifi/scan"+query, map[string]string{})
		if rr.Code != http.StatusOK {
			t.Fatalf("scan%s = %d: %s", query, rr.Code, rr.Body)
		}
		var resp struct {
			Networks []wifimanager.ScanResult `json:"networks"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for i, ssid := range want {
			if resp.Networks[i].SSID != ssid {
				t.Errorf("scan%s position %d = %q, want %q", query, i, resp.Networks[i].SSID, ssid)
			}
		}
	}
}

func TestWiFiScanError(t *testing.T) {
	mgr := &fakeWiFiManager{scanErr: errors.New("no Wi-Fi interfaces found")}
	s, _ := newWiFiTestServer(t, mgr)

	rr := doWiFi(t, s, "POST", "/api/wifi/scan", map[string]string{})
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestWiFiConnect(t *testing.T) {
	mgr := &fakeWiFiManager{}
	s, _ := newWiFiTestServer(t, mgr)

	// Surrounding whitespace on the SSID is a paste artefact; on the
	// passphrase it may be intentional, so only the SSID is trimmed.
	rr := doWiFi(t, s, "POST", "/api/wifi/connect", map[string]string{
		"ssid":     "  Home  ",
		"password": " padded-secret ",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body)
	}
	if len(mgr.added) != 1 || mgr.added[0][0] != "Home" || mgr.added[0][1] != " padded-secret " {
		t.Fatalf("AddNetwork called with %v", mgr.added)
	}
}

func TestWiFiConnectValidationError(t *testing.T) {
	mgr := &fakeWiFiManager{addErr: errors.New("Wi-Fi password must be at least 8 characters")}
	s, _ := newWiFiTestServer(t, mgr)

	rr := doWiFi(t, s, "POST", "/api/wifi/connect", map[string]string{"ssid": "Home", "password": "short"})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestWiFiForget(t *testing.T) {
	mgr := &fakeWiFiManager{}
	s, _ := newWiFiTestServer(t, mgr)

	rr := doWiFi(t, s, "POST", "/api/wifi/forget", map[string]string{"ssid": "Home"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body)
	}
	if len(mgr.removed) != 1 || mgr.removed[0] != "Home" {
		t.Fatalf("RemoveNetwork called with %v", mgr.removed)
	}

	mgr.removeErr = errors.New("network not found: Ghost")
	rr = doWiFi(t, s, "POST", "/api/wifi/forget", map[string]string{"ssid": "Ghost"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestWiFiReorder(t *testing.T) {
	mgr := &fakeWiFiManager{}
	s, _ := newWiFiTestServer(t, mgr)

	rr := doWiFi(t, s, "POST", "/api/wifi/reorder", map[string][]string{"ssids": {"B", "A"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body)
	}
	if len(mgr.reordered) != 2 || mgr.reordered[0] != "B" {
		t.Fatalf("ReorderNetworks called with %v", mgr.reordered)
	}
}

func TestWiFiMethodNotAllowed(t *testing.T) {
	s, _ := newWiFiTestServer(t, &fakeWiFiManager{})

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/wifi/status"},
		{"GET", "/api/wifi/scan"},
		{"GET", "/api/wifi/connect"},
		{"GET", "/api/wifi/forget"},
		{"GET", "/api/wifi/reorder"},
	} {
		rr := doWiFi(t, s, tc.method, tc.path, nil)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405", tc.method, tc.path, rr.Code)
		}
	}
}

func TestWiFiRequiresAuth(t *testing.T) {
	s, _ := newWiFiTestServer(t, &fakeWiFiManager{})

	req := authReq(t, "GET", "/api/wifi/status", nil, "", "")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestStatusReportsWiFiAvailability(t *testing.T) {
	for _, tc := range []struct {
		name string
		mgr  WiFiManager
		want bool
	}{
		{"with radio", &fakeWiFiManager{}, true},
		{"without radio", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newWiFiTestServer(t, tc.mgr)
			rr := doWiFi(t, s, "GET", "/api/status", nil)
			if body := decodeBody(t, rr); body["wifi_available"] != tc.want {
				t.Fatalf("wifi_available = %v, want %v", body["wifi_available"], tc.want)
			}
		})
	}
}
