package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestBootstrapAllowsAutomaticUpdates(t *testing.T) {
	if strings.Contains(bootstrap, "--disableupdate") {
		t.Fatal("bootstrap must not disable GitHub's automatic runner updates")
	}
	if !strings.Contains(bootstrap, "--unattended --replace") {
		t.Fatal("bootstrap lost the unattended registration flags")
	}
}

func TestRunnerHomePopulated(t *testing.T) {
	dir := t.TempDir()
	if runnerHomePopulated(dir) {
		t.Fatal("empty runner home reported as populated")
	}

	configPath := filepath.Join(dir, "config.sh")
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if runnerHomePopulated(dir) {
		t.Fatal("config.sh directory reported as a runner installation")
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !runnerHomePopulated(dir) {
		t.Fatal("config.sh file did not mark runner home as populated")
	}
}

func TestPopulateRunnerHomeUsesHostNetwork(t *testing.T) {
	args := buildPopulateRunnerHomeArgs("example.invalid/runner:latest")
	if !slices.Contains(args, "--network=host") {
		t.Fatalf("populate args must bypass Podman's nftables-backed default network: %q", args)
	}
}

func TestRefreshRunnerHomePolicy(t *testing.T) {
	pullFailure := errors.New("registry unavailable")
	populateFailure := errors.New("copy failed")
	tests := []struct {
		name              string
		pullErr           error
		alreadyPopulated  bool
		populateErr       error
		wantPopulate      bool
		wantPopulateState bool
		wantErr           error
	}{
		{
			name:              "successful pull refreshes existing installation",
			alreadyPopulated:  true,
			wantPopulate:      true,
			wantPopulateState: true,
		},
		{
			name:             "failed pull retains existing installation",
			pullErr:          pullFailure,
			alreadyPopulated: true,
		},
		{
			name:         "failed pull seeds empty home from cache",
			pullErr:      pullFailure,
			wantPopulate: true,
		},
		{
			name:         "populate failure is returned",
			populateErr:  populateFailure,
			wantPopulate: true,
			wantErr:      populateFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pullCalls := 0
			populateCalls := 0
			gotPopulateState := false
			err := refreshRunnerHomeWith(
				context.Background(),
				"example.invalid/runner:latest",
				func(context.Context, string) error {
					pullCalls++
					return tt.pullErr
				},
				func(string) bool { return tt.alreadyPopulated },
				func(_ context.Context, _ string, populated bool) error {
					populateCalls++
					gotPopulateState = populated
					return tt.populateErr
				},
			)
			if pullCalls != 1 {
				t.Errorf("pull calls = %d, want 1", pullCalls)
			}
			if got := populateCalls == 1; got != tt.wantPopulate {
				t.Errorf("populate called = %v, want %v", got, tt.wantPopulate)
			}
			if tt.wantPopulate && gotPopulateState != tt.wantPopulateState {
				t.Errorf("populate existing state = %v, want %v", gotPopulateState, tt.wantPopulateState)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestEnableAutomaticUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runner")
	const original = `{"AgentId":9007199254740993,"AgentName":"pi","DisableUpdate":true}`
	contents := append([]byte{0xef, 0xbb, 0xbf}, []byte(original)...)
	if err := os.WriteFile(path, contents, 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := enableAutomaticUpdates(path)
	if err != nil {
		t.Fatalf("enableAutomaticUpdates: %v", err)
	}
	if !changed {
		t.Fatal("DisableUpdate=true was not migrated")
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := after.Mode().Perm(), before.Mode().Perm(); got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
	beforeStat, beforeOK := before.Sys().(*syscall.Stat_t)
	afterStat, afterOK := after.Sys().(*syscall.Stat_t)
	if beforeOK && afterOK && (beforeStat.Uid != afterStat.Uid || beforeStat.Gid != afterStat.Gid) {
		t.Errorf("owner changed from %d:%d to %d:%d", beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatalf("updated settings are invalid JSON: %v", err)
	}
	if got := string(settings["AgentId"]); got != "9007199254740993" {
		t.Errorf("AgentId = %s, want exact original value", got)
	}
	if got := string(settings["DisableUpdate"]); got != "false" {
		t.Errorf("DisableUpdate = %s, want false", got)
	}
}

func TestEnableAutomaticUpdatesAlreadyEnabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runner")
	original := []byte(`{"AgentId":42,"DisableUpdate":false}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := enableAutomaticUpdates(path)
	if err != nil {
		t.Fatalf("enableAutomaticUpdates: %v", err)
	}
	if changed {
		t.Fatal("already-enabled settings reported as changed")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(original) {
		t.Errorf("already-enabled settings were rewritten: %q", b)
	}
}

func TestEnableAutomaticUpdatesMissingSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runner")
	changed, err := enableAutomaticUpdates(path)
	if err != nil {
		t.Fatalf("missing .runner: %v", err)
	}
	if changed {
		t.Fatal("missing .runner reported as changed")
	}
}

func TestEnableAutomaticUpdatesRejectsInvalidSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runner")
	original := []byte(`{"DisableUpdate":`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := enableAutomaticUpdates(path); err == nil {
		t.Fatal("invalid .runner JSON was accepted")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(original) {
		t.Errorf("invalid settings were modified: %q", b)
	}
}
