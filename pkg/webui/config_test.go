package webui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadConfigMissingFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := ReadConfig(filepath.Join(dir, "does-not-exist.env"))
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil cfg")
	}
	if cfg.Extra == nil {
		t.Fatal("expected non-nil Extra map")
	}
	if cfg.URL != "" || cfg.Name != "" || cfg.Labels != "" || cfg.Image != "" {
		t.Fatalf("expected zero-valued cfg, got %+v", cfg)
	}
	if len(cfg.Extra) != 0 {
		t.Fatalf("expected empty Extra, got %v", cfg.Extra)
	}
}

func TestReadConfigEmptyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Extra == nil || len(cfg.Extra) != 0 {
		t.Fatalf("expected empty non-nil Extra, got %v", cfg.Extra)
	}
}

func TestReadConfigAliasesAndQuotes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	body := `# top comment
REPO_URL=https://github.com/owner/repo
RUNNER_NAME="my runner"
LABELS='self-hosted,linux,arm64'
RUNNER_IMAGE=ghcr.io/example/runner:tag

# extras
ACCESS_TOKEN=abc123
RUNNER_GROUP="prod group"
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://github.com/owner/repo" {
		t.Errorf("URL: got %q", cfg.URL)
	}
	if cfg.Name != "my runner" {
		t.Errorf("Name: got %q", cfg.Name)
	}
	if cfg.Labels != "self-hosted,linux,arm64" {
		t.Errorf("Labels: got %q", cfg.Labels)
	}
	if cfg.Image != "ghcr.io/example/runner:tag" {
		t.Errorf("Image: got %q", cfg.Image)
	}
	want := map[string]string{
		"ACCESS_TOKEN": "abc123",
		"RUNNER_GROUP": "prod group",
	}
	if !reflect.DeepEqual(cfg.Extra, want) {
		t.Errorf("Extra: got %v, want %v", cfg.Extra, want)
	}
}

func TestReadConfigCanonicalKeysWinOnReadOrder(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	body := "URL=canonical\nNAME=alpha\nIMAGE=img1\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "canonical" || cfg.Name != "alpha" || cfg.Image != "img1" {
		t.Errorf("got %+v", cfg)
	}
	if len(cfg.Extra) != 0 {
		t.Errorf("Extra should be empty, got %v", cfg.Extra)
	}
}

func TestReadConfigSkipsBlankAndComments(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	body := "\n\n# a comment\n   # indented comment\nURL=u\n=novalue\nbadline\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "u" {
		t.Errorf("URL: got %q", cfg.URL)
	}
	if len(cfg.Extra) != 0 {
		t.Errorf("Extra should be empty, got %v", cfg.Extra)
	}
}

func TestWriteConfigBasic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	cfg := &RunnerConfig{
		URL:    "https://github.com/owner/repo",
		Name:   "node1",
		Labels: "self-hosted,linux",
		Image:  "img:latest",
		Extra: map[string]string{
			"ZETA":  "z",
			"ALPHA": "a",
		},
	}
	if err := WriteConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "URL=https://github.com/owner/repo\nNAME=node1\nLABELS=self-hosted,linux\nIMAGE=img:latest\nALPHA=a\nZETA=z\n"
	if string(data) != want {
		t.Errorf("got:\n%s\nwant:\n%s", data, want)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("tmp file should not exist after success: %v", err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode: got %v want 0644", info.Mode().Perm())
	}
}

func TestWriteConfigSkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	cfg := &RunnerConfig{
		URL: "u",
		Extra: map[string]string{
			"EMPTY": "",
			"FOO":   "bar",
		},
	}
	if err := WriteConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "URL=u\nFOO=bar\n" {
		t.Errorf("got %q", data)
	}
}

func TestWriteConfigQuotesWhenNeeded(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	cfg := &RunnerConfig{
		URL:  "u",
		Name: "with space",
		Extra: map[string]string{
			"HASHY":  "has#hash",
			"TABBED": "a\tb",
			"PLAIN":  "plain",
		},
	}
	if err := WriteConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	got := string(data)
	for _, sub := range []string{
		`URL=u`,
		`NAME="with space"`,
		`HASHY="has#hash"`,
		"TABBED=\"a\tb\"",
		`PLAIN=plain`,
	} {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in:\n%s", sub, got)
		}
	}
}

func TestWriteConfigRejectsForbiddenChars(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	for _, tc := range []struct {
		name string
		cfg  *RunnerConfig
	}{
		{"named-quote", &RunnerConfig{URL: `bad"value`}},
		{"named-newline", &RunnerConfig{URL: "bad\nvalue"}},
		{"extra-quote", &RunnerConfig{URL: "u", Extra: map[string]string{"K": `x"y`}}},
		{"extra-newline", &RunnerConfig{URL: "u", Extra: map[string]string{"K": "x\ny"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := WriteConfig(p, tc.cfg)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestWriteConfigIgnoresReservedInExtra(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	cfg := &RunnerConfig{
		URL: "canonical",
		Extra: map[string]string{
			"URL":          "shadow1",
			"REPO_URL":     "shadow2",
			"NAME":         "shadow3",
			"RUNNER_NAME":  "shadow4",
			"LABELS":       "shadow5",
			"IMAGE":        "shadow6",
			"RUNNER_IMAGE": "shadow7",
			"OK":           "fine",
		},
	}
	if err := WriteConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	got := string(data)
	if !strings.Contains(got, "URL=canonical\n") {
		t.Errorf("expected URL=canonical, got:\n%s", got)
	}
	if !strings.Contains(got, "OK=fine\n") {
		t.Errorf("expected OK=fine, got:\n%s", got)
	}
	for _, sub := range []string{"shadow1", "shadow2", "shadow3", "shadow4", "shadow5", "shadow6", "shadow7"} {
		if strings.Contains(got, sub) {
			t.Errorf("reserved Extra key leaked %q into output:\n%s", sub, got)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	original := &RunnerConfig{
		URL:    "https://github.com/owner/repo",
		Name:   "with space",
		Labels: "self-hosted,linux,arm64",
		Image:  "ghcr.io/example/runner:tag",
		Extra: map[string]string{
			"ACCESS_TOKEN": "abc",
			"RUNNER_GROUP": "prod group",
			"HASHY":        "has#hash",
		},
	}
	if err := WriteConfig(p, original); err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != original.URL || got.Name != original.Name || got.Labels != original.Labels || got.Image != original.Image {
		t.Errorf("named fields differ: got %+v want %+v", got, original)
	}
	if !reflect.DeepEqual(got.Extra, original.Extra) {
		t.Errorf("Extra differs: got %v want %v", got.Extra, original.Extra)
	}
}

func TestWriteConfigAtomicTmpCleanedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	if err := WriteConfig(p, &RunnerConfig{URL: "u"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestWriteConfigOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	if err := os.WriteFile(p, []byte("STALE=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(p, &RunnerConfig{URL: "u"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if string(data) != "URL=u\n" {
		t.Errorf("expected overwrite, got %q", data)
	}
}

func TestWriteConfigNil(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runner.env")
	if err := WriteConfig(p, nil); err == nil {
		t.Fatal("expected error for nil cfg")
	}
}
