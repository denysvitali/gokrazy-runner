package webui

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsSensitiveEnvKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"GITHUB_TOKEN", true},
		{"RUNNER_TOKEN", true},
		{"MY_API_KEY", true},
		{"AWS_SECRET_ACCESS_KEY", true},
		{"PASSWORD", true},
		{"DB_PASSWD", true},
		{"AUTHKEY", true}, // ends with KEY
		{"AUTH_KEY", true},
		{"COOKIE", true},
		{"SESSION_ID", true},
		{"PRIVATE_TOKEN", true},
		{"URL", false},
		{"REPO_URL", false},
		{"NAME", false},
		{"LABELS", false},
		{"IMAGE", false},
		{"DEBUG", false},
		{"MONKEY", false},   // contains "KEY" but not at end as a word — we treat suffix only
		{"KEYCLOAK", false}, // starts with KEY but not sensitive substring
	}
	for _, c := range cases {
		got := isSensitiveEnvKey(c.key)
		if got != c.want {
			t.Errorf("isSensitiveEnvKey(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

func TestReadRedactedRunnerEnv(t *testing.T) {
	read := func(path string) ([]byte, error) {
		return []byte(`# header comment
URL=https://github.com/foo/bar
NAME=runner-1
LABELS=self-hosted,linux
GITHUB_TOKEN="ghp_supersecretvalue"
MY_API_KEY=abcdef
DEBUG=1
EMPTY_TOKEN=
`), nil
	}
	out, err := readRedactedRunnerEnv(read, "/perm/runner.env")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	mustContain(t, out, "URL=https://github.com/foo/bar")
	mustContain(t, out, "NAME=runner-1")
	mustContain(t, out, "DEBUG=1")
	mustContain(t, out, "# header comment")
	mustContain(t, out, "GITHUB_TOKEN=**redacted (")
	mustContain(t, out, "MY_API_KEY=**redacted (")
	mustContain(t, out, "EMPTY_TOKEN=(empty)")
	if strings.Contains(out, "ghp_supersecretvalue") {
		t.Fatalf("redacted output leaked secret value: %s", out)
	}
	if strings.Contains(out, "abcdef") {
		t.Fatalf("redacted output leaked secret value: %s", out)
	}
}

func TestReadRedactedRunnerEnvNotFound(t *testing.T) {
	read := func(string) ([]byte, error) {
		return nil, &fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}
	}
	out, err := readRedactedRunnerEnv(read, "/missing")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != "(not present)" {
		t.Fatalf("unexpected output: %q", out)
	}
}

func TestDescribeFilePresence(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		out, err := describeFilePresence(func(string) ([]byte, error) {
			return nil, &fs.PathError{Op: "open", Path: "x", Err: fs.ErrNotExist}
		}, "/perm/x")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		mustContain(t, out, "not present at /perm/x")
	})

	t.Run("present", func(t *testing.T) {
		out, err := describeFilePresence(func(string) ([]byte, error) {
			return []byte("hello"), nil
		}, "/perm/x")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		mustContain(t, out, "present at /perm/x (5 bytes)")
		if strings.Contains(out, "hello") {
			t.Fatalf("file body must not appear in output: %s", out)
		}
	})

	t.Run("empty", func(t *testing.T) {
		out, err := describeFilePresence(func(string) ([]byte, error) {
			return []byte("\n  \n"), nil
		}, "/perm/x")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		mustContain(t, out, "but empty")
	})
}

func TestSupportEndpointIntegration(t *testing.T) {
	s, dir, _ := newTestServer(t)
	envPath := filepath.Join(dir, "runner.env")
	if err := os.WriteFile(envPath, []byte("URL=https://x/y\nGITHUB_TOKEN=secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "runner.token")
	if err := os.WriteFile(tokenPath, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Patch in stub paths/binaries for the support bundle. The defaults
	// would point at /user/podman etc., which don't exist on the test
	// host — runWithTimeout handles that by returning a "(binary not
	// present...)" string per section.
	s.cfg.EnvPath = envPath
	s.cfg.TokenPath = tokenPath
	s.cfg.Support = SupportOptions{
		PermDir:         dir,
		OTAHistoryPath:  filepath.Join(dir, "ota-install-history.json"),
		PodmanBinary:    filepath.Join(dir, "no-podman"),
		TailscaleBinary: filepath.Join(dir, "no-tailscale"),
		ContainerName:   "gokrazy-runner",

		readFile: func(path string) ([]byte, error) {
			switch path {
			case "/etc/resolv.conf":
				return []byte("nameserver 1.1.1.1\n"), nil
			case "/etc/hosts":
				return []byte("127.0.0.1 localhost\n"), nil
			case "/etc/nsswitch.conf":
				return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
			case "/proc/uptime":
				return []byte("12345.67 11000.00\n"), nil
			default:
				return os.ReadFile(path)
			}
		},
		runCmd: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return nil, errors.New("not invoked")
		},
		readKmsg: func(int) (string, error) {
			return "kernel: hello\n", nil
		},
		now:        func() time.Time { return time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC) },
		hostname:   func() string { return "test-host" },
		interfaces: func() ([]net.Interface, error) { return nil, nil },
	}

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "GET", "/api/support", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content-type: %q", got)
	}
	body := rr.Body.String()
	mustContain(t, body, "gokrazy-runner support bundle")
	mustContain(t, body, "hostname:  test-host")
	mustContain(t, body, "uptime:    12345.67s")
	mustContain(t, body, "==== /etc/resolv.conf ====")
	mustContain(t, body, "nameserver 1.1.1.1")
	mustContain(t, body, "==== runner.env (redacted) ====")
	mustContain(t, body, "URL=https://x/y")
	mustContain(t, body, "GITHUB_TOKEN=**redacted (")
	mustContain(t, body, "==== runner.token ====")
	mustContain(t, body, "present at "+tokenPath)
	mustContain(t, body, "==== Kernel log")
	mustContain(t, body, "kernel: hello")
	mustContain(t, body, "(binary not present at "+filepath.Join(dir, "no-podman")+")")
	if strings.Contains(body, "secret") {
		t.Fatalf("secret value leaked in support bundle: %s", body)
	}
	if strings.Contains(body, "AAAA") {
		t.Fatalf("token contents leaked in support bundle: %s", body)
	}
}

func TestSupportEndpointMethodNotAllowed(t *testing.T) {
	s, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "POST", "/api/support", map[string]string{}, "correct-horse", "application/json"))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestSupportEndpointAuth(t *testing.T) {
	s, _, _ := newTestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/support", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestTailLines(t *testing.T) {
	in := "a\nb\nc\nd\ne\n"
	if got := tailLines(in, 2); got != "d\ne\n" {
		t.Fatalf("got %q", got)
	}
	if got := tailLines(in, 10); got != in {
		t.Fatalf("got %q", got)
	}
	if got := tailLines("", 5); got != "" {
		t.Fatalf("got %q", got)
	}
}

func mustContain(t *testing.T, body, sub string) {
	t.Helper()
	if !strings.Contains(body, sub) {
		t.Fatalf("body missing %q\n----\n%s", sub, body)
	}
}
