package e2e_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/ShifuRain/convertflow/internal/janitor"
	"github.com/ShifuRain/convertflow/internal/jobs"
)

// Review Focus 7 (round trip). The audio round trip checks container, duration and that the
// downloaded bytes decode to exactly the uploaded samples (FLAC is lossless).
func TestRoundTripAudio(t *testing.T) {
	s := start(t, opts{})
	in := tone(t, 440, 1)
	id := s.mustUpload("audio-flac", "tone.wav", in)
	done := s.wait(id)
	if done["status"] != "done" {
		t.Fatalf("job: %v", done)
	}
	r, out := s.download(done)
	if r.Code != 200 || r.Head.Get("Content-Type") != "audio/flac" {
		t.Fatalf("download: %d %v", r.Code, r.Head)
	}
	p := probe(t, out)
	if !strings.Contains(p.Format.FormatName, "flac") {
		t.Fatalf("format %q", p.Format.FormatName)
	}
	if d, _ := strconv.ParseFloat(p.Format.Duration, 64); d < 0.8 || d > 1.3 {
		t.Fatalf("unexpected duration %v", d)
	}
	inPath := filepath.Join(t.TempDir(), "in.wav")
	_ = os.WriteFile(inPath, in, 0o600)
	if pcmMD5(t, inPath) != pcmMD5(t, out) {
		t.Fatal("converted audio differs from the input samples")
	}
	if got := done["encoder_used"]; got != "flac" {
		t.Fatalf("encoder_used = %v", got)
	}
	s.waitDirEmpty("in")
}

// Review Focus 7 (round trip, video): real VP9 encode of a video+audio input.
func TestRoundTripVideo(t *testing.T) {
	s := start(t, opts{})
	id := s.mustUpload("video-vp9", "movie.mkv", clip(t, "av"))
	done := s.wait(id)
	if done["status"] != "done" {
		t.Fatalf("job: %v", done)
	}
	r, out := s.download(done)
	if r.Code != 200 || r.Head.Get("Content-Type") != "video/webm" {
		t.Fatalf("download: %d %v", r.Code, r.Head)
	}
	p := probe(t, out)
	if p.count("video") != 1 || p.count("audio") != 1 {
		t.Fatalf("streams: %+v", p.Streams)
	}
}

// Review Focus 7 (existing behaviour): DELETE of a finished job removes its files.
func TestDeleteRemovesFiles(t *testing.T) {
	s := start(t, opts{})
	id := s.mustUpload("audio-wav", "a.wav", clip(t, "audio"))
	if st := s.wait(id)["status"]; st != "done" {
		t.Fatalf("status %v", st)
	}
	if len(s.names("out")) != 1 {
		t.Fatalf("expected one output, got %v", s.names("out"))
	}
	if r := s.del(id); r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", r.Code)
	}
	if n := s.names("out"); len(n) != 0 {
		t.Fatalf("output not removed: %v", n)
	}
	if r := s.get("/api/v1/jobs/" + id); r.Code != http.StatusNotFound {
		t.Fatalf("job still present: %d", r.Code)
	}
}

// Review Focus 7 (cancel): DELETE while running answers 202, the worker kills FFmpeg and the job
// ends canceled with no leftover files; a second DELETE then removes the row.
func TestCancelWhileRunning(t *testing.T) {
	s := start(t, opts{slow: true})
	id := s.mustUpload("audio-flac", "long.wav", tone(t, 330, 30))
	s.waitStatus(id, 30*time.Second, "running")
	r := s.del(id)
	if r.Code != http.StatusAccepted || r.Body["status"] != "canceling" {
		t.Fatalf("delete while running: %d %v", r.Code, r.Body)
	}
	got := s.waitStatus(id, 30*time.Second)
	if got["status"] != "canceled" {
		t.Fatalf("want canceled, got %v", got)
	}
	s.waitDirEmpty("out")
	s.waitDirEmpty("in")
	if r := s.del(id); r.Code != http.StatusNoContent {
		t.Fatalf("second delete: %d", r.Code)
	}
	if r := s.get("/api/v1/jobs/" + id); r.Code != http.StatusNotFound {
		t.Fatalf("row still present: %d", r.Code)
	}
	logs := s.logs.String()
	for _, ev := range []string{"job_cancel_requested", "job_canceled"} {
		if !strings.Contains(logs, ev) {
			t.Errorf("audit log lacks %s", ev)
		}
	}
}

