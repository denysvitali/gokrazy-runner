// wifi-init is a one-shot service that brings the Wi-Fi radio up and then
// hands off to github.com/gokrazy/wifi, which associates using
// /perm/wifi.json.
//
// gokrazy has no udev and no modprobe, so the brcmfmac driver for the
// Raspberry Pi 4's on-board radio is never auto-loaded: without this service
// wlan0 simply does not exist. We load the modules by hand, set the
// regulatory domain (channels above 11 are otherwise unusable), disable
// power save, and exec the gokrazy Wi-Fi client.
//
// A runner is normally on Ethernet, so by default we wait briefly for an
// Ethernet carrier and skip Wi-Fi entirely when one appears — a wired link
// is faster and more reliable for CI. Set WIFI_INIT_ETHERNET_FIRST=false to
// always bring Wi-Fi up.
package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// Version is set at build time via -ldflags.
var Version = "dev"

const (
	defaultInterface       = "wlan0"
	defaultTimeout         = 15 * time.Second
	defaultCountry         = "CH"
	defaultEthernetIface   = "eth0"
	defaultEthernetTimeout = 10 * time.Second
	defaultWiFiCommand     = "/user/wifi"
)

type kernelModule struct {
	name     string
	optional bool
}

// moduleOrder is load order, not alphabetical: brcmfmac depends on brcmutil.
// brcmfmac-wcc is only present on newer kernels.
var moduleOrder = []kernelModule{
	{name: "brcmutil"},
	{name: "brcmfmac"},
	{name: "brcmfmac-wcc", optional: true},
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("wifi-init: version %s", Version)

	iface := getenv("WIFI_INIT_INTERFACE", defaultInterface)
	timeout := getenvDuration("WIFI_INIT_TIMEOUT", defaultTimeout)
	country := strings.ToUpper(getenv("WIFI_COUNTRY", defaultCountry))
	ethernetFirst := getenvBool("WIFI_INIT_ETHERNET_FIRST", true)
	ethernetIface := getenv("WIFI_INIT_ETHERNET_INTERFACE", defaultEthernetIface)
	ethernetTimeout := getenvDuration("WIFI_INIT_ETHERNET_TIMEOUT", defaultEthernetTimeout)
	wifiCommand := getenv("WIFI_INIT_WIFI_COMMAND", defaultWiFiCommand)

	if ethernetFirst {
		log.Printf("wifi-init: waiting up to %s for Ethernet carrier on %s", ethernetTimeout, ethernetIface)
		if waitForEthernetCarrier(ethernetIface, ethernetTimeout) {
			log.Printf("wifi-init: Ethernet carrier on %s; leaving Wi-Fi disabled", ethernetIface)
			return
		}
		log.Printf("wifi-init: no Ethernet carrier on %s; enabling Wi-Fi", ethernetIface)
	}

	for _, module := range moduleOrder {
		if err := loadModule(module.name); err != nil {
			if module.optional {
				log.Printf("wifi-init: skipping optional module %s: %v", module.name, err)
				continue
			}
			log.Fatalf("wifi-init: load %s: %v", module.name, err)
		}
	}

	if err := waitForInterface(iface, timeout); err != nil {
		log.Fatalf("wifi-init: wait for %s: %v", iface, err)
	}
	log.Printf("wifi-init: %s is available", iface)

	if err := setRegulatoryDomain(country); err != nil {
		log.Printf("wifi-init: set country %s: %v", country, err)
	} else {
		log.Printf("wifi-init: set Wi-Fi country to %s", country)
	}

	if err := disablePowerSave(iface); err != nil {
		log.Printf("wifi-init: disable power save on %s: %v", iface, err)
	} else {
		log.Printf("wifi-init: disabled power save on %s", iface)
	}

	if strings.TrimSpace(wifiCommand) == "" {
		log.Printf("wifi-init: WIFI_INIT_WIFI_COMMAND is empty; not starting the Wi-Fi client")
		return
	}
	log.Printf("wifi-init: starting Wi-Fi client %s", wifiCommand)
	if err := unix.Exec(wifiCommand, []string{filepath.Base(wifiCommand)}, os.Environ()); err != nil {
		log.Fatalf("wifi-init: start Wi-Fi client %s: %v", wifiCommand, err)
	}
}

