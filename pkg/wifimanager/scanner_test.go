package wifimanager

import (
	"testing"

	"github.com/mdlayher/wifi"
)

func TestConvertSignalToDBM(t *testing.T) {
	tests := []struct {
		name    string
		mBm     int32
		percent uint32
		want    int
	}{
		{"negative mBm rounds away from zero", -7250, 0, -73},
		{"exact mBm", -6700, 0, -67},
		{"positive mBm", 4550, 0, 46},
		{"percent fallback at 100", 0, 100, -40},
		{"percent fallback at 50", 0, 50, -70},
		{"percent fallback clamped above 100", 0, 250, -40},
		{"no signal at all", 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := convertSignalToDBM(tc.mBm, tc.percent); got != tc.want {
				t.Fatalf("convertSignalToDBM(%d, %d) = %d, want %d", tc.mBm, tc.percent, got, tc.want)
			}
		})
	}
}

func encryptedBSS(ssid string, freq int, mBm int32) *wifi.BSS {
	return &wifi.BSS{
		SSID:      ssid,
		Frequency: freq,
		Signal:    mBm,
		RSN:       wifi.RSNInfo{PairwiseCiphers: []wifi.RSNCipher{wifi.RSNCipherCCMP128}},
	}
}

func TestProcessAccessPointsDedupesBySSID(t *testing.T) {
	aps := []*wifi.BSS{
		encryptedBSS("Home", 2412, -5000),
		encryptedBSS("Home", 5180, -7000), // same SSID, 5 GHz, weaker
		{SSID: "", Frequency: 2412},       // hidden
		{SSID: "Open", Frequency: 2437, Signal: -6000},
	}

	got := map[string]ScanResult{}
	processAccessPoints(aps, got, true)

	if len(got) != 2 {
		t.Fatalf("got %d networks, want 2: %+v", len(got), got)
	}
	// prefer5GHz wins even though the 2.4 GHz BSS is stronger.
	if got["Home"].Frequency != 5180 {
		t.Errorf("Home frequency = %d, want 5180", got["Home"].Frequency)
	}
	if !got["Home"].Encrypted {
		t.Error("Home should be reported as encrypted")
	}
	if got["Open"].Encrypted {
		t.Error("Open should be reported as unencrypted")
	}

	// Without the preference, the strongest BSS wins instead.
	got = map[string]ScanResult{}
	processAccessPoints(aps, got, false)
	if got["Home"].Frequency != 2412 {
		t.Errorf("Home frequency = %d, want 2412 with prefer5GHz off", got["Home"].Frequency)
	}
}

func TestProcessAccessPointsKeepsStrongestWithinBand(t *testing.T) {
	aps := []*wifi.BSS{
		encryptedBSS("Mesh", 5180, -7500),
		encryptedBSS("Mesh", 5745, -4500),
	}
	got := map[string]ScanResult{}
	processAccessPoints(aps, got, true)

	if got["Mesh"].Signal != -45 {
		t.Fatalf("Mesh signal = %d, want -45", got["Mesh"].Signal)
	}
}

func TestIs5GHzFrequency(t *testing.T) {
	for freq, want := range map[int]bool{2412: false, 4999: false, 5180: true, 5999: true, 6000: false} {
		if got := is5GHzFrequency(freq); got != want {
			t.Errorf("is5GHzFrequency(%d) = %v, want %v", freq, got, want)
		}
	}
}
