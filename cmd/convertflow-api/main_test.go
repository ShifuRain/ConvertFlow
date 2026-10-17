package main

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

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
