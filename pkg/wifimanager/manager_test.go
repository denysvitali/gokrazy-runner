package wifimanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "extra-wifi.json")
	gokrazyPath := filepath.Join(dir, "wifi.json")
	m, err := NewManagerWithPaths(configPath, gokrazyPath)
	if err != nil {
		t.Fatalf("NewManagerWithPaths: %v", err)
	}
	return m, configPath, gokrazyPath
}

func TestAddNetworkWritesBothConfigs(t *testing.T) {
	m, configPath, gokrazyPath := newTestManager(t)

	if err := m.AddNetwork("HomeNet", "supersecret"); err != nil {
		t.Fatalf("AddNetwork: %v", err)
	}

	var saved WiFiConfig
	readJSON(t, configPath, &saved)
	if len(saved.Networks) != 1 || saved.Networks[0].SSID != "HomeNet" || saved.Networks[0].PSK != "supersecret" {
		t.Fatalf("unexpected extra-wifi.json contents: %+v", saved.Networks)
	}

	var gokrazy Network
	readJSON(t, gokrazyPath, &gokrazy)
	if gokrazy.SSID != "HomeNet" || gokrazy.PSK != "supersecret" {
		t.Fatalf("unexpected wifi.json contents: %+v", gokrazy)
	}

	// Both files hold a PSK and must not be world-readable.
	for _, path := range []string{configPath, gokrazyPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %o, want 600", path, perm)
		}
	}
}

func TestAddNetworkPromotesExisting(t *testing.T) {
	m, _, gokrazyPath := newTestManager(t)

	for _, ssid := range []string{"First", "Second"} {
		if err := m.AddNetwork(ssid, "password1"); err != nil {
			t.Fatalf("AddNetwork(%s): %v", ssid, err)
		}
	}
	// Re-adding First must move it back to the front and replace the PSK,
	// not create a duplicate entry.
	if err := m.AddNetwork("First", "password2"); err != nil {
		t.Fatalf("AddNetwork: %v", err)
	}

	networks := m.GetNetworks()
	if len(networks) != 2 {
		t.Fatalf("got %d networks, want 2: %+v", len(networks), networks)
	}
	if networks[0].SSID != "First" || networks[0].PSK != "password2" {
		t.Errorf("head is %+v, want First/password2", networks[0])
	}

	var gokrazy Network
	readJSON(t, gokrazyPath, &gokrazy)
	if gokrazy.SSID != "First" {
		t.Errorf("wifi.json mirrors %q, want First", gokrazy.SSID)
	}
}

func TestAddNetworkOpenNetwork(t *testing.T) {
	m, _, _ := newTestManager(t)
	if err := m.AddNetwork("OpenNet", ""); err != nil {
		t.Fatalf("AddNetwork with empty password: %v", err)
	}
	if got := m.GetNetworks()[0].PSK; got != "" {
		t.Errorf("PSK = %q, want empty", got)
	}
}

func TestAddNetworkRejectsInvalidInput(t *testing.T) {
	m, _, _ := newTestManager(t)

	tests := []struct {
		name     string
		ssid     string
		password string
	}{
		{"empty ssid", "", "password1"},
		{"oversized ssid", strings.Repeat("a", 33), "password1"},
		{"ssid with newline", "bad\nssid", "password1"},
		{"short password", "Net", "short"},
		{"long password", "Net", strings.Repeat("a", 64)},
		{"non-ascii password", "Net", "pässwordé"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.AddNetwork(tc.ssid, tc.password); err == nil {
				t.Fatal("expected an error, got nil")
			}
			if len(m.GetNetworks()) != 0 {
				t.Fatalf("rejected network was saved: %+v", m.GetNetworks())
			}
		})
	}
}

func TestAddNetworkRollsBackOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	// A directory where the config file should be makes every write fail.
	configPath := filepath.Join(dir, "extra-wifi.json")
	m, err := NewManagerWithPaths(configPath, filepath.Join(dir, "wifi.json"))
	if err != nil {
		t.Fatalf("NewManagerWithPaths: %v", err)
	}
	if err := os.Mkdir(configPath, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := m.AddNetwork("HomeNet", "supersecret"); err == nil {
		t.Fatal("expected AddNetwork to fail")
	}
	// The PSK must not linger in memory after a failed persist, or the UI
	// would report a credential the device will never use.
	if got := m.GetNetworks(); len(got) != 0 {
		t.Fatalf("networks = %+v, want empty after rollback", got)
	}
}

func TestRemoveNetwork(t *testing.T) {
	m, _, gokrazyPath := newTestManager(t)
	mustAdd(t, m, "First", "password1")
	mustAdd(t, m, "Second", "password2")

	// Head order after two adds is Second, First.
	if err := m.RemoveNetwork("Second"); err != nil {
		t.Fatalf("RemoveNetwork: %v", err)
	}
	networks := m.GetNetworks()
	if len(networks) != 1 || networks[0].SSID != "First" {
		t.Fatalf("networks = %+v, want [First]", networks)
	}

	var gokrazy Network
	readJSON(t, gokrazyPath, &gokrazy)
	if gokrazy.SSID != "First" {
		t.Errorf("wifi.json mirrors %q, want First", gokrazy.SSID)
	}

	if err := m.RemoveNetwork("Missing"); err == nil {
		t.Error("expected an error removing an unknown SSID")
	}
}

func TestRemoveLastNetworkDeletesGokrazyConfig(t *testing.T) {
	m, _, gokrazyPath := newTestManager(t)
	mustAdd(t, m, "Only", "password1")

	if err := m.RemoveNetwork("Only"); err != nil {
		t.Fatalf("RemoveNetwork: %v", err)
	}
	// Leaving a stale wifi.json behind would make the device keep retrying
	// an SSID the operator explicitly forgot.
	if _, err := os.Stat(gokrazyPath); !os.IsNotExist(err) {
		t.Fatalf("wifi.json still present (err=%v)", err)
	}
}

func TestReorderNetworks(t *testing.T) {
	m, _, gokrazyPath := newTestManager(t)
	mustAdd(t, m, "A", "password1")
	mustAdd(t, m, "B", "password2")
	mustAdd(t, m, "C", "password3")

	if err := m.ReorderNetworks([]string{"A", "B", "C"}); err != nil {
		t.Fatalf("ReorderNetworks: %v", err)
	}
	got := []string{}
	for _, net := range m.GetNetworks() {
		got = append(got, net.SSID)
	}
	if strings.Join(got, ",") != "A,B,C" {
		t.Fatalf("order = %v, want [A B C]", got)
	}

	var gokrazy Network
	readJSON(t, gokrazyPath, &gokrazy)
	if gokrazy.SSID != "A" {
		t.Errorf("wifi.json mirrors %q, want A", gokrazy.SSID)
	}

	if err := m.ReorderNetworks([]string{"A", "B"}); err == nil {
		t.Error("expected an error for a short SSID list")
	}
	// A duplicate is the same length but not a permutation; silently
	// dropping C would lose its stored PSK.
	if err := m.ReorderNetworks([]string{"A", "A", "B"}); err == nil {
		t.Error("expected an error for duplicate SSIDs")
	}
}

func TestGetNetworksReturnsCopy(t *testing.T) {
	m, _, _ := newTestManager(t)
	mustAdd(t, m, "HomeNet", "password1")

	networks := m.GetNetworks()
	networks[0].SSID = "mutated"

	if got := m.GetNetworks()[0].SSID; got != "HomeNet" {
		t.Fatalf("caller mutation leaked into the manager: %q", got)
	}
}

func mustAdd(t *testing.T, m *Manager, ssid, psk string) {
	t.Helper()
	if err := m.AddNetwork(ssid, psk); err != nil {
		t.Fatalf("AddNetwork(%s): %v", ssid, err)
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}
