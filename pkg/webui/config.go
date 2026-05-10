// Package webui provides helpers for the on-device runner configuration UI.
package webui

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

type RunnerConfig struct {
	URL    string            `json:"url"`
	Name   string            `json:"name"`
	Labels string            `json:"labels"`
	Image  string            `json:"image"`
	Extra  map[string]string `json:"extra"`
}

var reservedKeys = map[string]string{
	"URL":          "URL",
	"REPO_URL":     "URL",
	"NAME":         "NAME",
	"RUNNER_NAME":  "NAME",
	"LABELS":       "LABELS",
	"IMAGE":        "IMAGE",
	"RUNNER_IMAGE": "IMAGE",
}

func ReadConfig(path string) (*RunnerConfig, error) {
	cfg := &RunnerConfig{Extra: map[string]string{}}

	f, err := os.Open(path) // #nosec G304 -- caller-controlled path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = stripQuotes(val)

		switch reservedKeys[key] {
		case "URL":
			cfg.URL = val
		case "NAME":
			cfg.Name = val
		case "LABELS":
			cfg.Labels = val
		case "IMAGE":
			cfg.Image = val
		default:
			cfg.Extra[key] = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func WriteConfig(path string, cfg *RunnerConfig) error {
	if cfg == nil {
		return errors.New("nil config")
	}

	named := []struct{ k, v string }{
		{"URL", cfg.URL},
		{"NAME", cfg.Name},
		{"LABELS", cfg.Labels},
		{"IMAGE", cfg.Image},
	}

	var lines []string
	for _, p := range named {
		if p.v == "" {
			continue
		}
		enc, err := encodeValue(p.v)
		if err != nil {
			return fmt.Errorf("%s: %w", p.k, err)
		}
		lines = append(lines, p.k+"="+enc)
	}

	extraKeys := make([]string, 0, len(cfg.Extra))
	for k := range cfg.Extra {
		if _, isReserved := reservedKeys[k]; isReserved {
			continue
		}
		extraKeys = append(extraKeys, k)
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		v := cfg.Extra[k]
		if v == "" {
			continue
		}
		enc, err := encodeValue(v)
		if err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		lines = append(lines, k+"="+enc)
	}

	out := strings.Join(lines, "\n")
	if out != "" {
		out += "\n"
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o644); err != nil { // #nosec G306 -- intentional 0644
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func stripQuotes(s string) string {
	if len(s) >= 2 {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func encodeValue(v string) (string, error) {
	if strings.ContainsAny(v, "\"\n") {
		return "", errors.New("value contains forbidden character (\" or newline)")
	}
	if strings.ContainsAny(v, " \t#") {
		return `"` + v + `"`, nil
	}
	return v, nil
}