// Review Focus 7 (crash requeue): the first worker dies mid-run; RequeueStale hands the job to a
// second worker. Attempt counter, per-attempt output names and file cleanup are verified, including
// a stray partial output of the dead attempt (as a hard kill would leave) that the janitor removes.
func TestWorkerCrashRequeue(t *testing.T) {
	s := start(t, opts{noWorkers: true})
	a := s.addWorker("crash-a", workerOpts{slow: true})
	s.waitWorkers(1)
	in := tone(t, 523, 30)
	id := s.mustUpload("audio-flac", "crash.wav", in)
	run := s.waitStatus(id, 30*time.Second, "running")
	if run["attempts"].(float64) != 1 {
		t.Fatalf("attempts = %v", run["attempts"])
	}
	// The output of attempt 1 appears while FFmpeg runs.
	first := id + ".1.flac"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if contains(s.names("out"), first) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("attempt 1 output %s never appeared: %v", first, s.names("out"))
		}
		time.Sleep(25 * time.Millisecond)
	}

	a.stop() // the worker process goes away; the job stays running in the database
	if n := s.names("out"); len(n) != 0 {
		t.Fatalf("dead worker left files behind: %v", n)
	}
	// A hard kill would leave the partial output: simulate it and make it look old.
	stray := filepath.Join(s.dir, "out", first)
	if err := os.WriteFile(stray, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(stray, old, old)

	time.Sleep(1200 * time.Millisecond) // let the heartbeat go stale
	requeued, failed, err := s.store.RequeueStale(context.Background(), time.Second, 3)
	if err != nil || requeued != 1 || failed != 0 {
		t.Fatalf("RequeueStale = %d %d %v", requeued, failed, err)
	}
	if st := s.get("/api/v1/jobs/" + id).Body["status"]; st != "queued" {
		t.Fatalf("after requeue status = %v", st)
	}
	s.addWorker("crash-b", workerOpts{})
	done := s.waitStatus(id, 60*time.Second)
	if done["status"] != "done" || done["attempts"].(float64) != 2 {
		t.Fatalf("second worker result: %v", done)
	}
	second := id + ".2.flac"
	names := s.names("out")
	if !contains(names, second) || !contains(names, first) {
		t.Fatalf("want %s and stray %s in out/, got %v", second, first, names)
	}
	r, out := s.download(done)
	if r.Code != 200 {
		t.Fatalf("download %d", r.Code)
	}
	inPath := filepath.Join(t.TempDir(), "in.wav")
	_ = os.WriteFile(inPath, in, 0o600)
	if pcmMD5(t, out) != pcmMD5(t, inPath) {
		t.Fatal("requeued job produced different audio")
	}

	j := &janitor.Janitor{Store: s.store, Audit: audit.New(s.logs), Blob: blob.OpenLocalFS(s.dir), Retention: 24 * time.Hour,
		StaleAfter: time.Hour, MaxAttempts: 3, OrphanGrace: time.Hour}
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := s.names("out"); len(n) != 1 || n[0] != second {
		t.Fatalf("after janitor out/ = %v, want only %s", n, second)
	}
	s.waitDirEmpty("in")
}

func contains(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// Review Focus 7 (retention): the janitor deletes expired jobs together with their files and keeps
// recent ones.
func TestRetentionJanitor(t *testing.T) {
	const retention = 5 * time.Second
	s := start(t, opts{})
	old := s.mustUpload("audio-wav", "old.wav", clip(t, "audio"))
	oldFailed := s.mustUpload("audio-wav", "bad.wav", []byte("not media at all"))
	if s.wait(old)["status"] != "done" || s.wait(oldFailed)["status"] != "failed" {
		t.Fatal("setup jobs did not finish as expected")
	}
	// Poll until both are past the retention window (no fixed sleeps).
	deadline := time.Now().Add(30 * time.Second)
	for {
		exp, err := s.store.Expired(context.Background(), retention)
		if err != nil {
			t.Fatal(err)
		}
		if len(exp) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("jobs never became expired")
		}
		time.Sleep(100 * time.Millisecond)
	}
	fresh := s.mustUpload("audio-wav", "fresh.wav", clip(t, "audio"))
	if s.wait(fresh)["status"] != "done" {
		t.Fatal("fresh job failed")
	}
	if len(s.names("out")) != 2 {
		t.Fatalf("want 2 outputs before sweep, got %v", s.names("out"))
	}
	j := &janitor.Janitor{Store: s.store, Audit: audit.New(s.logs), Blob: blob.OpenLocalFS(s.dir), Retention: retention,
		StaleAfter: time.Hour, MaxAttempts: 3}
	if err := j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{old, oldFailed} {
		if r := s.get("/api/v1/jobs/" + id); r.Code != http.StatusNotFound {
			t.Errorf("expired job %s still there: %d", id, r.Code)
		}
	}
	if r := s.get("/api/v1/jobs/" + fresh); r.Code != http.StatusOK {
		t.Errorf("fresh job removed: %d", r.Code)
	}
	if n := s.names("out"); len(n) != 1 || !strings.HasPrefix(n[0], fresh+".") {
		t.Errorf("out/ after sweep = %v, want only the fresh output", n)
	}
	if !strings.Contains(s.logs.String(), "job_expired") {
		t.Error("no job_expired audit record")
	}
}

