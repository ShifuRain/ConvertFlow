package api_test

import (
	"bytes"
	"io"
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

func TestCrossOriginTable(t *testing.T) {
	f := newFixture(t, 1<<20)
	const host = "convertflow.internal:8080"
	cases := []struct {
		name   string
		method string
		hdr    map[string]string
		reject bool
	}{
		{"post same-origin", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, false},
		{"post none", "POST", map[string]string{"Sec-Fetch-Site": "none"}, false},
		{"post cross-site", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"post same-site", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"post unknown fetch-site", "POST", map[string]string{"Sec-Fetch-Site": "bogus"}, true},
		{"fetch-site same-origin beats foreign origin", "POST", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://evil.example"}, false},
		{"fetch-site cross-site beats matching origin", "POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://" + host}, true},
		{"origin matches", "POST", map[string]string{"Origin": "http://" + host}, false},
		{"origin matches other scheme", "POST", map[string]string{"Origin": "https://" + host}, false},
		{"origin matches case-insensitively", "POST", map[string]string{"Origin": "http://ConvertFlow.Internal:8080"}, false},
		{"origin other host", "POST", map[string]string{"Origin": "https://evil.example"}, true},
		{"origin other port", "POST", map[string]string{"Origin": "http://convertflow.internal:9999"}, true},
		{"origin null", "POST", map[string]string{"Origin": "null"}, true},
		{"origin garbage", "POST", map[string]string{"Origin": "::::"}, true},
		{"origin empty-host", "POST", map[string]string{"Origin": "http://"}, true},
		{"no headers (curl)", "POST", nil, false},
		{"x-forwarded-host not trusted", "POST", map[string]string{"Origin": "http://evil.example", "X-Forwarded-Host": "evil.example"}, true},
		{"delete cross-site", "DELETE", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"delete origin mismatch", "DELETE", map[string]string{"Origin": "http://evil.example"}, true},
		{"delete no headers", "DELETE", nil, false},
		{"put cross-site", "PUT", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"patch cross-site", "PATCH", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"get cross-site is not state changing", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		{"get foreign origin", "GET", map[string]string{"Origin": "https://evil.example"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := "/api/v1/jobs"
			if c.method == "DELETE" {
				path = "/api/v1/jobs/00000000-0000-4000-8000-000000000000"
			}
			r := httptest.NewRequest(c.method, path, strings.NewReader(""))
			r.Host = host
			for k, v := range c.hdr {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			f.h.ServeHTTP(w, r)
			if c.reject {
				if w.Code != 403 {
					t.Fatalf("want 403, got %d", w.Code)
				}
				p := problemOf(t, w)
				if strings.Contains(w.Body.String(), "evil") || p["status"] != float64(403) {
					t.Fatalf("response must be generic: %s", w.Body.String())
				}
			} else if w.Code == 403 {
				t.Fatalf("unexpected 403: %s", w.Body.String())
			}
		})
	}
}

func TestCrossOriginAuditIsGeneric(t *testing.T) {
	pool := dbtest.New(t)
	var logs bytes.Buffer
	h := api.NewHandler(api.Deps{Store: jobs.NewStore(pool), Cfg: config.Config{DataDir: t.TempDir(), Retention: time.Hour}, Audit: audit.New(&logs)})
	r := httptest.NewRequest("DELETE", "/api/v1/jobs/00000000-0000-4000-8000-000000000000", nil)
	r.Header.Set("Origin", "https://evil-attacker.example")
	r.RemoteAddr = "192.0.2.7:1"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	out := logs.String()
	if w.Code != 403 || !strings.Contains(out, `"event":"request_rejected"`) || !strings.Contains(out, `"reason":"cross_origin"`) {
		t.Fatalf("code %d audit %q", w.Code, out)
	}
	if strings.Contains(out, "evil") {
		t.Fatalf("attacker-controlled string in audit: %s", out)
	}
}

func TestCrossOriginUploadRejectedBeforeAnyWrite(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte{1}, 4096))
	r := httptest.NewRequest("POST", "/api/v1/jobs", body)
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("got %d", w.Code)
	}
	if n := inFiles(t, f); len(n) != 0 {
		t.Fatalf("files written: %v", n)
	}
	list, err := f.store.List(r.Context(), 10)
	if err != nil || len(list) != 0 {
		t.Fatalf("rows: %v %v", list, err)
	}
	// the body must not even have been read
	if rem, _ := io.ReadAll(body); len(rem) == 0 {
		t.Fatal("request body was consumed")
	}
}

func TestNoOriginUploadStillWorks(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte("data"))
	r := httptest.NewRequest("POST", "/api/v1/jobs", body)
	r.Header.Set("Content-Type", ct)
	r.Header.Set("User-Agent", "curl/8")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	// and a same-origin browser upload
	body, ct = multipartBody(t, []field{{"preset", "audio-wav"}}, "b.wav", []byte("data"))
	r = httptest.NewRequest("POST", "/api/v1/jobs", body)
	r.Host = "example.test"
	r.Header.Set("Content-Type", ct)
	r.Header.Set("Origin", "http://example.test")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w = httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("same-origin: got %d %s", w.Code, w.Body.String())
	}
}

func TestSecurityHeadersIncludeCOOP(t *testing.T) {
	f := newFixture(t, 1<<20)
	w := get(f, "/healthz")
	if w.Header().Get("Cross-Origin-Opener-Policy") != "same-origin" || w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("headers: %v", w.Header())
	}
}
