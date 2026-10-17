package e2e_test

import (
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Review Focus 8: after a stack that served uploads, SSE streams, a cancellation, aborted uploads
// and a worker shutdown has been torn down, no goroutine running code of the API, worker, ffmpeg,
// jobs, janitor or audit packages may remain. (TestMain repeats the same check after the whole
// suite.) The raw goroutine counts are logged for information; pgx and the test's own helpers make
// them an unreliable assertion, so the stack-based filter is the check.
func TestNoGoroutineLeakAfterLifecycle(t *testing.T) {
	before := runtime.NumGoroutine()
	s := start(t, opts{slow: true, workers: 2})
	good := tone(t, 440, 2)
	a := s.mustUpload("audio-flac", "a.wav", good)
	if ev := s.readSSE(a); len(ev) == 0 || ev[len(ev)-1]["status"] != "done" {
		t.Fatalf("sse: %v", ev)
	}
	b := s.mustUpload("audio-flac", "b.wav", tone(t, 500, 30))
	s.waitStatus(b, 30*time.Second, "running")
	if r := s.del(b); r.Code != http.StatusAccepted {
		t.Fatalf("cancel: %d", r.Code)
	}
	s.waitStatus(b, 30*time.Second, "canceled")
	rawUpload(t, strings.TrimPrefix(s.base, "http://"), 2)
	s.waitIdle()
	// An SSE reader that disconnects early must not pin a handler either.
	c := s.mustUpload("audio-flac", "c.wav", tone(t, 700, 5))
	req, _ := http.NewRequest("GET", s.base+"/api/v1/jobs/"+c+"/events", nil)
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	s.del(c)

	mid := runtime.NumGoroutine()
	s.shutdown()
	leaks := settleGoroutines(10 * time.Second)
	t.Logf("goroutines: before=%d during=%d after=%d", before, mid, runtime.NumGoroutine())
	if len(leaks) > 0 {
		t.Fatalf("goroutines of the system under test survived shutdown:\n%s", strings.Join(leaks, "\n\n"))
	}
}