// Review Focus 7 (SSE): the event stream of a real running job ends with the terminal state, shows
// monotonic progress, and a stream opened on a finished job delivers it and closes.
func TestSSEUntilDone(t *testing.T) {
	s := start(t, opts{slow: true})
	id := s.mustUpload("audio-flac", "sse.wav", tone(t, 600, 4))

	events := s.readSSE(id)
	if len(events) < 2 {
		t.Fatalf("too few events: %v", events)
	}
	last := events[len(events)-1]
	if last["status"] != "done" || last["progress"].(float64) != 100 || last["output_url"] == nil {
		t.Fatalf("last event: %v", last)
	}
	sawRunning, prev := false, -1.0
	for _, e := range events {
		if e["status"] == "running" {
			sawRunning = true
		}
		p := e["progress"].(float64)
		if p < prev {
			t.Errorf("progress went backwards: %v after %v", p, prev)
		}
		prev = p
	}
	if !sawRunning {
		t.Errorf("no running event in %v", events)
	}
	again := s.readSSE(id)
	if len(again) != 1 || again[0]["status"] != "done" {
		t.Fatalf("stream on a finished job: %v", again)
	}
}

// readSSE reads the event stream of a job until the server closes it.
func (s *stack) readSSE(id string) []map[string]any {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.base+"/api/v1/jobs/"+id+"/events", nil)
	r, err := s.client.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/event-stream") {
		s.t.Fatalf("sse: %d %v", r.StatusCode, r.Header)
	}
	var out []map[string]any
	sc := bufio.NewScanner(r.Body)
	for sc.Scan() {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var v map[string]any
			if err := json.Unmarshal([]byte(d), &v); err != nil {
				s.t.Fatalf("bad event %q", d)
			}
			out = append(out, v)
		}
	}
	return out
}

// Review Focus 7 (fallback chain): a worker whose FFmpeg has no SVT-AV1 encoder falls back to VP9
// for the AV1 preset (simulated by hiding libsvtav1 from `ffmpeg -encoders`), a full worker uses SVT-AV1.
func TestPresetFallbackChain(t *testing.T) {
	for _, tc := range []struct{ name, hide, want string }{
		{"with-svtav1", "", "libsvtav1"},
		{"without-svtav1", "libsvtav1", "libvpx-vp9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := start(t, opts{hide: tc.hide})
			var ids []map[string]any
			r := s.get("/api/v1/presets")
			_ = json.Unmarshal(r.Raw, &ids)
			found := false
			for _, p := range ids {
				found = found || p["id"] == "video-av1"
			}
			if !found {
				t.Fatalf("video-av1 not offered: %s", r.Raw)
			}
			id := s.mustUpload("video-av1", "v.mkv", clip(t, "video"))
			done := s.wait(id)
			if done["status"] != "done" || done["encoder_used"] != tc.want {
				t.Fatalf("want done with %s, got %v", tc.want, done)
			}
			_, out := s.download(done)
			if probe(t, out).count("video") != 1 {
				t.Fatal("output has no video")
			}
		})
	}
}

