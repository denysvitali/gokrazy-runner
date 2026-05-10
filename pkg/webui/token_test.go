package webui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteTokenSuccess(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.token")
	if err := WriteToken(p, "abc123"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Fatalf("mode = %o, want 0600", got)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "abc123" {
		t.Fatalf("content = %q, want %q", string(b), "abc123")
	}
}

func TestWriteTokenTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.token")
	if err := WriteToken(p, "  \n\ttoken-value\n  "); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "token-value" {
		t.Fatalf("content = %q, want %q", string(b), "token-value")
	}
}

func TestWriteTokenRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.token")
	for _, in := range []string{"", "   ", "\n\t  \r\n"} {
		if err := WriteToken(p, in); err == nil {
			t.Fatalf("WriteToken(%q) = nil, want error", in)
		}
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("expected file not to exist, got err=%v", err)
	}
}

func TestWriteTokenCreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nested", "deeper", "runner.token")
	if err := WriteToken(p, "x"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Fatalf("mode = %o, want 0600", got)
	}
}

func TestTokenExistsMissing(t *testing.T) {
	dir := t.TempDir()
	if TokenExists(filepath.Join(dir, "nope")) {
		t.Fatal("TokenExists on missing file = true, want false")
	}
}

func TestTokenExistsDir(t *testing.T) {
	dir := t.TempDir()
	if TokenExists(dir) {
		t.Fatal("TokenExists on dir = true, want false")
	}
}

func TestTokenExistsSymlinkToMissing(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if TokenExists(link) {
		t.Fatal("TokenExists on dangling symlink = true, want false")
	}
}

func TestTokenExistsAfterWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.token")
	if TokenExists(p) {
		t.Fatal("TokenExists before write = true, want false")
	}
	if err := WriteToken(p, "hunter2"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if !TokenExists(p) {
		t.Fatal("TokenExists after write = false, want true")
	}
}
