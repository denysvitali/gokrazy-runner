package wifimanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/mdlayher/wifi"
)

const scanTimeout = 10 * time.Second

var (
	// scanWaitDelay gives the driver a moment to publish results after
	// NL80211_CMD_TRIGGER_SCAN completes but before we dump them.
	scanWaitDelay = 2 * time.Second

	// Indirections so tests can drive the scan path without a radio.
	triggerScan       = triggerInterfaceScan
	readAccessPoints  = readAccessPointsWithFallback
	newScanClient     = func() (scanClient, error) { return wifi.New() }
	scanClientCloseFn = func(cl scanClient) {
		if c, ok := cl.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
)

// ErrNoRadio means nl80211 reported zero Wi-Fi interfaces. On gokrazy that
// almost always means the driver was never loaded (no udev, no modprobe) —
// wifi-init is what binds brcmfmac and brings wlan0 up — rather than that
// the board has no radio at all.
var ErrNoRadio = errors.New("no Wi-Fi interfaces found: the radio driver is not loaded (check the wifi-init service logs)")

// ScanResult is one SSID visible to the radio.
type ScanResult struct {
	SSID      string `json:"ssid"`
	Signal    int    `json:"signal"`    // dBm
	Frequency int    `json:"frequency"` // MHz
	Encrypted bool   `json:"encrypted"`
	Saved     bool   `json:"saved"`     // a PSK for this SSID is stored in /perm
	Connected bool   `json:"connected"` // this is the associated BSS
}

// ConnectionInfo describes the current association.
type ConnectionInfo struct {
	SSID      string `json:"ssid"`
	Signal    int    `json:"signal"` // dBm
	Interface string `json:"interface"`
}

type scanClient interface {
	Interfaces() ([]*wifi.Interface, error)
	AccessPoints(ifi *wifi.Interface) ([]*wifi.BSS, error)
	Scan(ctx context.Context, ifi *wifi.Interface) error
}

// ScanNetworks triggers a scan on every Wi-Fi interface and returns the
// union of the visible SSIDs, strongest BSS per SSID.
func (m *Manager) ScanNetworks() ([]ScanResult, error) {
	cl, err := newScanClient()
	if err != nil {
		return nil, fmt.Errorf("Wi-Fi client unavailable: %w", err)
	}
	defer scanClientCloseFn(cl)

	interfaces, err := cl.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list Wi-Fi interfaces: %w", err)
	}
	if len(interfaces) == 0 {
		return nil, ErrNoRadio
	}

	networksMap := make(map[string]ScanResult)
	prefer5GHz := m.getPrefer5GHzNetworks()
	scanned := 0

	for _, intf := range interfaces {
		accessPoints, err := scanInterface(cl, intf)
		if err != nil {
			log.Printf("wifi: scan on %s failed: %v", intf.Name, err)
			continue
		}
		scanned++
		processAccessPoints(accessPoints, networksMap, prefer5GHz)
	}

	if scanned == 0 {
		return nil, errors.New("scan failed on every Wi-Fi interface")
	}

	saved := make(map[string]bool)
	for _, net := range m.GetNetworks() {
		saved[net.SSID] = true
	}
	current, _ := m.GetCurrentConnection()

	results := make([]ScanResult, 0, len(networksMap))
	for _, result := range networksMap {
		result.Saved = saved[result.SSID]
		result.Connected = current != nil && current.SSID == result.SSID
		results = append(results, result)
	}
	return results, nil
}

// scanInterface triggers a scan on one interface and reads the results,
// falling back to whatever the kernel already has cached when the trigger
// fails (EBUSY while another scan is in flight is common).
func scanInterface(cl scanClient, intf *wifi.Interface) ([]*wifi.BSS, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()

	if err := triggerScan(ctx, cl, intf); err != nil {
		accessPoints, cachedErr := readAccessPoints(cl, intf)
		if cachedErr != nil {
			return nil, fmt.Errorf("trigger scan: %w; cached access points unavailable: %w", err, cachedErr)
		}
		log.Printf("wifi: trigger scan on %s failed (%v); using cached results", intf.Name, err)
		return accessPoints, nil
	}

	if scanWaitDelay > 0 {
		time.Sleep(scanWaitDelay)
	}
	return readAccessPoints(cl, intf)
}

