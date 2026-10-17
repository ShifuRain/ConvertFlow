package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
)

func handler(t *testing.T, cfg config.Config) (*httptest.Server, func()) {
	t.Helper()
	pool := dbtest.New(t)
	cfg.DataDir = t.TempDir()
	h := api.NewHandler(api.Deps{Store: jobs.NewStore(pool), Cfg: cfg, Audit: audit.New(io.Discard)})
	return httptest.NewUnstartedServer(h), pool.Close
}

func listen(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Setenv("CONVERTFLOW_LISTEN", strings.TrimPrefix(srv.URL, "http://"))
}

func TestHealthcheckEndToEnd(t *testing.T) {
	srv, closePool := handler(t, config.Config{})
	srv.Start()
	defer srv.Close()
	listen(t, srv)
	if rc := healthcheck(); rc != 0 {
		t.Fatalf("healthy server: exit %d", rc)
	}
	closePool() // database gone => /healthz 503 => unhealthy
	if rc := healthcheck(); rc != 1 {
		t.Fatalf("database down: exit %d", rc)
	}
	srv.Close()
	if rc := healthcheck(); rc != 1 {
		t.Fatalf("server down: exit %d", rc)
	}
}

func TestHealthcheckTLS(t *testing.T) {
	srv, closePool := handler(t, config.Config{TLSCertFile: "x", TLSKeyFile: "y"})
	defer closePool()
	srv.StartTLS()
	defer srv.Close()
	t.Setenv("CONVERTFLOW_LISTEN", strings.TrimPrefix(srv.URL, "https://"))
	t.Setenv("CONVERTFLOW_TLS_CERT_FILE", "/some/cert")
	if rc := healthcheck(); rc != 0 {
		t.Fatalf("tls: exit %d", rc)
	}
}

func TestShutdownForcesCloseAfterTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-block // a handler that never finishes on its own
	}))
	srv.Start()
	defer close(block)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	if err := shutdownServer(srv.Config, 300*time.Millisecond); err != nil {
		t.Fatalf("forced close must not be an error: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestShutdownCleanReturnsNil(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Start()
	if err := shutdownServer(srv.Config, time.Second); err != nil {
		t.Fatal(err)
	}
}
