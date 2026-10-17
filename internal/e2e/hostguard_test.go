package e2e_test

import (
	"net/http"
	"strings"
	"testing"
)

// DNS rebinding: a browser tricked into resolving attacker.example to this server still sends
// "Host: attacker.example". Every route must refuse it with 421, without leaving a row or file,
// while the normal loopback Host keeps working.
func TestRebindingHostRefusedOnEveryRoute(t *testing.T) {
	s := start(t, opts{})
	// httptest listens on 127.0.0.1, so s.base carries an IP-literal Host: allowed by default.
	if r := s.get("/healthz"); r.Code != 200 {
		t.Fatalf("loopback Host: %d", r.Code)
	}
	for _, p := range []string{"/", "/api", "/api/openapi.json", "/api/v1/presets", "/api/v1/jobs", "/healthz", "/metrics",
		"/api/v1/jobs/00000000-0000-4000-8000-000000000000/events"} {
		req, _ := http.NewRequest("GET", s.base+p, nil)
		req.Host = "rebind.attacker.example:8080"
		r := s.do(req)
		if r.Code != 421 || !strings.HasPrefix(r.Head.Get("Content-Type"), "application/problem+json") || strings.Contains(string(r.Raw), "attacker") {
			t.Fatalf("GET %s with foreign Host: %d %s", p, r.Code, r.Raw)
		}
	}
	ct, body := multipartBody("audio-wav", "a.wav", []byte("RIFFxxxxxxxx"))
	req, _ := http.NewRequest("POST", s.base+"/api/v1/jobs", body)
	req.Header.Set("Content-Type", ct)
	req.Host = "rebind.attacker.example"
	if r := s.do(req); r.Code != 421 {
		t.Fatalf("POST with foreign Host: %d %s", r.Code, r.Raw)
	}
	if n := s.names("in"); len(n) != 0 || s.jobCount() != 0 {
		t.Fatalf("state changed by a rejected request: %v %d", n, s.jobCount())
	}
	if !strings.Contains(s.logs.String(), `"reason":"host_not_allowed"`) || strings.Contains(s.logs.String(), "attacker") {
		t.Fatalf("audit log: %s", s.logs.String())
	}
}
