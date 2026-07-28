package main

import (
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

func TestWaitForEthernetCarrierMissingInterface(t *testing.T) {
	// A device with no eth0 must fall through to enabling Wi-Fi rather than
	// blocking for the whole timeout.
	start := time.Now()
	if waitForEthernetCarrier("definitely-not-an-interface", 5*time.Second) {
		t.Fatal("reported a carrier on a nonexistent interface")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s for a nonexistent interface", elapsed)
	}
}
