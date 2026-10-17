package e2e_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// inFlight reads the uploads-in-flight gauge from /metrics.
func (s *stack) inFlight() int {
	s.t.Helper()
	r := s.get("/metrics")
	for _, l := range strings.Split(string(r.Raw), "\n") {
		if v, ok := strings.CutPrefix(l, "convertflow_uploads_in_flight "); ok {
			var n int
			_, _ = fmt.Sscanf(v, "%d", &n)
			return n
		}
	}
	s.t.Fatalf("no uploads gauge in metrics: %s", r.Raw)
	return -1
}

func (s *stack) waitIdle() {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for s.inFlight() != 0 {
		if time.Now().After(deadline) {
			s.t.Fatal("upload slot was never released")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Review Focus 4 (too large): over-limit uploads get 413 whether the size is announced
// (Content-Length) or discovered while streaming (chunked), leave no partial file and no row;
// an upload of exactly the limit is accepted.
func TestTooLargeUploadRejected(t *testing.T) {
	const limit = 100 << 10
	s := start(t, opts{maxUpload: limit, maxUploads: 1})
	junk := func(n int) []byte { return make([]byte, n) }

	assertNothingStored := func(what string) {
		t.Helper()
		s.waitIdle()
		if n := s.names("in"); len(n) != 0 {
			t.Errorf("%s: partial input left behind: %v", what, n)
		}
		if n := s.jobCount(); n != 0 {
			t.Errorf("%s: %d job rows exist", what, n)
		}
	}

	// Announced length beyond limit+slack: refused before reading the body.
	if code, v := s.upload("audio-wav", "huge.wav", junk(300<<10)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("announced oversize: %d %v", code, v)
	}
	assertNothingStored("announced oversize")

	// Announced length within the multipart slack but file bytes above the limit: stopped while copying.
	if code, v := s.upload("audio-wav", "slightly.wav", junk(limit+1000)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("slightly oversize: %d %v", code, v)
	}
	assertNothingStored("slightly oversize")

	// Unknown length (chunked): the limit trips mid-stream.
	ct, body := multipartBody("audio-wav", "chunked.wav", junk(limit+100<<10))
	req, _ := http.NewRequest("POST", s.base+"/api/v1/jobs", io.NopCloser(body)) // NopCloser hides the length
	req.ContentLength = -1
	req.Header.Set("Content-Type", ct)
	if r := s.do(req); r.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked oversize: %d %s", r.Code, r.Raw)
	}
	assertNothingStored("chunked oversize")

	// Boundary: exactly the limit is accepted (and then fails as unreadable media, not as an upload error).
	id := s.mustUpload("audio-wav", "exact.wav", junk(limit))
	if v := s.wait(id); v["status"] != "failed" || v["error"] != msgUnreadable {
		t.Errorf("exact-limit job: %v", v)
	}
	s.waitDirEmpty("in")
	if !strings.Contains(s.logs.String(), "upload too large") {
		t.Error("no audit record for rejected oversize uploads")
	}
}

// rawUpload opens a connection, sends the request head and the first part of a multipart body,
// then breaks the connection in the given way.
func rawUpload(t *testing.T, addr string, style int) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	const b = "XBOUNDARYX"
	head := fmt.Sprintf("POST /api/v1/jobs HTTP/1.1\r\nHost: %s\r\nContent-Type: multipart/form-data; boundary=%s\r\nContent-Length: 10000000\r\n\r\n", addr, b)
	pre := fmt.Sprintf("--%s\r\nContent-Disposition: form-data; name=\"preset\"\r\n\r\naudio-wav\r\n--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.wav\"\r\nContent-Type: audio/wav\r\n\r\n", b, b)
	_, _ = io.WriteString(c, head)
	switch style % 4 {
	case 0: // headers only, then close
	case 1: // some of the file, then a clean close
		_, _ = io.WriteString(c, pre)
		_, _ = c.Write(make([]byte, 50<<10))
	case 2: // some of the file, then a reset
		_, _ = io.WriteString(c, pre)
		_, _ = c.Write(make([]byte, 200<<10))
		_ = c.(*net.TCPConn).SetLinger(0)
	case 3: // dies inside the part headers
		_, _ = io.WriteString(c, pre[:len(pre)-40])
	}
	time.Sleep(30 * time.Millisecond)
	_ = c.Close()
}

// Review Focus 4 (aborted upload): clients that vanish mid-body leave no partial file, no row and
// do not leak the upload slot. With a single slot, ten aborts in a row each must be followed by a
// working upload (503 would mean a leaked slot).
func TestAbortedUploadsReleaseSlot(t *testing.T) {
	s := start(t, opts{maxUploads: 1})
	addr := strings.TrimPrefix(s.base, "http://")
	good := clip(t, "audio")
	var ids []string
	for i := 0; i < 10; i++ {
		rawUpload(t, addr, i)
		s.waitIdle()
		if n := s.names("in"); len(n) != 0 {
			// Only a finished good upload still waiting for its worker may be there.
			for _, name := range n {
				if !contains(ids, name) {
					t.Fatalf("round %d: partial input %q left behind", i, name)
				}
			}
		}
		code, v := s.upload("audio-wav", "ok.wav", good)
		if code != http.StatusAccepted {
			t.Fatalf("round %d: upload after abort got %d %v (leaked slot?)", i, code, v)
		}
		ids = append(ids, v["id"].(string))
	}
	for _, id := range ids {
		if v := s.wait(id); v["status"] != "done" {
			t.Errorf("job %s: %v", id, v)
		}
	}
	if n := s.jobCount(); n != 10 {
		t.Errorf("rows = %d, want only the 10 good jobs", n)
	}
	s.waitDirEmpty("in")
	if n := s.names("out"); len(n) != 10 {
		t.Errorf("out/ = %v", n)
	}
	s.waitIdle()
}
