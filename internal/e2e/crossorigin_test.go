package e2e_test

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// A browser page on another origin must not be able to upload or delete: the request is refused
// before any byte is stored and before any row exists. Without Origin (curl) the same upload works.
func TestCrossOriginUploadRefusedBeforeAnyWrite(t *testing.T) {
	s := start(t, opts{})
	wav := bytes.Repeat([]byte("RIFF"), 4096)

	post := func(hdr map[string]string) resp {
		ct, body := multipartBody("audio-wav", "a.wav", wav)
		req, _ := http.NewRequest("POST", s.base+"/api/v1/jobs", body)
		req.Header.Set("Content-Type", ct)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		return s.do(req)
	}
	for _, hdr := range []map[string]string{
		{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://evil.example"},
		{"Origin": "null"},
	} {
		r := post(hdr)
		if r.Code != 403 || !strings.HasPrefix(r.Head.Get("Content-Type"), "application/problem+json") {
			t.Fatalf("%v: got %d %s", hdr, r.Code, r.Raw)
		}
	}
	if n := s.names("in"); len(n) != 0 {
		t.Fatalf("input files written: %v", n)
	}
	if n := s.jobCount(); n != 0 {
		t.Fatalf("%d job rows", n)
	}
	if !strings.Contains(s.logs.String(), `"reason":"cross_origin"`) || strings.Contains(s.logs.String(), "evil.example") {
		t.Fatalf("audit log: %s", s.logs.String())
	}

	// curl-style: no Origin / Fetch metadata
	if r := post(nil); r.Code != http.StatusAccepted {
		t.Fatalf("no-origin upload: %d %s", r.Code, r.Raw)
	}
	// cross-site DELETE of the existing job is refused and the job survives
	var jid string
	for _, j := range decodeList(s.get("/api/v1/jobs").Raw) {
		jid = j
	}
	req, _ := http.NewRequest("DELETE", s.base+"/api/v1/jobs/"+jid, nil)
	req.Header.Set("Origin", "https://evil.example")
	if r := s.do(req); r.Code != 403 {
		t.Fatalf("cross-origin delete: %d", r.Code)
	}
	if s.jobCount() != 1 {
		t.Fatal("job was deleted by a cross-origin request")
	}
}

func decodeList(raw []byte) []string {
	var ids []string
	for _, m := range regexpID.FindAllSubmatch(raw, -1) {
		ids = append(ids, string(m[1]))
	}
	return ids
}

var regexpID = regexp.MustCompile(`"id":\s*"([0-9a-f-]{36})"`)