// Review Focus 7 (presets reflect live workers): the list is empty without workers, shows only what
// the live workers can encode, drops workers that stopped heartbeating, and uploads for presets
// nobody can run are refused with 422.
func TestPresetsReflectLiveWorkers(t *testing.T) {
	s := start(t, opts{noWorkers: true})
	ids := func() map[string]string {
		var l []map[string]string
		r := s.get("/api/v1/presets")
		if r.Code != 200 || json.Unmarshal(r.Raw, &l) != nil {
			t.Fatalf("presets: %d %s", r.Code, r.Raw)
		}
		m := map[string]string{}
		for _, p := range l {
			m[p["id"]] = p["kind"]
		}
		return m
	}
	if m := ids(); len(m) != 0 {
		t.Fatalf("presets without workers: %v", m)
	}
	if code, v := s.upload("audio-wav", "a.wav", clip(t, "audio")); code != 422 {
		t.Fatalf("upload without workers = %d %v", code, v)
	}

	audioOnly := s.addWorker("audio-only", workerOpts{hide: "libvpx-vp9|libvpx|libsvtav1|libx264|libx265"})
	s.waitWorkers(1)
	m := ids()
	if len(m) == 0 {
		t.Fatal("no presets with an audio worker")
	}
	for id, kind := range m {
		if kind != "audio" {
			t.Errorf("audio-only worker advertises %s (%s)", id, kind)
		}
	}
	if code, v := s.upload("video-vp9", "v.mkv", clip(t, "video")); code != 422 {
		t.Fatalf("video upload with audio-only worker = %d %v", code, v)
	}
	if st := s.wait(s.mustUpload("audio-wav", "a.wav", clip(t, "audio")))["status"]; st != "done" {
		t.Fatalf("audio job: %v", st)
	}

	// The worker dies and stops being seen: its capabilities disappear from the API.
	audioOnly.stop()
	if _, err := s.pool.Exec(context.Background(), `UPDATE workers SET seen_at = now() - interval '10 minutes'`); err != nil {
		t.Fatal(err)
	}
	if m := ids(); len(m) != 0 {
		t.Fatalf("stale worker still advertised: %v", m)
	}

	s.addWorker("full", workerOpts{})
	s.waitWorkers(1)
	if m := ids(); m["video-vp9"] != "video" || m["audio-flac"] != "audio" {
		t.Fatalf("full worker presets: %v", m)
	}
}

// Review Focus 7 (heterogeneous workers): Claim only hands a worker jobs for presets it can encode,
// so an audio-only worker next to a full one never fails a video job.
func TestHeterogeneousWorkersMisclaim(t *testing.T) {
	s := start(t, opts{noWorkers: true})
	s.addWorker("audio-only", workerOpts{hide: "libvpx-vp9|libvpx|libsvtav1|libx264|libx265"})
	s.addWorker("full", workerOpts{})
	s.waitWorkers(2)
	for i := 0; i < 12; i++ {
		id := s.mustUpload("video-vp9", "v.mkv", clip(t, "video"))
		if done := s.wait(id); done["status"] != "done" {
			t.Fatalf("job %d: %v", i, done)
		}
	}
}

// Review Focus 7 (concurrency): eight parallel uploads, two workers; every job finishes, both
// workers take part, and every output is bit-exact the audio of its own input (no cross-talk).
func TestParallelUploadsTwoWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("slow: runs eight real-time conversions")
	}
	s := start(t, opts{workers: 2, slow: true})
	type item struct {
		in   []byte
		id   string
		freq int
		dur  float64
	}
	items := make([]*item, 8)
	for i := range items {
		freq, dur := 300+i*110, 1.0+float64(i)*0.15
		items[i] = &item{in: tone(t, freq, dur), freq: freq, dur: dur}
	}
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, v := s.upload("audio-flac", "p"+strconv.Itoa(i)+".wav", it.in)
			if code != 202 {
				t.Errorf("upload %d: %d %v", i, code, v)
				return
			}
			it.id = v["id"].(string)
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	for i, it := range items {
		done := s.wait(it.id)
		if done["status"] != "done" {
			t.Fatalf("job %d: %v", i, done)
		}
		_, out := s.download(done)
		inPath := filepath.Join(t.TempDir(), "in.wav")
		_ = os.WriteFile(inPath, it.in, 0o600)
		if pcmMD5(t, inPath) != pcmMD5(t, out) {
			t.Errorf("job %d: output audio differs from its own input", i)
		}
		d, _ := strconv.ParseFloat(probe(t, out).Format.Duration, 64)
		if d < it.dur-0.05 || d > it.dur+0.1 {
			t.Errorf("job %d: duration %v, want ~%v", i, d, it.dur)
		}
	}
	used := map[string]bool{}
	for _, line := range strings.Split(s.logs.String(), "\n") {
		if strings.Contains(line, `"event":"job_finished"`) {
			var rec map[string]any
			_ = json.Unmarshal([]byte(line), &rec)
			used[rec["worker"].(string)] = true
		}
	}
	if len(used) != 2 {
		t.Errorf("expected both workers to finish jobs, got %v", used)
	}
	s.waitDirEmpty("in")
	if n := s.names("out"); len(n) != 8 {
		t.Errorf("want 8 outputs, got %v", n)
	}
}

