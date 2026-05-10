package perminit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBootBlockDevice(t *testing.T) {
	tests := []struct {
		name         string
		cmdline      string
		want         string
		wantErr      bool
		wantPartUUID string // if non-empty, expect resolvePartUUID call with this UUID
	}{
		{
			name:    "raspberry pi mmcblk0p2",
			cmdline: "console=tty1 root=/dev/mmcblk0p2 init=/gokrazy/init rootwait\n",
			want:    "/dev/mmcblk0",
		},
		{
			name:    "loopback test",
			cmdline: "root=/dev/loop0p3 quiet",
			want:    "/dev/loop0",
		},
		{
			name:    "nvme",
			cmdline: "root=/dev/nvme0n1p2",
			want:    "/dev/nvme0n1",
		},
		{
			name:    "sda (no trailing p)",
			cmdline: "root=/dev/sda2",
			want:    "/dev/sda",
		},
		{
			name:    "unsupported partition number 4",
			cmdline: "root=/dev/mmcblk0p4",
			wantErr: true,
		},
		{
			name:    "no match",
			cmdline: "console=ttyS0",
			wantErr: true,
		},
		{
			name:         "PARTUUID with PARTNROFF (gokrazy default)",
			cmdline:      "console=tty1 root=PARTUUID=60c24cc1-f3f9-427a-8199-89a6807c0001/PARTNROFF=1 init=/gokrazy/init",
			wantPartUUID: "60c24cc1-f3f9-427a-8199-89a6807c0001",
			want:         "/dev/mmcblk0",
		},
		{
			name:         "PARTUUID without PARTNROFF",
			cmdline:      "root=PARTUUID=aabbccdd-eeff-0011-2233-445566778899",
			wantPartUUID: "aabbccdd-eeff-0011-2233-445566778899",
			want:         "/dev/sda",
		},
	}

	dir := t.TempDir()
	origCmdline := CmdlineFile
	origResolve := resolvePartUUID
	t.Cleanup(func() {
		CmdlineFile = origCmdline
		resolvePartUUID = origResolve
	})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "cmdline-"+tc.name)
			if err := os.WriteFile(path, []byte(tc.cmdline), 0o600); err != nil {
				t.Fatal(err)
			}
			CmdlineFile = path
			if tc.wantPartUUID != "" {
				resolvePartUUID = func(uuid string) (string, error) {
					if uuid != tc.wantPartUUID {
						t.Errorf("resolvePartUUID(%q), want %q", uuid, tc.wantPartUUID)
					}
					return tc.want, nil
				}
			} else {
				resolvePartUUID = func(uuid string) (string, error) {
					t.Errorf("resolvePartUUID called unexpectedly with %q", uuid)
					return "", nil
				}
			}
			got, err := BootBlockDevice()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BootBlockDevice() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BootBlockDevice() error: %v", err)
			}
			if got != tc.want {
				t.Errorf("BootBlockDevice() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPartitionDevice(t *testing.T) {
	tests := []struct {
		blockDev string
		n        int
		want     string
	}{
		{"/dev/mmcblk0", 4, "/dev/mmcblk0p4"},
		{"/dev/mmcblk0p", 4, "/dev/mmcblk0p4"},
		{"/dev/loop0", 1, "/dev/loop0p1"},
		{"/dev/nvme0n1", 2, "/dev/nvme0n1p2"},
		{"/dev/sda", 4, "/dev/sda4"},
	}
	for _, tc := range tests {
		got := PartitionDevice(tc.blockDev, tc.n)
		if got != tc.want {
			t.Errorf("PartitionDevice(%q, %d) = %q, want %q", tc.blockDev, tc.n, got, tc.want)
		}
	}
}
