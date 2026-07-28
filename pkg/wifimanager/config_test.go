package wifimanager

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAcceptsAllOnDiskShapes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"wrapper object", `{"networks":[{"ssid":"A","psk":"password1"},{"ssid":"B"}]}`, []string{"A", "B"}},
		{"bare array", `[{"ssid":"A","psk":"password1"}]`, []string{"A"}},
		{"gokrazy single object", `{"ssid":"A","psk":"password1"}`, []string{"A"}},
		{"blank ssid dropped", `{"networks":[{"ssid":"  "},{"ssid":"B"}]}`, []string{"B"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "extra-wifi.json")
			if err := os.WriteFile(configPath, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			m, err := NewManagerWithPaths(configPath, filepath.Join(dir, "wifi.json"))
			if err != nil {
				t.Fatalf("NewManagerWithPaths: %v", err)
			}
			got := []string{}
			for _, net := range m.GetNetworks() {
				got = append(got, net.SSID)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("networks = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("networks = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestLoadFallsBackToGokrazyConfig(t *testing.T) {
	dir := t.TempDir()
	gokrazyPath := filepath.Join(dir, "wifi.json")
	// A device provisioned by setup-gokrazy.sh has only wifi.json; its
	// network must still show up in the UI.
	if err := os.WriteFile(gokrazyPath, []byte(`{"ssid":"Provisioned","psk":"password1"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	m, err := NewManagerWithPaths(filepath.Join(dir, "extra-wifi.json"), gokrazyPath)
	if err != nil {
		t.Fatalf("NewManagerWithPaths: %v", err)
	}
	networks := m.GetNetworks()
	if len(networks) != 1 || networks[0].SSID != "Provisioned" {
		t.Fatalf("networks = %+v, want [Provisioned]", networks)
	}
}

func TestLoadMissingFilesIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManagerWithPaths(filepath.Join(dir, "extra-wifi.json"), filepath.Join(dir, "wifi.json"))
	if err != nil {
		t.Fatalf("NewManagerWithPaths: %v", err)
	}
	if got := m.GetNetworks(); len(got) != 0 {
		t.Fatalf("networks = %+v, want empty", got)
	}
}

func TestLoadRejectsCorruptConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "extra-wifi.json")
	if err := os.WriteFile(configPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := NewManagerWithPaths(configPath, filepath.Join(dir, "wifi.json")); err == nil {
		t.Fatal("expected an error for a corrupt config")
	}
}

func TestWriteJSONFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "wifi.json")

	if err := writeJSONFile(path, Network{SSID: "A", PSK: "password1"}); err != nil {
		t.Fatalf("writeJSONFile: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind (err=%v)", err)
	}
	var got Network
	readJSON(t, path, &got)
	if got.SSID != "A" {
		t.Fatalf("got %+v, want SSID A", got)
	}
}
