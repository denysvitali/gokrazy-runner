// Package wifimanager manages Wi-Fi configuration for the gokrazy-runner
// appliance.
//
// Files:
//   - manager.go: Manager struct, add/remove/reorder of saved networks
//   - scanner.go: scanning + connection status via nl80211
//   - scanner_linux.go: raw nl80211 scan trigger and scan-result parsing
//   - config.go: persistence to /perm/extra-wifi.json and /perm/wifi.json
//
// The first entry of the saved network list is mirrored into
// /perm/wifi.json, which is the file github.com/gokrazy/wifi reads to
// associate. Everything else is bookkeeping so the web UI can offer a list
// of known networks.
//
// Usage:
//
//	mgr, err := wifimanager.NewManager()
//	if err != nil {
//		log.Fatal(err)
//	}
//	networks, err := mgr.ScanNetworks()
//	err = mgr.AddNetwork("MyWiFi", "password123")
package wifimanager
