package webui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadAuthorizedKeysMissing(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadAuthorizedKeys(filepath.Join(dir, "missing"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breakglass", "authorized_keys")
	want := "ssh-ed25519 AAAA test@host\n"
	if err := WriteAuthorizedKeys(path, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestWriteCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "authorized_keys")
	if err := WriteAuthorizedKeys(path, "key\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	}
}

func TestWriteEmptyAllowed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breakglass", "authorized_keys")
	if err := WriteAuthorizedKeys(path, ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("want empty file, got size %d", info.Size())
	}
}

func TestWriteRejectsNUL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breakglass", "authorized_keys")
	if err := WriteAuthorizedKeys(path, "ssh-ed25519 AAAA\x00bad"); err == nil {
		t.Fatal("want error for NUL byte, got nil")
	}
}

func TestWriteFinalMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "breakglass", "authorized_keys")
	if err := WriteAuthorizedKeys(path, "key\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("want 0600, got %o", mode)
	}
}