// waitForEthernetCarrier reports whether name has link within timeout.
func waitForEthernetCarrier(name string, timeout time.Duration) bool {
	if timeout < 0 {
		timeout = 0
	}
	deadline := time.Now().Add(timeout)
	for {
		carrier, err := hasCarrier(name)
		if err != nil {
			// Interface missing entirely — no point polling for a carrier.
			log.Printf("wifi-init: read carrier for %s: %v", name, err)
			return false
		}
		if carrier {
			return true
		}
		if timeout == 0 || time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// hasCarrier reads /sys/class/net/<name>/carrier. The file returns EINVAL
// while the interface is down, which is reported as "no carrier" rather than
// an error so callers keep polling until the link trains.
func hasCarrier(name string) (bool, error) {
	if name == "" || strings.ContainsAny(name, "/\x00") {
		return false, fmt.Errorf("invalid interface name %q", name)
	}
	path := filepath.Join("/sys/class/net", name, "carrier")
	// #nosec G304 -- path is built from a validated interface name
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, unix.EINVAL) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(string(data)) == "1", nil
}

// disablePowerSave turns off Wi-Fi power management via nl80211. brcmfmac
// defaults to power save on, which makes the radio stop acking after idle
// periods: the kernel keeps the DHCP lease so everything looks healthy
// on-device, but the runner becomes unreachable from the LAN until it
// reassociates.
func disablePowerSave(iface string) error {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", iface, err)
	}

	conn, family, err := dialNL80211()
	if err != nil {
		return err
	}
	defer conn.Close()

	ae := netlink.NewAttributeEncoder()
	ae.Uint32(unix.NL80211_ATTR_IFINDEX, uint32(ifi.Index))
	ae.Uint32(unix.NL80211_ATTR_PS_STATE, unix.NL80211_PS_DISABLED)
	data, err := ae.Encode()
	if err != nil {
		return fmt.Errorf("encode attrs: %w", err)
	}

	if _, err := conn.Execute(genetlink.Message{
		Header: genetlink.Header{
			Command: unix.NL80211_CMD_SET_POWER_SAVE,
			Version: family.Version,
		},
		Data: data,
	}, family.ID, netlink.Request|netlink.Acknowledge); err != nil {
		return fmt.Errorf("set power save: %w", err)
	}
	return nil
}

// setRegulatoryDomain applies an ISO 3166-1 alpha-2 country code. Without
// it the kernel uses the world-roaming domain, which forbids the upper
// 2.4 GHz channels and most of 5 GHz.
func setRegulatoryDomain(country string) error {
	if len(country) != 2 {
		return errors.New("country must be a two-letter ISO 3166-1 alpha-2 code")
	}
	for _, r := range country {
		if r < 'A' || r > 'Z' {
			return errors.New("country must contain only uppercase ASCII letters")
		}
	}

	conn, family, err := dialNL80211()
	if err != nil {
		return err
	}
	defer conn.Close()

	ae := netlink.NewAttributeEncoder()
	ae.String(unix.NL80211_ATTR_REG_ALPHA2, country)
	data, err := ae.Encode()
	if err != nil {
		return fmt.Errorf("encode attrs: %w", err)
	}

	if _, err := conn.Execute(genetlink.Message{
		Header: genetlink.Header{
			Command: unix.NL80211_CMD_REQ_SET_REG,
			Version: family.Version,
		},
		Data: data,
	}, family.ID, netlink.Request|netlink.Acknowledge); err != nil {
		return fmt.Errorf("set regulatory domain: %w", err)
	}
	return nil
}

func dialNL80211() (*genetlink.Conn, genetlink.Family, error) {
	conn, err := genetlink.Dial(nil)
	if err != nil {
		return nil, genetlink.Family{}, fmt.Errorf("genetlink dial: %w", err)
	}
	family, err := conn.GetFamily(unix.NL80211_GENL_NAME)
	if err != nil {
		_ = conn.Close()
		return nil, genetlink.Family{}, fmt.Errorf("get nl80211 family: %w", err)
	}
	return conn, family, nil
}

// loadModule is a minimal modprobe: gokrazy ships no module tools, so we
// find the .ko under /lib/modules/<release> and hand it to finit_module.
// EEXIST means the module is already loaded, which is success.
func loadModule(name string) error {
	path, err := findModule(name)
	if err != nil {
		return err
	}

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	err = unix.FinitModule(fd, "", 0)
	if err == nil || errors.Is(err, unix.EEXIST) {
		log.Printf("wifi-init: loaded %s from %s", name, path)
		return nil
	}
	return err
}

func findModule(name string) (string, error) {
	release, err := kernelRelease()
	if err != nil {
		return "", err
	}

	root := filepath.Join("/lib/modules", release)
	var matches []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // unreadable subtrees are skipped, not fatal
		}
		base := filepath.Base(path)
		// Match both plain .ko and compressed .ko.xz / .ko.gz.
		if base == name+".ko" || strings.HasPrefix(base, name+".ko.") {
			matches = append(matches, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("module %q not found under %s", name, root)
	}
	return matches[0], nil
}

func kernelRelease() (string, error) {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		return "", err
	}
	return charsToString(uts.Release[:]), nil
}

func charsToString(chars []byte) string {
	if i := strings.IndexByte(string(chars), 0); i >= 0 {
		return string(chars[:i])
	}
	return string(chars)
}

func waitForInterface(name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := net.InterfaceByName(name); err == nil {
			return nil
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return fmt.Errorf("interface did not appear within %s", timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("wifi-init: invalid %s=%q, using %s", key, raw, fallback)
		return fallback
	}
	return duration
}

func getenvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		log.Printf("wifi-init: invalid %s=%q, using %v", key, raw, fallback)
		return fallback
	}
}
