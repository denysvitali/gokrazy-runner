// Command runner-webui serves a small web UI for configuring the GitHub
// Actions runner on a gokrazy appliance. It listens on :8443 over HTTPS
// when a gokrazy TLS cert is available, otherwise on :8080 over plain HTTP.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/denysvitali/gokrazy-runner/pkg/ota"
	"github.com/denysvitali/gokrazy-runner/pkg/tlsconfig"
	"github.com/denysvitali/gokrazy-runner/pkg/webui"
)

const (
	permRoot   = "/perm"
	envFile    = "/perm/runner.env"
	tokenFile  = "/perm/runner.token"
	keysFile   = "/perm/breakglass/authorized_keys"
	dataDir    = "/perm/runner-data"
	pwPrimary  = "/perm/gokr-pw.txt"
	pwFallback = "/etc/gokr-pw.txt"

	defaultPassword = "gokrazy-runner"

	httpPort  = "8080"
	httpsPort = "8443"

	permWaitInterval = 10 * time.Second
	permWaitMax      = 60 * time.Second
)

var (
	Version   = "dev"
	BuildDate = "unknown"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("runner-webui starting (version=%s date=%s)", Version, BuildDate)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	waitForPerm(ctx)

	pm, err := webui.NewPasswordManager(pwPrimary, pwFallback, defaultPassword)
	if err != nil {
		log.Fatalf("password manager: %v", err)
	}

	otaMgr, err := ota.NewManager(ota.Options{
		Password: pm.Active,
	})
	if err != nil {
		log.Fatalf("ota manager: %v", err)
	}

	srv, err := webui.NewServer(webui.ServerConfig{
		EnvPath:          envFile,
		TokenPath:        tokenFile,
		KeysPath:         keysFile,
		DataDir:          dataDir,
		TailscaleKeyPath: webui.TailscaleAuthKeyFile,
		PasswordMgr:      pm,
		Version:          Version,
		OTAMgr:           otaMgr,
	})
	if err != nil {
		log.Fatalf("webui server: %v", err)
	}

	useHTTPS := false
	var certFile, keyFile string
	if os.Getenv("WEBUI_LISTEN_HTTP_ONLY") == "" {
		if cfg := tlsconfig.ResolveConfig(); cfg.CertificatesExist() {
			useHTTPS = true
			certFile, keyFile = cfg.CertFile, cfg.KeyFile
		} else {
			log.Printf("warning: gokrazy TLS cert not found at %s; falling back to plain HTTP. Set WEBUI_LISTEN_HTTP_ONLY to silence this.", tlsconfig.ResolveConfig().CertFile)
		}
	}

	addr := ":" + httpPort
	scheme := "http"
	if useHTTPS {
		addr = ":" + httpsPort
		scheme = "https"
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          tlsconfig.NewServerErrorLog(),
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "<device>"
	}
	port := httpPort
	if useHTTPS {
		port = httpsPort
	}
	log.Printf("listening on %s://%s:%s/", scheme, host, port)

	errCh := make(chan error, 1)
	go func() {
		if useHTTPS {
			errCh <- httpSrv.ListenAndServeTLS(certFile, keyFile)
		} else {
			errCh <- httpSrv.ListenAndServe()
		}
	}()

	select {
	case <-ctx.Done():
		log.Printf("shutdown signal received")
		shutdownCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Fatalf("shutdown: %v", err)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}
}

func waitForPerm(ctx context.Context) {
	deadline := time.Now().Add(permWaitMax)
	for {
		if _, err := os.Stat(permRoot); err == nil {
			return
		}
		if time.Now().After(deadline) {
			log.Printf("warning: %s not available after %s; proceeding with fallback password sources", permRoot, permWaitMax)
			return
		}
		log.Printf("waiting for %s...", permRoot)
		select {
		case <-ctx.Done():
			return
		case <-time.After(permWaitInterval):
		}
	}
}

