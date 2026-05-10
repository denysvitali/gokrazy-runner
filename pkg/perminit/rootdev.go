package perminit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// CmdlineFile is the file used by BootBlockDevice to read the kernel command
// line. It is overridable for testing.
var CmdlineFile = "/proc/cmdline"

// SysBlockDir is the sysfs directory listing whole-disk block devices. It is
// overridable for testing.
var SysBlockDir = "/sys/block"

// DevDir is the directory containing block device nodes. It is overridable
// for testing.
var DevDir = "/dev"

var rootRe = regexp.MustCompile(`(?:^|\s)(?:root|ubd0)=(/dev/(?:mmcblk[01]p|sda|loop0p|nvme0n1p))([23])\b`)

// partUUIDRe matches the gokrazy-style root=PARTUUID=<uuid>[/PARTNROFF=N]
// kernel cmdline form. PARTNROFF is consumed by the kernel to select the
// active root partition; perm-init only needs the parent disk, so we ignore
// it here.
var partUUIDRe = regexp.MustCompile(`(?:^|\s)root=PARTUUID=([0-9A-Fa-f-]+)(?:/PARTNROFF=-?\d+)?\b`)

// resolvePartUUID is overridable in tests so the cmdline-parsing path can be
// exercised without real block devices.
var resolvePartUUID = diskByPartUUID

// BootBlockDevice returns the file system path to the block device gokrazy
// booted from (e.g. /dev/mmcblk0).
func BootBlockDevice() (string, error) {
	b, err := os.ReadFile(CmdlineFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", CmdlineFile, err)
	}
	cmdline := string(b)

	if matches := rootRe.FindStringSubmatch(cmdline); len(matches) == 3 {
		return strings.TrimSuffix(matches[1], "p"), nil
	}
	if matches := partUUIDRe.FindStringSubmatch(cmdline); len(matches) >= 2 {
		dev, err := resolvePartUUID(matches[1])
		if err != nil {
			return "", fmt.Errorf("resolve PARTUUID=%s: %w", matches[1], err)
		}
		return dev, nil
	}
	return "", fmt.Errorf("could not find supported root= entry in %s: %q", CmdlineFile, strings.TrimSpace(cmdline))
}

// diskByPartUUID enumerates whole-disk block devices under SysBlockDir and
// returns the one whose GPT contains a partition with the given UUID.
func diskByPartUUID(uuid string) (string, error) {
	entries, err := os.ReadDir(SysBlockDir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", SysBlockDir, err)
	}
	var tried []string
	for _, e := range entries {
		name := e.Name()
		if !looksLikeWholeDisk(name) {
			continue
		}
		dev := filepath.Join(DevDir, name)
		tried = append(tried, dev)
		ok, err := diskHasPartUUID(dev, uuid)
		if err != nil {
			continue
		}
		if ok {
			return dev, nil
		}
	}
	return "", fmt.Errorf("no block device contains a partition with UUID %s (tried %v)", uuid, tried)
}

// looksLikeWholeDisk filters /sys/block entries to plausible whole-disk
// devices, excluding partitions, eMMC boot/rpmb areas, and pseudo-devices we
// have no business touching.
func looksLikeWholeDisk(name string) bool {
	switch {
	case strings.HasPrefix(name, "mmcblk"):
		// e.g. mmcblk0; exclude mmcblk0boot0, mmcblk0rpmb, partitions are
		// not exposed under /sys/block (they live under the parent dir).
		return !strings.Contains(name, "boot") && !strings.HasSuffix(name, "rpmb")
	case strings.HasPrefix(name, "sd"):
		return len(name) >= 3
	case strings.HasPrefix(name, "nvme"):
		return strings.Contains(name, "n") && !strings.Contains(name, "p")
	case strings.HasPrefix(name, "loop"):
		return true
	}
	return false
}

func diskHasPartUUID(blockDev, uuid string) (bool, error) {
	disk, err := diskfs.Open(blockDev, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		return false, err
	}
	defer disk.Backend.Close()
	tbl, err := disk.GetPartitionTable()
	if err != nil {
		return false, err
	}
	gptTbl, ok := tbl.(*gpt.Table)
	if !ok {
		return false, nil
	}
	for _, p := range gptTbl.Partitions {
		if strings.EqualFold(p.GUID, uuid) {
			return true, nil
		}
	}
	return false, nil
}

// PartitionDevice returns the device path for the given partition number on
// the given block device. E.g. PartitionDevice("/dev/mmcblk0", 4) ->
// "/dev/mmcblk0p4".
func PartitionDevice(blockDev string, n int) string {
	if (strings.HasPrefix(blockDev, "/dev/mmcblk") ||
		strings.HasPrefix(blockDev, "/dev/loop") ||
		strings.HasPrefix(blockDev, "/dev/nvme")) &&
		!strings.HasSuffix(blockDev, "p") {
		blockDev += "p"
	}
	return blockDev + strconv.Itoa(n)
}
