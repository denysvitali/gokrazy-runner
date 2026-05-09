package perminit

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// CmdlineFile is the file used by BootBlockDevice to read the kernel command
// line. It is overridable for testing.
var CmdlineFile = "/proc/cmdline"

var rootRe = regexp.MustCompile(`(?:^|\s)(?:root|ubd0)=(/dev/(?:mmcblk[01]p|sda|loop0p|nvme0n1p))([23])\b`)

// BootBlockDevice returns the file system path to the block device gokrazy
// booted from (e.g. /dev/mmcblk0).
func BootBlockDevice() (string, error) {
	b, err := os.ReadFile(CmdlineFile)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", CmdlineFile, err)
	}
	matches := rootRe.FindStringSubmatch(string(b))
	if len(matches) != 3 {
		return "", fmt.Errorf("could not find supported root= entry in %s: %q", CmdlineFile, strings.TrimSpace(string(b)))
	}
	dev := strings.TrimSuffix(matches[1], "p")
	return dev, nil
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
