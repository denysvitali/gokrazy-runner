// Command usbdev-init populates /dev/bus/usb/BBB/DDD character device nodes
// on a udev-less gokrazy host.
//
// Modern Linux USB libraries (nusb, libusb) open devices via
// /dev/bus/usb/<busnum>/<devnum>. On a stock distro udev creates these
// nodes from kernel uevents using rules in 50-udev-default.rules; gokrazy
// has no udev, so the kernel populates /sys/bus/usb/devices/ but the
// /dev/bus/usb/ tree never appears and tools like `probe-rs run` fail
// with `os error 2`.
//
// This service walks /sys/bus/usb/devices on a short interval, reads each
// device's busnum/devnum/dev attributes, and `mknod`s the matching char
// device. Stale nodes for vanished devices are removed. A periodic scan
// is good enough for our use case (USB JTAG probes that stay plugged in
// across CI jobs); we avoid the extra complexity of a netlink uevent
// listener.
//
// The host /dev/bus/usb tree is bind-mounted into the runner container
// by cmd/runner-init so the actions-runner job can see the devices.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	sysfsUsbDir  = "/sys/bus/usb/devices"
	devBusUsb    = "/dev/bus/usb"
	scanInterval = 5 * time.Second
	nodeMode     = 0o666
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds | log.Lshortfile)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if err := os.MkdirAll(devBusUsb, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", devBusUsb, err)
	}

	if err := reconcile(); err != nil {
		log.Printf("initial reconcile: %v", err)
	}

	t := time.NewTicker(scanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := reconcile(); err != nil {
				log.Printf("reconcile: %v", err)
			}
		}
	}
}

// reconcile makes /dev/bus/usb match the current /sys/bus/usb/devices view:
// creates missing nodes, refreshes nodes whose major:minor drifted, and
// removes nodes whose devices have disappeared.
func reconcile() error {
	wanted, err := scanSysfs()
	if err != nil {
		return fmt.Errorf("scan sysfs: %w", err)
	}

	for path, dev := range wanted {
		if err := ensureNode(path, dev); err != nil {
			log.Printf("ensure %s: %v", path, err)
		}
	}

	if err := pruneStale(wanted); err != nil {
		log.Printf("prune stale: %v", err)
	}
	return nil
}

type devInfo struct {
	major uint32
	minor uint32
}

// scanSysfs returns a map of desired /dev/bus/usb/BBB/DDD paths to their
// major:minor numbers, derived from /sys/bus/usb/devices/*.
func scanSysfs() (map[string]devInfo, error) {
	entries, err := os.ReadDir(sysfsUsbDir)
	if err != nil {
		return nil, err
	}

	out := make(map[string]devInfo, len(entries))
	for _, e := range entries {
		name := e.Name()
		// Interface entries contain ':' (e.g. "1-1.3:1.0") — skip them,
		// they aren't device nodes.
		if strings.ContainsRune(name, ':') {
			continue
		}

		sysPath := filepath.Join(sysfsUsbDir, name)
		busnum, err := readUint(filepath.Join(sysPath, "busnum"))
		if err != nil {
			// Not every entry has these attrs (e.g. transient symlinks).
			continue
		}
		devnum, err := readUint(filepath.Join(sysPath, "devnum"))
		if err != nil {
			continue
		}
		major, minor, err := readDev(filepath.Join(sysPath, "dev"))
		if err != nil {
			continue
		}

		nodePath := filepath.Join(devBusUsb,
			fmt.Sprintf("%03d", busnum),
			fmt.Sprintf("%03d", devnum))
		out[nodePath] = devInfo{major: major, minor: minor}
	}
	return out, nil
}

func ensureNode(path string, dev devInfo) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}

	wantRdev := unix.Mkdev(dev.major, dev.minor)

	if info, err := os.Stat(path); err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if ok && uint64(st.Rdev) == wantRdev && info.Mode()&os.ModeCharDevice != 0 {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	if err := unix.Mknod(path, unix.S_IFCHR|nodeMode, int(wantRdev)); err != nil {
		return fmt.Errorf("mknod %s: %w", path, err)
	}
	// Mknod honours umask; force the requested mode.
	if err := os.Chmod(path, nodeMode); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	log.Printf("created %s (%d:%d)", path, dev.major, dev.minor)
	return nil
}

func pruneStale(wanted map[string]devInfo) error {
	busDirs, err := os.ReadDir(devBusUsb)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, b := range busDirs {
		if !b.IsDir() {
			continue
		}
		busPath := filepath.Join(devBusUsb, b.Name())
		nodes, err := os.ReadDir(busPath)
		if err != nil {
			continue
		}
		for _, n := range nodes {
			nodePath := filepath.Join(busPath, n.Name())
			if _, keep := wanted[nodePath]; keep {
				continue
			}
			if err := os.Remove(nodePath); err != nil {
				log.Printf("remove stale node %s: %v", nodePath, err)
				continue
			}
			log.Printf("removed stale %s", nodePath)
		}
	}
	return nil
}

func readUint(path string) (uint32, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- caller passes sysfs paths
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(n), nil
}

func readDev(path string) (uint32, uint32, error) {
	b, err := os.ReadFile(path) // #nosec G304 -- caller passes sysfs paths
	if err != nil {
		return 0, 0, err
	}
	parts := strings.SplitN(strings.TrimSpace(string(b)), ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid dev attr %q", string(b))
	}
	major, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return 0, 0, err
	}
	minor, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0, 0, err
	}
	return uint32(major), uint32(minor), nil
}
