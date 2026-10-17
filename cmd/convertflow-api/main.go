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
		StaleAfter: cfg.StaleAfter, MaxAttempts: cfg.MaxAttempts, JobTimeout: cfg.JobTimeout,
		Interval: time.Minute}).Run(ctx)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.NewHandler(api.Deps{Store: store, Cfg: cfg, Audit: al}),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout/WriteTimeout: uploads are multi-GB and SSE is long-lived. The upload
		// handler extends a rolling per-read deadline through http.ResponseController instead.
		MaxHeaderBytes: 64 << 10,
		IdleTimeout:    120 * time.Second,
		TLSConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
	}
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
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
	resp, err := client.Get(fmt.Sprintf("%s://%s/healthz", scheme, net.JoinHostPort(host, port)))
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
