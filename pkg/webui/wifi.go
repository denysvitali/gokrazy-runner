package webui

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/denysvitali/gokrazy-runner/pkg/wifimanager"
)

// WiFiManager is the subset of *wifimanager.Manager the web UI needs. It is
// an interface so handler tests can run without a radio.
type WiFiManager interface {
	GetNetworks() []wifimanager.Network
	AddNetwork(ssid, password string) error
	RemoveNetwork(ssid string) error
	ReorderNetworks(orderedSSIDs []string) error
	ScanNetworks() ([]wifimanager.ScanResult, error)
	GetCurrentConnection() (*wifimanager.ConnectionInfo, error)
}

// SavedNetwork is the API view of a saved network. The PSK is deliberately
// reduced to a boolean: credentials must never leave the device, even for an
// authenticated caller.
type SavedNetwork struct {
	SSID        string `json:"ssid"`
	HasPassword bool   `json:"has_password"`
}

func (s *Server) wifiMgr(w http.ResponseWriter) (WiFiManager, bool) {
	if s.cfg.WiFiMgr == nil {
		http.Error(w, "Wi-Fi manager not available", http.StatusServiceUnavailable)
		return nil, false
	}
	return s.cfg.WiFiMgr, true
}

// handleWiFiStatus reports the current association plus the saved networks.
func (s *Server) handleWiFiStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mgr, ok := s.wifiMgr(w)
	if !ok {
		return
	}

	saved := mgr.GetNetworks()
	safe := make([]SavedNetwork, len(saved))
	for i, net := range saved {
		safe[i] = SavedNetwork{SSID: net.SSID, HasPassword: net.PSK != ""}
	}

	resp := map[string]any{
		"connected": false,
		"networks":  safe,
	}
	if conn, err := mgr.GetCurrentConnection(); err == nil && conn != nil {
		resp["connected"] = true
		resp["ssid"] = conn.SSID
		resp["signal"] = conn.Signal
		resp["interface"] = conn.Interface
	} else if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleWiFiScan triggers a scan. POST (not GET) because a scan is a radio
// operation with side effects — it briefly interrupts the current
// association.
func (s *Server) handleWiFiScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	mgr, ok := s.wifiMgr(w)
	if !ok {
		return
	}

	sortBy := strings.TrimSpace(r.URL.Query().Get("sort"))
	networks, err := mgr.ScanNetworks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"networks": sortScanResults(networks, sortBy),
	})
}

// handleWiFiConnect saves an SSID/PSK and makes it the active profile.
// github.com/gokrazy/wifi picks the new /perm/wifi.json up on its next
// association attempt.
func (s *Server) handleWiFiConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	mgr, ok := s.wifiMgr(w)
	if !ok {
		return
	}

	var body struct {
		SSID     string `json:"ssid"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ssid := strings.TrimSpace(body.SSID)
	// The password is not trimmed: leading and trailing spaces are valid
	// (if unwise) characters in a WPA passphrase.
	if err := mgr.AddNetwork(ssid, body.Password); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleWiFiForget removes a saved network.
func (s *Server) handleWiFiForget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	mgr, ok := s.wifiMgr(w)
	if !ok {
		return
	}

	var body struct {
		SSID string `json:"ssid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := mgr.RemoveNetwork(strings.TrimSpace(body.SSID)); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleWiFiReorder rewrites the priority order; the first SSID becomes the
// active profile.
func (s *Server) handleWiFiReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	mgr, ok := s.wifiMgr(w)
	if !ok {
		return
	}

	var body struct {
		SSIDs []string `json:"ssids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := mgr.ReorderNetworks(body.SSIDs); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// sortScanResults orders scan results for display. The default (and the
// fallback for any unknown key) is strongest signal first.
func sortScanResults(networks []wifimanager.ScanResult, sortBy string) []wifimanager.ScanResult {
	sorted := make([]wifimanager.ScanResult, len(networks))
	copy(sorted, networks)

	switch strings.ToLower(sortBy) {
	case "name", "ssid":
		sort.SliceStable(sorted, func(i, j int) bool {
			return strings.ToLower(sorted[i].SSID) < strings.ToLower(sorted[j].SSID)
		})
	case "security":
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Encrypted != sorted[j].Encrypted {
				return sorted[i].Encrypted
			}
			return sorted[i].Signal > sorted[j].Signal
		})
	default:
		sort.SliceStable(sorted, func(i, j int) bool {
			return sorted[i].Signal > sorted[j].Signal
		})
	}
	return sorted
}
