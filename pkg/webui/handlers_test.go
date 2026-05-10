package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newTestServer(t *testing.T) (*Server, string, *PasswordManager) {
	t.Helper()
	dir := t.TempDir()
	pwPath := filepath.Join(dir, "pw.txt")
	pm, err := NewPasswordManager(pwPath, "", "defaultpw")
	if err != nil {
		t.Fatalf("NewPasswordManager: %v", err)
	}
	if err := pm.Set("correct-horse"); err != nil {
		t.Fatalf("set initial password: %v", err)
	}
	dataDir := filepath.Join(dir, "runner-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	cfg := ServerConfig{
		EnvPath:     filepath.Join(dir, "runner.env"),
		TokenPath:   filepath.Join(dir, "runner.token"),
		KeysPath:    filepath.Join(dir, "breakglass", "authorized_keys"),
		DataDir:     dataDir,
		PasswordMgr: pm,
		Version:     "test-1.2.3",
		Reboot:      func(ctx context.Context) error { return nil },
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, dir, pm
}

func authReq(t *testing.T, method, target string, body any, pw string, contentType string) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		r = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, r)
	if pw != "" {
		req.SetBasicAuth("admin", pw)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req
}

func TestStatusAuth(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	// no creds
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/status", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no-creds: got %d", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Basic") {
		t.Fatalf("missing WWW-Authenticate: %q", got)
	}

	// wrong password
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "GET", "/api/status", nil, "wrong", ""))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-creds: got %d", rr.Code)
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != "" {
		t.Fatalf("WWW-Authenticate should be absent on wrong-creds: %q", got)
	}

	// correct password
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "GET", "/api/status", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("correct-creds: got %d body=%s", rr.Code, rr.Body.String())
	}
	var status map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status["version"] != "test-1.2.3" {
		t.Fatalf("version: %v", status["version"])
	}
	if status["has_runner_data"] != true {
		t.Fatalf("has_runner_data: %v", status["has_runner_data"])
	}
	if status["has_token"] != false {
		t.Fatalf("has_token: %v", status["has_token"])
	}
	if status["password_is_default"] != false {
		t.Fatalf("password_is_default: %v", status["password_is_default"])
	}
}

func TestConfigGetRoundTrip(t *testing.T) {
	s, dir, _ := newTestServer(t)
	h := s.Handler()

	// seed runner.env
	if err := os.WriteFile(filepath.Join(dir, "runner.env"),
		[]byte("URL=https://github.com/foo/bar\nNAME=runner-1\nMY_VAR=hello\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "GET", "/api/config", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	var got RunnerConfig
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.URL != "https://github.com/foo/bar" {
		t.Fatalf("URL: %q", got.URL)
	}
	if got.Name != "runner-1" {
		t.Fatalf("Name: %q", got.Name)
	}
	if got.Extra == nil || got.Extra["MY_VAR"] != "hello" {
		t.Fatalf("Extra: %v", got.Extra)
	}
}

func TestConfigPostWritesAndGetsBack(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	body := RunnerConfig{
		URL:    "https://github.com/foo/bar",
		Name:   "runner-x",
		Labels: "self-hosted,arm64",
		Image:  "ghcr.io/example/runner:latest",
		Extra:  map[string]string{"FOO": "bar"},
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "POST", "/api/config", body, "correct-horse", "application/json"))
	if rr.Code != http.StatusOK {
		t.Fatalf("post: %d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "GET", "/api/config", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}
	var got RunnerConfig
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.URL != body.URL || got.Name != body.Name || got.Labels != body.Labels || got.Image != body.Image {
		t.Fatalf("mismatch: %+v", got)
	}
	if got.Extra["FOO"] != "bar" {
		t.Fatalf("extra: %+v", got.Extra)
	}
}

func TestConfigPostEmptyURLReturns400(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "POST", "/api/config", RunnerConfig{}, "correct-horse", "application/json"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTokenPostWritesMode0600(t *testing.T) {
	s, dir, _ := newTestServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "POST", "/api/token",
		map[string]string{"token": "ABCDEF"}, "correct-horse", "application/json"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	tokPath := filepath.Join(dir, "runner.token")
	fi, err := os.Stat(tokPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want 0600", mode)
	}
	data, err := os.ReadFile(tokPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != "ABCDEF" {
		t.Fatalf("contents: %q", data)
	}
}

func TestKeysRoundTrip(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "GET", "/api/keys", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("get0: %d", rr.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["keys"] != "" {
		t.Fatalf("expected empty, got %q", got["keys"])
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "POST", "/api/keys",
		map[string]string{"keys": "ssh-ed25519 AAAA test\n"}, "correct-horse", "application/json"))
	if rr.Code != http.StatusOK {
		t.Fatalf("post: %d body=%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "GET", "/api/keys", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("get1: %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["keys"] != "ssh-ed25519 AAAA test\n" {
		t.Fatalf("got %q", got["keys"])
	}
}

func TestPasswordHappyPath(t *testing.T) {
	s, _, pm := newTestServer(t)
	h := s.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "POST", "/api/password",
		map[string]string{"old": "correct-horse", "new": "newpassword1"},
		"correct-horse", "application/json"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if !pm.Verify("newpassword1") {
		t.Fatalf("password not updated")
	}
}

func TestPasswordWrongOld(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "POST", "/api/password",
		map[string]string{"old": "wrong", "new": "newpassword1"},
		"correct-horse", "application/json"))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "current password is incorrect") {
		t.Fatalf("body: %s", rr.Body.String())
	}
}

func TestPasswordSetValidationError(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "POST", "/api/password",
		map[string]string{"old": "correct-horse", "new": "short"},
		"correct-horse", "application/json"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "at least 8") {
		t.Fatalf("body: %s", rr.Body.String())
	}
}

func TestRebootCallsInjected(t *testing.T) {
	dir := t.TempDir()
	pwPath := filepath.Join(dir, "pw.txt")
	pm, err := NewPasswordManager(pwPath, "", "defaultpw")
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Set("correct-horse"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	called := make(chan struct{}, 1)
	cfg := ServerConfig{
		EnvPath:     filepath.Join(dir, "runner.env"),
		TokenPath:   filepath.Join(dir, "runner.token"),
		KeysPath:    filepath.Join(dir, "authorized_keys"),
		DataDir:     dir,
		PasswordMgr: pm,
		Reboot: func(ctx context.Context) error {
			defer wg.Done()
			called <- struct{}{}
			return nil
		},
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, authReq(t, "POST", "/api/reboot", nil, "correct-horse", "application/json"))
	if rr.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "rebooting") {
		t.Fatalf("body: %s", rr.Body.String())
	}
	wg.Wait()
	select {
	case <-called:
	default:
		t.Fatal("reboot not called")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "DELETE", "/api/status", nil, "correct-horse", ""))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestPostRequiresJSONContentType(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/config", strings.NewReader(`{"url":"x"}`))
	req.SetBasicAuth("u", "correct-horse")
	// no Content-Type
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d", rr.Code)
	}
}

func TestStaticAssetServed(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "GET", "/static/app.js", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	if rr.Body.Len() == 0 {
		t.Fatalf("empty body")
	}
}

func TestRootServesIndex(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authReq(t, "GET", "/", nil, "correct-horse", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type: %q", ct)
	}
}
