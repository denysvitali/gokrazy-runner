package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/denysvitali/gokrazy-runner/pkg/ota"
	"github.com/gokrazy/gokapi"
	"github.com/gokrazy/gokapi/ondeviceapi"
)

type ServerConfig struct {
	EnvPath          string
	TokenPath        string
	KeysPath         string
	DataDir          string
	TailscaleKeyPath string
	PasswordMgr      *PasswordManager
	Version          string
	Reboot           func(ctx context.Context) error
	// OTAMgr handles GitHub-release-driven OTA updates. Optional; when nil
	// the /api/ota/* endpoints return 503 Service Unavailable.
	OTAMgr *ota.Manager
	// Support overrides the diagnostics endpoint defaults. Zero values
	// are filled in at request time.
	Support SupportOptions
}

type Server struct {
	cfg     ServerConfig
	handler http.Handler
}

func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.PasswordMgr == nil {
		return nil, errors.New("PasswordMgr is required")
	}
	if cfg.EnvPath == "" {
		return nil, errors.New("EnvPath is required")
	}
	if cfg.TokenPath == "" {
		return nil, errors.New("TokenPath is required")
	}
	if cfg.KeysPath == "" {
		return nil, errors.New("KeysPath is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("DataDir is required")
	}
	if cfg.TailscaleKeyPath == "" {
		cfg.TailscaleKeyPath = TailscaleAuthKeyFile
	}
	if cfg.Reboot == nil {
		cfg.Reboot = defaultReboot
	}

	s := &Server{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.Handle("/static/", StaticHandler("/static/"))
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/token", s.handleToken)
	mux.HandleFunc("/api/keys", s.handleKeys)
	mux.HandleFunc("/api/password", s.handlePassword)
	mux.HandleFunc("/api/tailscale", s.handleTailscale)
	mux.HandleFunc("/api/reboot", s.handleReboot)
	mux.HandleFunc("/api/ota/status", s.handleOTAStatus)
	mux.HandleFunc("/api/ota/install", s.handleOTAInstall)
	mux.HandleFunc("/api/support", s.handleSupport)

	s.handler = s.logMiddleware(s.authMiddleware(s.securityHeadersMiddleware(mux)))
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("webui: %s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pw, ok := r.BasicAuth()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="gokrazy-runner"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.cfg.PasswordMgr.Verify(pw) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := IndexHTML()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tailscaleConfigured, err := HasTailscaleAuthKey(s.cfg.TailscaleKeyPath)
	if err != nil {
		log.Printf("webui: read tailscale auth key status: %v", err)
	}
	resp := map[string]any{
		"has_token":            TokenExists(s.cfg.TokenPath),
		"has_runner_data":      dirExists(s.cfg.DataDir),
		"version":              s.cfg.Version,
		"password_is_default":  s.cfg.PasswordMgr.IsDefault(),
		"tailscale_configured": tailscaleConfigured,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleTailscale(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		configured, err := HasTailscaleAuthKey(s.cfg.TailscaleKeyPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": configured,
			"key_path":   s.cfg.TailscaleKeyPath,
		})
	case http.MethodPost:
		if !requireJSON(w, r) {
			return
		}
		var body struct {
			AuthKey string `json:"auth_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		key := strings.TrimSpace(body.AuthKey)
		if err := ValidateTailscaleAuthKey(key); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := ConfigureTailscale(r.Context(), s.cfg.TailscaleKeyPath, key); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := ReadConfig(s.cfg.EnvPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if cfg.Extra == nil {
			cfg.Extra = map[string]string{}
		}
		writeJSON(w, http.StatusOK, cfg)
	case http.MethodPost:
		if !requireJSON(w, r) {
			return
		}
		var cfg RunnerConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(cfg.URL) == "" {
			http.Error(w, "url is required", http.StatusBadRequest)
			return
		}
		if err := WriteConfig(s.cfg.EnvPath, &cfg); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := WriteToken(s.cfg.TokenPath, body.Token); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keys, err := ReadAuthorizedKeys(s.cfg.KeysPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"keys": keys})
	case http.MethodPost:
		if !requireJSON(w, r) {
			return
		}
		var body struct {
			Keys string `json:"keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := WriteAuthorizedKeys(s.cfg.KeysPath, body.Keys); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSON(w, r) {
		return
	}
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.cfg.PasswordMgr.Verify(body.Old) {
		http.Error(w, "current password is incorrect", http.StatusUnauthorized)
		return
	}
	if err := s.cfg.PasswordMgr.Set(body.New); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rebooting"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		if err := s.cfg.Reboot(context.Background()); err != nil {
			log.Printf("webui: reboot failed: %v", err)
		}
	}()
}

func requireJSON(w http.ResponseWriter, r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	// Allow optional charset/parameters.
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(ct)
	if !strings.EqualFold(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func dirExists(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.IsDir()
}

func defaultReboot(ctx context.Context) error {
	cfg, err := gokapi.ConnectOnDevice()
	if err != nil {
		return fmt.Errorf("gokapi connect: %w", err)
	}
	cl := ondeviceapi.NewAPIClient(cfg)
	if _, err := cl.UpdateApi.Reboot(ctx, &ondeviceapi.UpdateApiRebootOpts{}); err != nil {
		return fmt.Errorf("reboot: %w", err)
	}
	return nil
}