// readAccessPointsWithFallback prefers our own nl80211 dump because the
// mdlayher/wifi parser drops BSSes whose information elements it does not
// recognise; it falls back to the library when the raw read yields nothing.
func readAccessPointsWithFallback(cl scanClient, intf *wifi.Interface) ([]*wifi.BSS, error) {
	accessPoints, err := readAccessPointsNL80211(intf)
	if err == nil && len(accessPoints) > 0 {
		return accessPoints, nil
	}

	fallback, fallbackErr := cl.AccessPoints(intf)
	if fallbackErr != nil {
		if err == nil {
			return accessPoints, nil
		}
		return nil, fallbackErr
	}
	return fallback, nil
}

// processAccessPoints folds the BSS list into networksMap, keeping one entry
// per SSID.
func processAccessPoints(accessPoints []*wifi.BSS, networksMap map[string]ScanResult, prefer5GHz bool) {
	for _, ap := range accessPoints {
		if ap == nil || ap.SSID == "" {
			continue // hidden network
		}

		result := ScanResult{
			SSID:      ap.SSID,
			Signal:    convertSignalToDBM(ap.Signal, ap.SignalUnspecified),
			Frequency: ap.Frequency,
			Encrypted: len(ap.RSN.PairwiseCiphers) > 0,
		}

		existing, exists := networksMap[ap.SSID]
		if !exists || shouldReplaceDuplicateSSID(existing, result, prefer5GHz) {
			networksMap[ap.SSID] = result
		}
	}
}

func shouldReplaceDuplicateSSID(existing, candidate ScanResult, prefer5GHz bool) bool {
	if prefer5GHz {
		existing5GHz := is5GHzFrequency(existing.Frequency)
		candidate5GHz := is5GHzFrequency(candidate.Frequency)
		if existing5GHz != candidate5GHz {
			return candidate5GHz
		}
	}
	return candidate.Signal > existing.Signal
}

func is5GHzFrequency(frequency int) bool {
	return frequency >= 5000 && frequency < 6000
}

// convertSignalToDBM turns the two nl80211 signal representations into dBm.
//
// mBm is preferred and divided by 100 with half-away-from-zero rounding: Go's
// integer division truncates toward zero, which would round -7250 mBm to
// -72 dBm and make weak APs look stronger than `iw` reports them. When the
// driver only fills the percent field, 0..100% is mapped onto -100..-40 dBm
// (the NetworkManager convention) — returning a literal 0 would advertise
// every such AP as line-of-sight.
func convertSignalToDBM(signalMBm int32, signalPercent uint32) int {
	if signalMBm != 0 {
		if signalMBm < 0 {
			return int((signalMBm - 50) / 100)
		}
		return int((signalMBm + 50) / 100)
	}
	if signalPercent == 0 {
		return 0
	}
	pct := signalPercent
	if pct > 100 {
		pct = 100
	}
	return -100 + int(pct)*60/100
}

// HasRadio reports whether nl80211 exposes at least one Wi-Fi interface.
// Used by the web UI to distinguish "no radio" from "scan failed".
func (m *Manager) HasRadio() bool {
	cl, err := newScanClient()
	if err != nil {
		return false
	}
	defer scanClientCloseFn(cl)

	interfaces, err := cl.Interfaces()
	return err == nil && len(interfaces) > 0
}

// GetCurrentConnection returns the active association, or an error when the
// device is not associated on any Wi-Fi interface.
func (m *Manager) GetCurrentConnection() (*ConnectionInfo, error) {
	cl, err := wifi.New()
	if err != nil {
		return nil, fmt.Errorf("Wi-Fi client unavailable: %w", err)
	}
	defer cl.Close()

	interfaces, err := cl.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list Wi-Fi interfaces: %w", err)
	}

	for _, intf := range interfaces {
		info, err := getInterfaceConnection(cl, intf)
		if err == nil && info != nil {
			return info, nil
		}
	}
	return nil, errors.New("not connected to any Wi-Fi network")
}

func getInterfaceConnection(cl *wifi.Client, intf *wifi.Interface) (*ConnectionInfo, error) {
	stationInfos, err := cl.StationInfo(intf)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err // not associated on this interface
		}
		return nil, err
	}

	for _, sta := range stationInfos {
		if bytes.Equal(sta.HardwareAddr, net.HardwareAddr{}) {
			continue
		}
		bss, err := cl.BSS(intf)
		if err != nil || bss == nil || bss.SSID == "" {
			continue
		}
		return &ConnectionInfo{
			SSID:      bss.SSID,
			Signal:    sta.Signal,
			Interface: intf.Name,
		}, nil
	}
	return nil, errors.New("no active connection")
}
