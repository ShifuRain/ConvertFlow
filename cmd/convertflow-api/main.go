package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db"
	"github.com/ShifuRain/convertflow/internal/janitor"
	"github.com/ShifuRain/convertflow/internal/jobs"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	for _, d := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o700); err != nil {
			return err
		}
	}
	store := jobs.NewStore(pool)
	al := audit.New(os.Stdout)
	go (&janitor.Janitor{Store: store, Audit: al, DataDir: cfg.DataDir, Retention: cfg.Retention,
		StaleAfter: cfg.StaleAfter, MaxAttempts: cfg.MaxAttempts, JobTimeout: cfg.JobTimeout, MaxQueueAge: cfg.MaxQueueAge, Flavor: cfg.Flavor,
		Interval: time.Minute}).Run(ctx)

	shutdownStarted := make(chan struct{})
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.NewHandler(api.Deps{Store: store, Cfg: cfg, Audit: al, Shutdown: shutdownStarted}),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: uploads are multi-GB and SSE is long-lived. The upload
		// handler extends a rolling per-read deadline through http.ResponseController instead.
		MaxHeaderBytes: 64 << 10,
		IdleTimeout:    120 * time.Second,
		TLSConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
	}
	srv.RegisterOnShutdown(func() { close(shutdownStarted) })
	errc := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "")
		if cfg.TLSCertFile != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	return shutdownServer(srv, shutdownTimeout)
}

const shutdownTimeout = 30 * time.Second // in-flight uploads and downloads get this long to finish

// shutdownServer stops accepting connections and waits up to timeout for in-flight requests
// (SSE streams end on their own via Deps.Shutdown). Past the timeout the remaining connections are
// closed forcibly; that is logged, not an error, so the process still exits 0.
func shutdownServer(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("graceful shutdown timed out; closing remaining connections", "timeout", timeout)
		return srv.Close()
	}
	return err
}

// healthcheck is used by the container HEALTHCHECK (distroless has no curl).
func healthcheck() int {
	addr := os.Getenv("CONVERTFLOW_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}
	if os.Getenv("CONVERTFLOW_TLS_CERT_FILE") != "" {
		scheme = "https"
		// #nosec G402 -- loopback health probe against our own listener
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	// #nosec G704 -- URL is built from our own listen address (loopback probe), never from request input
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s://%s/healthz", scheme, net.JoinHostPort(host, port)), nil)
	if err != nil {
		return 1
	}
	req.Host = net.JoinHostPort("127.0.0.1", port) // always an allowed Host, whatever CONVERTFLOW_LISTEN names
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
