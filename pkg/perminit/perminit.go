// Package perminit provides helpers for ensuring the gokrazy /perm partition
// exists, fills the boot disk, and is formatted with a real filesystem.
//
// gokrazy reserves partition 4 for /perm. The CI image is built with a fixed
// TARGET_STORAGE_BYTES so partition 4 ends inside the image boundary and
// contains no filesystem. When the image is flashed onto a larger SD card,
// that leaves /perm both small and unformatted, so gokrazy falls back to
// mounting / read-only at /perm.
//
// This package is meant to be used by the cmd/perm-init service, which runs
// once per boot and reboots after each step it performs.
package perminit

import (
	"fmt"
	"io"
	"os"
	"strings"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// PermPartitionIndex is the gokrazy convention: /perm lives on partition 4.
const PermPartitionIndex = 4

// reservedTrailingSectors is the number of sectors at the end of a
// GPT-formatted disk reserved for the secondary GPT header (1 sector) and
// partition array (128 entries × 128 bytes / 512 bytes = 32 sectors).
const reservedTrailingSectors = 33

// GrowOutcome describes the result of GrowPermPartition.
type GrowOutcome int

const (
	// AlreadyFull means partition 4 already extends to the end of the disk
	// (within the GPT trailing reservation), so no rewrite happened.
	AlreadyFull GrowOutcome = iota

	// Grew means the partition table was rewritten so that partition 4
	// extends to the end of the disk. The kernel still has the previous
	// geometry cached; reboot before running mkfs against the partition
	// device.
	Grew
)

// GrowPermPartition rewrites the GPT on blockDev (e.g. /dev/mmcblk0) so that
// partition 4 extends to the last usable LBA of the device. It is a no-op if
// partition 4 already covers the disk.
func GrowPermPartition(blockDev string) (GrowOutcome, error) {
	// Open without O_EXCL: at the point perm-init runs the kernel already has
	// the rootfs partition (partition 2) mounted, so an exclusive open would
	// fail with EBUSY on real hardware.
	disk, err := diskfs.Open(blockDev, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", blockDev, err)
	}
	defer disk.Backend.Close()

	tbl, err := disk.GetPartitionTable()
	if err != nil {
		return 0, fmt.Errorf("read partition table: %w", err)
	}
	table, ok := tbl.(*gpt.Table)
	if !ok {
		return 0, fmt.Errorf("expected GPT partition table, got %T", tbl)
	}
	if len(table.Partitions) < PermPartitionIndex {
		return 0, fmt.Errorf("disk has %d partitions, expected at least %d", len(table.Partitions), PermPartitionIndex)
	}

	sectorSize := uint64(table.LogicalSectorSize)
	if sectorSize == 0 {
		sectorSize = 512
	}
	diskSectors := uint64(disk.Size) / sectorSize
	if diskSectors <= reservedTrailingSectors+1 {
		return 0, fmt.Errorf("disk too small (%d sectors of %d bytes) for GPT layout", diskSectors, sectorSize)
	}
	maxEnd := diskSectors - reservedTrailingSectors - 1

	perm := table.Partitions[PermPartitionIndex-1]
	if perm.End >= maxEnd {
		return AlreadyFull, nil
	}
	if perm.Start == 0 || perm.Start >= maxEnd {
		return 0, fmt.Errorf("partition %d has implausible start sector %d (disk has %d sectors)", PermPartitionIndex, perm.Start, diskSectors)
	}

	perm.End = maxEnd
	perm.Size = (perm.End - perm.Start + 1) * sectorSize

	if err := disk.Partition(table); err != nil {
		return 0, fmt.Errorf("write partition table: %w", err)
	}
	return Grew, nil
}

// HasExistingFilesystem reports whether devicePath looks like it already
// holds an ext2/3/4 or FAT filesystem we should preserve.
func HasExistingFilesystem(devicePath string) (bool, error) {
	f, err := os.Open(devicePath)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", devicePath, err)
	}
	defer f.Close()

	buf := make([]byte, 2048)
	if _, err := io.ReadFull(f, buf); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", devicePath, err)
	}

	if buf[1080] == 0x53 && buf[1081] == 0xEF {
		return true, nil
	}
	switch string(buf[54:62]) {
	case "FAT12   ", "FAT16   ", "FAT     ":
		return true, nil
	}
	if string(buf[82:90]) == "FAT32   " {
		return true, nil
	}
	return false, nil
}

// MountInfoFile is overridable in tests; in production it points at the
// per-process mountinfo file the kernel exposes.
var MountInfoFile = "/proc/self/mountinfo"

// IsPermMounted reports whether /perm is mounted *and writable*.
//
// gokrazy bind-mounts the read-only rootfs at /perm when partition 4 can't
// be mounted (unformatted, too small, missing). That fallback shows up in
// /proc/self/mountinfo just like a real /perm mount, so we additionally
// require the mount options not to contain "ro" — otherwise perm-init would
// see "/perm is mounted" on first boot, do nothing, and leave the device
// stuck with a read-only /perm forever.
func IsPermMounted() (bool, error) {
	b, err := os.ReadFile(MountInfoFile)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		if fields[4] != "/perm" {
			continue
		}
		for _, opt := range strings.Split(fields[5], ",") {
			if opt == "ro" {
				return false, nil
			}
		}
		return true, nil
	}
	return false, nil
}
