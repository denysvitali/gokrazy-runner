package wifimanager

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// MaxSSIDLength is the 802.11 limit for an SSID, in bytes.
const MaxSSIDLength = 32

// Network is a saved Wi-Fi network.
type Network struct {
	SSID string `json:"ssid"`
	PSK  string `json:"psk,omitempty"` // empty for open networks
}

// Manager owns the saved network list and mirrors it to disk.
//
// The zero value is not usable; call NewManager.
type Manager struct {
	mu                 sync.RWMutex
	networks           []Network
	prefer5GHzNetworks bool

	// configPath is the runner-owned list of every known network.
	configPath string
	// gokrazyConfigPath is the single-network file github.com/gokrazy/wifi
	// reads at boot. The first entry of networks is mirrored there.
	gokrazyConfigPath string
}

// NewManager loads the saved networks from the default /perm paths.
func NewManager() (*Manager, error) {
	return NewManagerWithPaths(WiFiConfigPath, GokrazyWiFiConfigPath)
}

// NewManagerWithPaths is NewManager with overridable file locations (tests).
func NewManagerWithPaths(configPath, gokrazyConfigPath string) (*Manager, error) {
	m := &Manager{
		networks:           make([]Network, 0),
		prefer5GHzNetworks: true,
		configPath:         configPath,
		gokrazyConfigPath:  gokrazyConfigPath,
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// GetNetworks returns a copy of the saved networks, in priority order.
func (m *Manager) GetNetworks() []Network {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cloneNetworks()
}

// SetPrefer5GHzNetworks controls whether a scan collapses duplicate SSIDs
// onto the 5 GHz BSS.
func (m *Manager) SetPrefer5GHzNetworks(prefer bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prefer5GHzNetworks = prefer
}

func (m *Manager) getPrefer5GHzNetworks() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.prefer5GHzNetworks
}

// ValidateSSID rejects SSIDs that the 802.11 spec (or gokrazy's JSON config)
// cannot represent.
func ValidateSSID(ssid string) error {
	if ssid == "" {
		return errors.New("SSID cannot be empty")
	}
	if len(ssid) > MaxSSIDLength {
		return fmt.Errorf("SSID exceeds maximum length of %d bytes", MaxSSIDLength)
	}
	if strings.ContainsAny(ssid, "\x00\r\n") {
		return errors.New("SSID contains control characters")
	}
	return nil
}

// AddNetwork saves a network and makes it the active gokrazy profile. An
// existing entry with the same SSID has its PSK replaced and is promoted to
// the front. Pass an empty password for an open network.
func (m *Manager) AddNetwork(ssid, password string) error {
	if err := ValidateSSID(ssid); err != nil {
		return err
	}
	if password != "" {
		if err := ValidateWiFiPassword(password); err != nil {
			return err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Snapshot so a failed save (read-only /perm, disk full, …) doesn't leave
	// credentials live in memory that were never persisted — GetNetworks
	// would otherwise report a PSK the device will not use after a reboot.
	previous := m.cloneNetworks()

	rest := make([]Network, 0, len(m.networks))
	for _, net := range m.networks {
		if net.SSID != ssid {
			rest = append(rest, net)
		}
	}
	m.networks = append([]Network{{SSID: ssid, PSK: password}}, rest...)

	if err := m.save(); err != nil {
		m.networks = previous
		return err
	}
	return nil
}

// RemoveNetwork forgets a saved network.
func (m *Manager) RemoveNetwork(ssid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	previous := m.cloneNetworks()
	for i, net := range m.networks {
		if net.SSID == ssid {
			m.networks = append(m.networks[:i:i], m.networks[i+1:]...)
			if err := m.save(); err != nil {
				m.networks = previous
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("network not found: %s", ssid)
}

// ReorderNetworks rewrites the priority order. orderedSSIDs must be a
// permutation of the currently saved SSIDs; the first entry becomes the
// active gokrazy profile.
func (m *Manager) ReorderNetworks(orderedSSIDs []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(orderedSSIDs) != len(m.networks) {
		return fmt.Errorf("expected %d SSIDs, got %d", len(m.networks), len(orderedSSIDs))
	}

	bySSID := make(map[string]Network, len(m.networks))
	for _, net := range m.networks {
		bySSID[net.SSID] = net
	}

	newNetworks := make([]Network, 0, len(orderedSSIDs))
	for _, ssid := range orderedSSIDs {
		net, ok := bySSID[ssid]
		if !ok {
			return fmt.Errorf("network not found: %s", ssid)
		}
		delete(bySSID, ssid) // reject duplicates in the request
		newNetworks = append(newNetworks, net)
	}

	previous := m.networks
	m.networks = newNetworks
	if err := m.save(); err != nil {
		m.networks = previous
		return err
	}
	return nil
}

// cloneNetworks copies the network slice. Callers must hold m.mu.
func (m *Manager) cloneNetworks() []Network {
	out := make([]Network, len(m.networks))
	copy(out, m.networks)
	return out
}
