package wifimanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// WiFiConfigPath holds every network the operator has saved through the web
// UI, in priority order.
const WiFiConfigPath = "/perm/extra-wifi.json"

// GokrazyWiFiConfigPath is the file github.com/gokrazy/wifi reads at boot.
// It holds a single network, so only the first saved entry is mirrored there.
const GokrazyWiFiConfigPath = "/perm/wifi.json"

// WiFiConfig is the on-disk shape of WiFiConfigPath.
type WiFiConfig struct {
	Networks []Network `json:"networks"`
}

// save persists both config files. Callers must hold m.mu.
func (m *Manager) save() error {
	if err := writeJSONFile(m.configPath, WiFiConfig{Networks: m.networks}); err != nil {
		return err
	}
	return m.saveGokrazyWiFiConfig()
}

// load reads the saved networks, falling back to gokrazy's own wifi.json so
// a device provisioned by hand (or by setup-gokrazy.sh) shows its network in
// the UI on first launch.
func (m *Manager) load() error {
	networks, err := loadWiFiNetworksFromFile(m.configPath)
	if err != nil {
		return err
	}
	if len(networks) == 0 {
		networks, err = loadWiFiNetworksFromFile(m.gokrazyConfigPath)
		if err != nil {
			return err
		}
	}
	if networks == nil {
		networks = make([]Network, 0)
	}
	m.networks = networks
	return nil
}

// loadWiFiNetworksFromFile accepts all three shapes we may find on disk:
// our {"networks":[…]} wrapper, a bare array, and gokrazy's single object.
func loadWiFiNetworksFromFile(path string) ([]Network, error) {
	// #nosec G304 -- path is a well-known config file location under /perm
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var config WiFiConfig
	if err := json.Unmarshal(data, &config); err == nil && config.Networks != nil {
		return filterConfiguredNetworks(config.Networks), nil
	}

	var networks []Network
	if err := json.Unmarshal(data, &networks); err == nil {
		return filterConfiguredNetworks(networks), nil
	}

	var network Network
	if err := json.Unmarshal(data, &network); err != nil {
		return nil, err
	}
	return filterConfiguredNetworks([]Network{network}), nil
}

// saveGokrazyWiFiConfig mirrors the highest-priority network into the file
// github.com/gokrazy/wifi reads. With no networks left the file is removed,
// which leaves the device on Ethernet instead of retrying a stale SSID.
func (m *Manager) saveGokrazyWiFiConfig() error {
	networks := filterConfiguredNetworks(m.cloneNetworks())
	if len(networks) == 0 {
		if err := os.Remove(m.gokrazyConfigPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return writeJSONFile(m.gokrazyConfigPath, networks[0])
}

func filterConfiguredNetworks(networks []Network) []Network {
	filtered := make([]Network, 0, len(networks))
	for _, network := range networks {
		network.SSID = strings.TrimSpace(network.SSID)
		if network.SSID != "" {
			filtered = append(filtered, network)
		}
	}
	return filtered
}

// writeJSONFile writes v atomically with mode 0600 — these files hold PSKs,
// and a power cut mid-write must not leave the device with a truncated
// wifi.json that gokrazy/wifi refuses to parse.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Chmod(path, 0o600)
}
