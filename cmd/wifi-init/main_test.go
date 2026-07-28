package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGetenv(t *testing.T) {
	t.Setenv("WIFI_TEST_VALUE", "  wlan1  ")
	if got := getenv("WIFI_TEST_VALUE", "wlan0"); got != "wlan1" {
		t.Errorf("getenv = %q, want wlan1", got)
	}
	t.Setenv("WIFI_TEST_VALUE", "   ")
	if got := getenv("WIFI_TEST_VALUE", "wlan0"); got != "wlan0" {
		t.Errorf("whitespace-only value should fall back, got %q", got)
	}
	if got := getenv("WIFI_TEST_UNSET", "wlan0"); got != "wlan0" {
		t.Errorf("getenv = %q, want wlan0", got)
	}
}

func TestGetenvDuration(t *testing.T) {
	t.Setenv("WIFI_TEST_TIMEOUT", "30s")
	if got := getenvDuration("WIFI_TEST_TIMEOUT", time.Second); got != 30*time.Second {
		t.Errorf("getenvDuration = %s, want 30s", got)
	}
	// A malformed duration must not take the radio down; fall back instead.
	t.Setenv("WIFI_TEST_TIMEOUT", "not-a-duration")
	if got := getenvDuration("WIFI_TEST_TIMEOUT", time.Second); got != time.Second {
		t.Errorf("getenvDuration = %s, want the 1s fallback", got)
	}
}

func TestGetenvBool(t *testing.T) {
	tests := map[string]bool{"1": true, "true": true, "YES": true, "on": true,
		"0": false, "false": false, "no": false, "OFF": false}
	for value, want := range tests {
		t.Setenv("WIFI_TEST_BOOL", value)
		if got := getenvBool("WIFI_TEST_BOOL", !want); got != want {
			t.Errorf("getenvBool(%q) = %v, want %v", value, got, want)
		}
	}
	t.Setenv("WIFI_TEST_BOOL", "maybe")
	if got := getenvBool("WIFI_TEST_BOOL", true); !got {
		t.Error("an unparseable value should use the fallback")
	}
}

func TestSetRegulatoryDomainRejectsBadCountry(t *testing.T) {
	// Validation happens before the netlink dial, so this is safe to run
	// without a radio.
	for _, country := range []string{"", "C", "CHE", "ch", "C1"} {
		if err := setRegulatoryDomain(country); err == nil {
			t.Errorf("setRegulatoryDomain(%q) accepted an invalid country", country)
		}
	}
}

func TestCharsToString(t *testing.T) {
	if got := charsToString([]byte{'6', '.', '1', 0, 'x'}); got != "6.1" {
		t.Errorf("charsToString = %q, want 6.1", got)
	}
	if got := charsToString([]byte("6.1")); got != "6.1" {
		t.Errorf("charsToString = %q, want 6.1", got)
	}
}

func TestHasCarrier(t *testing.T) {
	if _, err := hasCarrier("../etc/passwd"); err == nil {
		t.Error("hasCarrier accepted a path-traversing interface name")
	}
	if _, err := hasCarrier(""); err == nil {
		t.Error("hasCarrier accepted an empty interface name")
	}
	// A nonexistent interface is an error, not a silent false-with-nil.
	if _, err := hasCarrier("definitely-not-an-interface"); err == nil {
		t.Error("hasCarrier accepted a nonexistent interface")
	}
}

func TestFindModuleReportsMissing(t *testing.T) {
	// findModule walks /lib/modules/<release>, which does not contain a
	// module by this name on any machine running the test suite.
	if _, err := findModule("definitely-not-a-module"); err == nil {
		t.Error("expected an error for a missing module")
	}
}

func TestWaitForInterfaceTimeout(t *testing.T) {
	start := time.Now()
	if err := waitForInterface("definitely-not-an-interface", 100*time.Millisecond); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("waitForInterface blocked for %s", elapsed)
	}
}

func TestWiFiConfigured(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "absent.json")
	if wifiConfigured(missing) {
		t.Error("a missing config must not count as configured")
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte("  \n\t"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// An empty file would make the Wi-Fi client spin on a config it cannot
	// use, so it has to read as unconfigured.
	if wifiConfigured(empty) {
		t.Error("a whitespace-only config must not count as configured")
	}

	present := filepath.Join(dir, "wifi.json")
	if err := os.WriteFile(present, []byte(`{"ssid":"Home","psk":"password1"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !wifiConfigured(present) {
		t.Error("a populated config must count as configured")
	}
}

func TestHasCarrierOrFalse(t *testing.T) {
	if hasCarrierOrFalse("definitely-not-an-interface") {
		t.Error("a missing interface must not report a carrier")
	}
}

func TestSleepCtxCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleepCtx should report cancellation instead of sleeping")
	}
}

func TestSuperviseStopsOnContextCancel(t *testing.T) {
	// No config is present, so supervise parks in its poll loop; cancelling
	// must return promptly rather than block for configPollInterval.
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()

	done := make(chan struct{})
	go func() {
		defer close(done)
		supervise(ctx, "/nonexistent/wifi", filepath.Join(dir, "wifi.json"), false, "eth0")
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervise did not return after cancellation")
	}
}

func TestSuperviseRetriesFailingClient(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "wifi.json")
	if err := os.WriteFile(configPath, []byte(`{"ssid":"Home"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A client that exits immediately must not spin: the first retry waits
	// minBackoff, so within a short window we see exactly one attempt.
	script := filepath.Join(dir, "wifi-client")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	supervise(ctx, script, configPath, false, "eth0")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("supervise ran for %s, want it to honour the context", elapsed)
	}
}

func TestSetInterfaceUpRejectsLongName(t *testing.T) {
	if err := setInterfaceUp(strings.Repeat("x", 64)); err == nil {
		t.Error("expected an error for an oversized interface name")
	}
}

func TestBringRadioUpFailsWithoutHardware(t *testing.T) {
	// No wlan0 exists in the test environment, so this exercises the path the
	// device hit: modules cannot be loaded and the interface never appears.
	// It must return an error rather than calling log.Fatal — a fatal exit
	// here makes gokrazy respawn wifi-init in a tight loop.
	err := bringRadioUp("definitely-not-an-interface", "CH", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the interface never appears")
	}
	if !strings.Contains(err.Error(), "never appeared") {
		t.Errorf("error = %q, want it to name the missing interface", err)
	}
}

func TestWaitForRadioReturnsOnCancel(t *testing.T) {
	// The retry loop must be interruptible: a device that will never have a
	// radio should sit quietly, and shutdown must not wait a full interval.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		waitForRadio(ctx, "definitely-not-an-interface", "CH", 10*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForRadio ignored context cancellation")
	}
}

func TestFindModuleReportsMissingModuleTree(t *testing.T) {
	// The device reported `module "brcmutil" not found under
	// /lib/modules/6.12.47-v8`, which reads like a packaging mistake when the
	// real cause may be a kernel with no loadable modules at all.
	_, err := findModule("brcmutil")
	if err == nil {
		t.Skip("this machine has a matching module; nothing to assert")
	}
	if !strings.Contains(err.Error(), "brcmutil") && !strings.Contains(err.Error(), "loadable modules") {
		t.Errorf("error = %q, want it to name the module or the missing tree", err)
	}
}