// A queued job whose preset no live worker supports stays queued (neither failed nor claimed) and
// still counts as active in the API's queue cap.
func TestUnsupportedPresetStaysQueued(t *testing.T) {
	s := start(t, opts{noWorkers: true})
	s.addWorker("audio-only", workerOpts{hide: "libvpx-vp9|libvpx|libsvtav1|libx264|libx265"})
	s.waitWorkers(1)
	// Enqueue directly: the API (rightly) refuses presets nobody offers.
	id := "11111111-2222-4333-8444-555555555555"
	in := filepath.Join(s.dir, "in", id)
	_ = os.WriteFile(in, clip(t, "video"), 0o600)
	if err := s.store.Create(context.Background(), &jobs.Job{ID: id, Filename: "v.mkv", InputPath: in, Options: jobs.Options{Preset: "video-vp9"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	jan := &janitor.Janitor{Store: s.store, Audit: audit.New(s.logs), Blob: blob.OpenLocalFS(s.dir), Retention: time.Hour,
		StaleAfter: time.Hour, MaxAttempts: 3, MaxQueueAge: time.Hour}
	if err := jan.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := s.get("/api/v1/jobs/" + id).Body; v["status"] != "queued" || v["attempts"].(float64) != 0 {
		t.Fatalf("job should still be queued: %v", v)
	}
	if n, err := s.store.CountActive(context.Background()); err != nil || n != 1 {
		t.Fatalf("CountActive = %d %v", n, err)
	}
	s.addWorker("full", workerOpts{})
	s.waitWorkers(2)
	if v := s.waitStatus(id, 60*time.Second); v["status"] != "done" {
		t.Fatalf("after a capable worker appears: %v", v)
	}
}

// A job stranded in the queue (no live worker offers its preset) is failed by the janitor after the
// maximum queue age, its input is removed, and the API's queue cap is freed.
func TestStrandedQueuedJobExpires(t *testing.T) {
	s := start(t, opts{noWorkers: true, maxQueued: 1})
	s.addWorker("audio-only", workerOpts{hide: "libvpx-vp9|libvpx|libsvtav1|libx264|libx265"})
	s.waitWorkers(1)
	id := "22222222-3333-4444-8555-666666666666"
	in := filepath.Join(s.dir, "in", id)
	_ = os.WriteFile(in, clip(t, "video"), 0o600)
	if err := s.store.Create(context.Background(), &jobs.Job{ID: id, Filename: "v.mkv", InputPath: in, Options: jobs.Options{Preset: "video-vp9"}}); err != nil {
		t.Fatal(err)
	}
	if code, v := s.upload("audio-wav", "a.wav", clip(t, "audio")); code != http.StatusServiceUnavailable {
		t.Fatalf("queue cap should refuse: %d %v", code, v)
	}
	time.Sleep(700 * time.Millisecond)
	jan := &janitor.Janitor{Store: s.store, Audit: audit.New(s.logs), Blob: blob.OpenLocalFS(s.dir), Retention: time.Hour,
		StaleAfter: time.Hour, MaxAttempts: 3, MaxQueueAge: 500 * time.Millisecond}
	if err := jan.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := s.get("/api/v1/jobs/" + id).Body
	if v["status"] != "failed" || v["error"] != "no worker available for this preset" {
		t.Fatalf("stranded job: %v", v)
	}
	s.waitDirEmpty("in")
	if st := s.wait(s.mustUpload("audio-wav", "a.wav", clip(t, "audio")))["status"]; st != "done" {
		t.Fatalf("upload after the cap freed up: %v", st)
	}
	s.assertNoLeak("logs", s.logs.String())
}
