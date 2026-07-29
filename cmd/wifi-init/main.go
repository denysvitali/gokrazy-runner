// wifi-init brings the Wi-Fi radio up and supervises
// github.com/gokrazy/wifi, which associates using /perm/wifi.json.
//
// gokrazy has no udev and no modprobe, so the brcmfmac driver for the
// Raspberry Pi 4's on-board radio is never auto-loaded: without this service
// wlan0 simply does not exist. We load the modules by hand, bring the link
// administratively up, set the regulatory domain (channels above 11 are
// otherwise unusable), and disable power save.
//
// The radio is brought up unconditionally, even when the device is on
// Ethernet and no network is saved yet. That is what makes the web UI's
// "scan" button work: an operator reaching the UI over Ethernet has to be
// able to see the nearby networks before they can pick one. Scanning needs
// a driver bound and the link UP; anything less reports "no Wi-Fi
// interfaces found".
//
// Associating is separate. wifi-init then supervises the Wi-Fi client for
// as long as a network is configured, restarting it with backoff and
// picking up a /perm/wifi.json written by the web UI without a reboot. Set
// WIFI_INIT_ETHERNET_FIRST=true to suppress associating while eth0 has a
// carrier.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// Version is set at build time via -ldflags.
var Version = "dev"

const (
	defaultInterface      = "wlan0"
	defaultTimeout        = 15 * time.Second
	defaultCountry        = "CH"
	defaultEthernetIface  = "eth0"
	defaultWiFiCommand    = "/user/wifi"
	defaultWiFiConfigPath = "/perm/wifi.json"

	// configPollInterval is how often we re-read /perm/wifi.json while no
	// network is configured, so a save from the web UI takes effect without
	// a reboot.
	configPollInterval = 10 * time.Second

	// radioRetryInterval is how often we retry a radio that isn't there yet.
	// Deliberately slow: the common case is a board that will never have one.
	radioRetryInterval = 60 * time.Second

	minBackoff = 5 * time.Second
	maxBackoff = 2 * time.Minute
)

// moduleOrder is load order, not alphabetical: brcmfmac depends on brcmutil,
// and the -bca/-cyw/-wcc modules depend on brcmfmac.
//
// All three vendor modules are loaded, not just the one this board needs.
// Since Linux 6.9 brcmfmac keeps its vendor-specific firmware glue in
// separate "fwvid" modules and pulls the right one in with request_module()
// at probe time — which shells out to /sbin/modprobe, and gokrazy has no
// modprobe. The call fails, the probe never completes, and wlan0 never
// appears even though brcmfmac itself loaded fine. Loading all three up
// front means the one the chip asks for is already resident. The Pi 4's
// CYW43455 wants brcmfmac-cyw.
//
// Every entry is best-effort: a kernel with the driver built in has none of
// these files.
var moduleOrder = []string{
	"brcmutil",
	"brcmfmac",
	"brcmfmac-bca",
	"brcmfmac-cyw",
	"brcmfmac-wcc",
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("wifi-init: version %s", Version)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	iface := getenv("WIFI_INIT_INTERFACE", defaultInterface)
	timeout := getenvDuration("WIFI_INIT_TIMEOUT", defaultTimeout)
	country := strings.ToUpper(getenv("WIFI_COUNTRY", defaultCountry))
	ethernetFirst := getenvBool("WIFI_INIT_ETHERNET_FIRST", false)
	ethernetIface := getenv("WIFI_INIT_ETHERNET_INTERFACE", defaultEthernetIface)
	wifiCommand := getenv("WIFI_INIT_WIFI_COMMAND", defaultWiFiCommand)
	configPath := getenv("WIFI_INIT_CONFIG_PATH", defaultWiFiConfigPath)

	// A board with no Wi-Fi hardware, or a kernel that ships no brcmfmac, is
	// a normal state — not a reason to die. Exiting here would make gokrazy
	// respawn us immediately, and a service that fails in a tight loop
	// drowns the log and burns CPU that a CI runner needs.
	if err := bringRadioUp(iface, country, timeout); err != nil {
		log.Printf("wifi-init: Wi-Fi unavailable: %v", err)
		waitForRadio(ctx, iface, country, timeout)
		if ctx.Err() != nil {
			return
		}
	}

	if strings.TrimSpace(wifiCommand) == "" {
		log.Printf("wifi-init: WIFI_INIT_WIFI_COMMAND is empty; radio is up, not associating")
		<-ctx.Done()
		return
	}
	supervise(ctx, wifiCommand, configPath, ethernetFirst, ethernetIface)
}

// waitForRadio keeps retrying bringRadioUp in the background. A USB dongle
// can be plugged in long after boot, and retrying slowly is much cheaper
// than being restarted by gokrazy in a hot loop.
func waitForRadio(ctx context.Context, iface, country string, timeout time.Duration) {
	log.Printf("wifi-init: retrying every %s; plug in a supported adapter or "+
		"build an image whose kernel ships brcmfmac", radioRetryInterval)
	for {
		if !sleepCtx(ctx, radioRetryInterval) {
			return
		}
		if err := bringRadioUp(iface, country, timeout); err == nil {
			log.Printf("wifi-init: Wi-Fi is now available on %s", iface)
			return
		}
	}
}

