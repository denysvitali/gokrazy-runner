package perminit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsPermMounted(t *testing.T) {
	tests := []struct {
		name      string
		mountinfo string
		want      bool
	}{
		{
			name:      "no perm entry",
			mountinfo: "26 25 0:5 / /dev rw,nosuid - devtmpfs devtmpfs rw\n",
			want:      false,
		},
		{
			name: "real /perm partition (ext4, rw)",
			mountinfo: "" +
				"26 25 0:5 / /dev rw,nosuid - devtmpfs devtmpfs rw\n" +
				"40 1 179:4 / /perm rw,relatime - ext4 /dev/mmcblk0p4 rw\n",
			want: true,
		},
		{
			name: "fallback: gokrazy bind-mounts read-only rootfs at /perm",
			mountinfo: "" +
				"40 1 7:0 / /perm ro,relatime - squashfs /dev/loop0 ro\n",
			want: false,
		},
		{
			name: "ro shows up later in the option list",
			mountinfo: "" +
				"40 1 179:4 / /perm relatime,nosuid,ro - ext4 /dev/mmcblk0p4 ro\n",
			want: false,
		},
		{
			name: "do not be fooled by 'ro' as a substring",
			mountinfo: "" +
				"40 1 179:4 / /perm rw,relatime,errors=remount-ro - ext4 /dev/mmcblk0p4 rw\n",
			want: true,
		},
		{
			name: "another mount called /permanent should not match",
			mountinfo: "" +
				"40 1 179:4 / /permanent ro - ext4 /dev/sda1 ro\n",
			want: false,
		},
	}

	orig := MountInfoFile
	t.Cleanup(func() { MountInfoFile = orig })

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mountinfo")
			if err := os.WriteFile(path, []byte(tc.mountinfo), 0o600); err != nil {
				t.Fatal(err)
			}
			MountInfoFile = path
			got, err := IsPermMounted()
			if err != nil {
				t.Fatalf("IsPermMounted: %v", err)
			}
			if got != tc.want {
				t.Errorf("IsPermMounted() = %v, want %v", got, tc.want)
			}
		})
	}
}
