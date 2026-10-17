package api_test

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
)

func TestHostGuardTableAllRoutes(t *testing.T) {
	pool := dbtest.New(t)
	var logs bytes.Buffer
	newH := func(hosts []string) func(host, method, path string) *httptest.ResponseRecorder {
		h := api.NewHandler(api.Deps{Store: jobs.NewStore(pool), Cfg: config.Config{DataDir: t.TempDir(), AllowedHosts: hosts}, Audit: audit.New(&logs)})
		return func(host, method, path string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, nil)
			r.Host = host
			r.RemoteAddr = "192.0.2.7:1"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			return w
		}
	}
	def := newH(nil)
	paths := []string{"/", "/healthz", "/metrics", "/api", "/api/openapi.json", "/api/v1/presets", "/api/v1/jobs",
		"/api/v1/jobs/00000000-0000-4000-8000-000000000000", "/api/v1/jobs/00000000-0000-4000-8000-000000000000/events",
		"/api/v1/jobs/00000000-0000-4000-8000-000000000000/output", "/nonexistent"}
	cases := []struct {
		host  string
		allow bool
	}{
		{"localhost:8080", true}, {"127.0.0.1", true}, {"127.0.0.1:8080", true}, {"[::1]:8080", true},
		{"LOCALHOST", true}, {"localhost.", true},
		{"evil.example", false}, {"evil.example:8080", false}, {"", false}, {"EVIL.EXAMPLE", false},
		{"evil.example.", false}, {"127.0.0.1@evil.example", false}, {"evil.example@127.0.0.1", false},
		{":8080", false}, {"localhost evil.example", false}, {"127.0.0.1.evil.example", false},
	}
	for _, c := range cases {
		for _, p := range paths {
			w := def(c.host, "GET", p)
			if c.allow && w.Code == 421 {
				t.Errorf("Host %q GET %s: rejected", c.host, p)
			}
			if !c.allow {
				if w.Code != 421 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
					t.Errorf("Host %q GET %s: got %d %s", c.host, p, w.Code, w.Header().Get("Content-Type"))
				}
				if strings.Contains(w.Body.String(), "evil") {
					t.Errorf("rejection echoes the host: %s", w.Body)
				}
			}
		}
	}
	if w := def("evil.example", "POST", "/api/v1/jobs"); w.Code != 421 {
		t.Errorf("POST: %d", w.Code)
	}
	if w := def("evil.example", "DELETE", "/api/v1/jobs/00000000-0000-4000-8000-000000000000"); w.Code != 421 {
		t.Errorf("DELETE: %d", w.Code)
	}
	out := logs.String()
	if !strings.Contains(out, `"event":"request_rejected"`) || !strings.Contains(out, `"reason":"host_not_allowed"`) || strings.Contains(out, "evil") {
		t.Fatalf("audit: %.300s", out)
	}

	// configured names extend the defaults; "*" disables the check
	cfg := newH([]string{"convert.example.org"})
	if w := cfg("Convert.Example.org:8443", "GET", "/healthz"); w.Code == 421 {
		t.Error("configured host rejected")
	}
	if w := cfg("evil.example", "GET", "/healthz"); w.Code != 421 {
		t.Error("unlisted host accepted")
	}
	if w := cfg("127.0.0.1:8080", "GET", "/healthz"); w.Code == 421 {
		t.Error("loopback probe must stay allowed")
	}
	if w := newH([]string{"*"})("evil.example", "GET", "/healthz"); w.Code == 421 {
		t.Error("star must disable the check")
	}
}