// bringRadioUp makes the interface usable for scanning: driver loaded, link
// administratively UP, regulatory domain applied, power save off.
//
// Loading a module is best-effort. gokrazy kernels vary: some ship brcmfmac
// as a .ko under /lib/modules, others build it in, and on the latter there
// is nothing to load and wlan0 already exists. Whether the radio actually
// works is decided by the interface check below, not by finit_module.
func bringRadioUp(iface, country string, timeout time.Duration) error {
	for _, module := range moduleOrder {
		if err := loadModule(module); err != nil {
			log.Printf("wifi-init: could not load %s (harmless if it is built into the kernel): %v",
				module, err)
		}
	}

	if err := waitForInterface(iface, timeout); err != nil {
		// Without this the operator sees only "wlan0 never appeared" and has
		// to find a shell to learn why. The kernel already knows: firmware
		// that failed to load, an SDIO bus that never enumerated, or a
		// missing fwvid module all say so in the ring buffer.
		logKernelWiFiMessages()
		return fmt.Errorf("%s never appeared: %w", iface, err)
	}
	log.Printf("wifi-init: %s is available", iface)

	// nl80211 refuses to scan on an interface that is administratively down,
	// which is how it comes up after finit_module. The Wi-Fi client would
	// normally do this, but the UI must be able to scan before any network
	// is configured.
	if err := setInterfaceUp(iface); err != nil {
		return fmt.Errorf("bring %s up: %w", iface, err)
	}
	log.Printf("wifi-init: %s is up", iface)

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
	return nil
}

// supervise runs the Wi-Fi client for as long as a network is configured.
// It polls rather than watching inotify because /perm/wifi.json is replaced
// via rename, and a 10s delay on a rarely-changed file is not worth the
// extra machinery.
func supervise(ctx context.Context, wifiCommand, configPath string, ethernetFirst bool, ethernetIface string) {
	backoff := minBackoff
	for ctx.Err() == nil {
		if !wifiConfigured(configPath) {
			log.Printf("wifi-init: no network configured in %s; radio is up for scanning", configPath)
			if !sleepCtx(ctx, configPollInterval) {
				return
			}
			continue
		}

		if ethernetFirst && hasCarrierOrFalse(ethernetIface) {
			log.Printf("wifi-init: Ethernet carrier on %s; not associating", ethernetIface)
			if !sleepCtx(ctx, configPollInterval) {
				return
			}
			continue
		}

		log.Printf("wifi-init: starting Wi-Fi client %s", wifiCommand)
		start := time.Now()
		err := runWiFiClient(ctx, wifiCommand)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("wifi-init: Wi-Fi client exited: %v", err)
		} else {
			log.Printf("wifi-init: Wi-Fi client exited cleanly")
		}

		// A client that stayed up is not in a crash loop; reset the backoff
		// so a transient association failure much later doesn't inherit a
		// two-minute delay.
		if time.Since(start) > maxBackoff {
			backoff = minBackoff
		}
		log.Printf("wifi-init: restarting in %s", backoff)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func runWiFiClient(ctx context.Context, wifiCommand string) error {
	// #nosec G204 -- wifiCommand comes from the image's PackageConfig
	cmd := exec.CommandContext(ctx, wifiCommand)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// wifiConfigured reports whether the gokrazy Wi-Fi client has something to
// associate to. An empty or whitespace-only file counts as unconfigured.
func wifiConfigured(path string) bool {
	// #nosec G304 -- path is a well-known config file location under /perm
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(data))) > 0
}

func hasCarrierOrFalse(name string) bool {
	carrier, err := hasCarrier(name)
	if err != nil {
		return false
	}
	return carrier
}

// sleepCtx waits for d, returning false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// setInterfaceUp sets IFF_UP via SIOCSIFFLAGS. gokrazy ships no `ip` binary
// and we have no rtnetlink dependency, so the classic ioctl is the smallest
// way to do this.
func setInterfaceUp(name string) error {
	if len(name) >= unix.IFNAMSIZ {
		return fmt.Errorf("interface name %q too long", name)
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return fmt.Errorf("get flags: %w", err)
	}
	flags := ifr.Uint16()
	if flags&unix.IFF_UP != 0 {
		return nil
	}
	ifr.SetUint16(flags | unix.IFF_UP)
	if err := unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr); err != nil {
		return fmt.Errorf("set flags: %w", err)
	}
	return nil
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
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("%s does not exist: this kernel ships no loadable modules", root)
	}

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

// wifiKmsgPatterns are the subsystems that explain a missing wlan0.
var wifiKmsgPatterns = []string{"brcmfmac", "brcmutil", "mmc1", "sdio", "wlan", "cfg80211", "firmware"}

// logKernelWiFiMessages echoes the Wi-Fi-related tail of /dev/kmsg. Best
// effort by design: this runs on a path that already failed, and failing to
// read the ring buffer must not mask the original error.
func logKernelWiFiMessages() {
	f, err := os.OpenFile("/dev/kmsg", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		log.Printf("wifi-init: cannot read /dev/kmsg for diagnostics: %v", err)
		return
	}
	defer f.Close()

	var matched []string
	buf := make([]byte, 8192)
	for len(matched) < 40 {
		n, err := f.Read(buf)
		if n > 0 {
			line := string(buf[:n])
			// kmsg records look like "<level>,<seq>,<usec>,-;<message>".
			if i := strings.IndexByte(line, ';'); i >= 0 {
				line = line[i+1:]
			}
			line = strings.TrimRight(line, "\n")
			lower := strings.ToLower(line)
			for _, pattern := range wifiKmsgPatterns {
				if strings.Contains(lower, pattern) {
					matched = append(matched, line)
					break
				}
			}
		}
		if err != nil {
			break
		}
	}

	if len(matched) == 0 {
		log.Printf("wifi-init: the kernel logged nothing about brcmfmac, mmc or sdio — " +
			"the radio may be disabled in config.txt or absent on this board")
		return
	}
	log.Printf("wifi-init: kernel messages about the radio:")
	for _, line := range matched {
		log.Printf("wifi-init:   %s", line)
	}
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
