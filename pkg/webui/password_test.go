package webui

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestNewPasswordManager_MissingFileUsesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw.txt")
	pm, err := NewPasswordManager(path, "", "default-pw")
	if err != nil {
		t.Fatalf("NewPasswordManager: %v", err)
	}
	if !pm.IsDefault() {
		t.Fatal("expected IsDefault=true for missing file")
	}
	if !pm.Verify("default-pw") {
		t.Fatal("expected Verify to accept default password")
	}
	if pm.Verify("wrong") {
		t.Fatal("expected Verify to reject wrong password")
	}
}

func TestNewPasswordManager_ExistingFileLoaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw.txt")
	if err := os.WriteFile(path, []byte("stored-pw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pm, err := NewPasswordManager(path, "", "default-pw")
	if err != nil {
		t.Fatalf("NewPasswordManager: %v", err)
	}
	if pm.IsDefault() {
		t.Fatal("expected IsDefault=false when file exists")
	}
	if !pm.Verify("stored-pw") {
		t.Fatal("expected Verify to accept stored password (trimmed)")
	}
	if pm.Verify("default-pw") {
		t.Fatal("expected default password to not work when file present")
	}
}

func TestNewPasswordManager_FallbackPath(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "pw.txt")
	fallback := filepath.Join(dir, "etc-pw.txt")
	if err := os.WriteFile(fallback, []byte("baked-in\n"), 0644); err != nil {
		t.Fatal(err)
	}
	pm, err := NewPasswordManager(primary, fallback, "default-pw")
	if err != nil {
		t.Fatalf("NewPasswordManager: %v", err)
	}
	if pm.IsDefault() {
		t.Fatal("password loaded from fallbackPath should not count as default")
	}
	if !pm.Verify("baked-in") {
		t.Fatal("expected fallback password to verify")
	}

	if err := pm.Set("rotated-secret"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := os.Stat(primary); err != nil {
		t.Fatalf("Set must write to primary path: %v", err)
	}
	pm2, err := NewPasswordManager(primary, fallback, "default-pw")
	if err != nil {
		t.Fatal(err)
	}
	if !pm2.Verify("rotated-secret") {
		t.Fatal("after Set, primary path takes precedence over fallback")
	}
}

func TestNewPasswordManager_RejectsEmptyDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw.txt")
	if _, err := NewPasswordManager(path, "", ""); err == nil {
		t.Fatal("expected error for empty defaultPassword")
	}
}

func TestSet_MinLength(t *testing.T) {
	dir := t.TempDir()
	pm, err := NewPasswordManager(filepath.Join(dir, "pw.txt"), "", "default-pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Set("short"); err == nil {
		t.Fatal("expected error for <8 char password")
	}
	if err := pm.Set("1234567"); err == nil {
		t.Fatal("expected error for 7-char password")
	}
	if err := pm.Set("12345678"); err != nil {
		t.Fatalf("expected 8-char password to be accepted: %v", err)
	}
}

func TestSet_RejectsWhitespaceOnly(t *testing.T) {
	dir := t.TempDir()
	pm, err := NewPasswordManager(filepath.Join(dir, "pw.txt"), "", "default-pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Set("           "); err == nil {
		t.Fatal("expected whitespace-only password to be rejected")
	}
	if err := pm.Set(""); err == nil {
		t.Fatal("expected empty password to be rejected")
	}
}

func TestSet_MaxLength(t *testing.T) {
	dir := t.TempDir()
	pm, err := NewPasswordManager(filepath.Join(dir, "pw.txt"), "", "default-pw")
	if err != nil {
		t.Fatal(err)
	}
	long := make([]byte, 257)
	for i := range long {
		long[i] = 'a'
	}
	if err := pm.Set(string(long)); err == nil {
		t.Fatal("expected >256 char password to be rejected")
	}
}

func TestSet_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw.txt")
	pm, err := NewPasswordManager(path, "", "default-pw")
	if err != nil {
		t.Fatal(err)
	}
	if !pm.IsDefault() {
		t.Fatal("expected IsDefault before Set")
	}
	if err := pm.Set("new-secret-123"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if pm.IsDefault() {
		t.Fatal("expected IsDefault=false after Set")
	}
	if !pm.Verify("new-secret-123") {
		t.Fatal("expected Verify to accept new password in memory")
	}
	if pm.Verify("default-pw") {
		t.Fatal("expected old default to no longer verify")
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("expected mode 0600, got %v", fi.Mode().Perm())
	}

	pm2, err := NewPasswordManager(path, "", "default-pw")
	if err != nil {
		t.Fatal(err)
	}
	if pm2.IsDefault() {
		t.Fatal("expected IsDefault=false after reload")
	}
	if !pm2.Verify("new-secret-123") {
		t.Fatal("expected reloaded manager to verify the saved password")
	}
}

func TestVerify_PreservesValue(t *testing.T) {
	dir := t.TempDir()
	pm, err := NewPasswordManager(filepath.Join(dir, "pw.txt"), "", "default-pw")
	if err != nil {
		t.Fatal(err)
	}
	if pm.Verify("") {
		t.Fatal("empty input must not verify")
	}
	if pm.Verify("default-pw-extra") {
		t.Fatal("longer input must not verify")
	}
	if !pm.Verify("default-pw") {
		t.Fatal("exact input must verify")
	}
}

func TestConcurrentVerifyAndSet(t *testing.T) {
	dir := t.TempDir()
	pm, err := NewPasswordManager(filepath.Join(dir, "pw.txt"), "", "default-pw")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = pm.Verify("default-pw")
					_ = pm.IsDefault()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if err := pm.Set("password-iter-xx"); err != nil {
			t.Errorf("Set: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()
	if !pm.Verify("password-iter-xx") {
		t.Fatal("final password should verify")
	}
	if pm.IsDefault() {
		t.Fatal("IsDefault should be false after Set")
	}
}
