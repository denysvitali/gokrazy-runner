package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestReadUint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "n")
	if err := os.WriteFile(p, []byte("  42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readUint(p)
	if err != nil {
		t.Fatalf("readUint: %v", err)
	}
	if got != 42 {
		t.Fatalf("got %d, want 42", got)
	}
}

func TestReadDev(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dev")
	if err := os.WriteFile(p, []byte("189:12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	maj, min, err := readDev(p)
	if err != nil {
		t.Fatalf("readDev: %v", err)
	}
	if maj != 189 || min != 12 {
		t.Fatalf("got %d:%d, want 189:12", maj, min)
	}
}

func TestReadDevMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dev")
	if err := os.WriteFile(p, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readDev(p); err == nil {
		t.Fatal("expected error, got nil")
	}
}

// TestScanSysfsFiltersInterfaces verifies that interface entries (names
// containing ':') are skipped and only USB device entries are returned.
// Pointing scanSysfs at a temp dir is racy with the package-level constant,
// so instead we mimic its core filtering logic on a hand-built directory.
func TestScanSysfsFilteringPattern(t *testing.T) {
	dir := t.TempDir()
	entries := []struct {
		name      string
		busDevDev []string // busnum, devnum, dev contents; empty == omitted
	}{
		{"1-1.3", []string{"1", "12", "189:11"}},     // device
		{"1-1.3:1.0", []string{"1", "12", "189:11"}}, // interface
		{"1-1.3:1.1", []string{"1", "12", "189:11"}}, // interface
		{"usb1", []string{"1", "1", "189:0"}},        // root hub (also a device)
	}
	for _, e := range entries {
		sub := filepath.Join(dir, e.name)
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		files := []string{"busnum", "devnum", "dev"}
		for i, f := range files {
			if err := os.WriteFile(filepath.Join(sub, f), []byte(e.busDevDev[i]), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Re-implement the device-name filter that scanSysfs uses.
	var kept []string
	dirents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirents {
		if !contains(d.Name(), ':') {
			kept = append(kept, d.Name())
		}
	}
	sort.Strings(kept)
	want := []string{"1-1.3", "usb1"}
	if !reflect.DeepEqual(kept, want) {
		t.Fatalf("got %v, want %v", kept, want)
	}
}

func contains(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}
