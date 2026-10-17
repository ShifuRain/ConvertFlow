package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
)

func get(f fixture, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func del(f fixture, path string) *httptest.ResponseRecorder {
	return do(f, "DELETE", path)
}

func do(f fixture, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func seedDone(t *testing.T, f fixture, filename, preset, content string) string {
	t.Helper()
	id := uuid.NewString()
	in := filepath.Join(f.dir, "in", id)
	out := filepath.Join(f.dir, "out", id+".1.wav") // worker naming: <id>.<attempt>.<ext>
	_ = os.WriteFile(out, []byte(content), 0o600)
	ctx := context.Background()
	if err := f.store.Create(ctx, &jobs.Job{ID: id, Filename: filename, InputPath: in, Options: jobs.Options{Preset: preset}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Claim(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetRunning(ctx, id, "pcm_s16le"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Complete(ctx, id, out); err != nil {
		t.Fatal(err)
	}
	return id
}

func outPathOf(t *testing.T, f fixture, id string) string {
	t.Helper()
	j, err := f.store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j.OutputPath
}

func TestGetAndListJobs(t *testing.T) {
	f := newFixture(t, 1<<20)
	id := seedDone(t, f, "a.mp3", "audio-wav", "x")
	w := get(f, "/api/v1/jobs/"+id)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v["status"] != "done" || v["output_url"] != "/api/v1/jobs/"+id+"/output" {
		t.Fatalf("bad view %v", v)
	}
	if strings.Contains(w.Body.String(), f.dir) {
		t.Fatal("server paths must not be exposed")
	}
	var list []map[string]any
	_ = json.Unmarshal(get(f, "/api/v1/jobs").Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("want 1 job, got %v", list)
	}
}

func TestEmptyListIsJSONArray(t *testing.T) {
	f := newFixture(t, 1<<20)
	if b := strings.TrimSpace(get(f, "/api/v1/jobs").Body.String()); b != "[]" {
		t.Fatalf("want [] got %s", b)
	}
}

func TestGetJobNotFoundAndBadID(t *testing.T) {
	f := newFixture(t, 1<<20)
	for _, p := range []string{"/api/v1/jobs/" + uuid.NewString(), "/api/v1/jobs/not-a-uuid"} {
		w := get(f, p)
		if w.Code != 404 {
			t.Errorf("%s: want 404 got %d", p, w.Code)
		}
		problemOf(t, w)
	}
}

// A malformed id must be a 404 on every id route (never a Postgres cast error => 500), including
// non-canonical UUID spellings that uuid.Parse alone would accept.
func TestMalformedIDIs404EverywhereIncludingNonCanonical(t *testing.T) {
	f := newFixture(t, 1<<20)
	ids := []string{"not-a-uuid", "..%2f..%2fetc", "3F2B8C1E-5D4A-4B7E-9A10-0C1D2E3F4A5B",
		"urn:uuid:3f2b8c1e-5d4a-4b7e-9a10-0c1d2e3f4a5b", "3f2b8c1e5d4a4b7e9a100c1d2e3f4a5b", "%00"}
	for _, id := range ids {
		for _, c := range []struct{ m, suffix string }{{"GET", ""}, {"GET", "/events"}, {"GET", "/output"}, {"DELETE", ""}} {
			w := do(f, c.m, "/api/v1/jobs/"+id+c.suffix)
			if w.Code != 404 {
				t.Errorf("%s %s%s: want 404 got %d", c.m, id, c.suffix, w.Code)
				continue
			}
			problemOf(t, w)
		}
	}
}

func TestOutputDisposition(t *testing.T) {
	f := newFixture(t, 1<<20)
	id := seedDone(t, f, "My Clip – Ünï.mp4", "audio-wav", "RIFFDATA")
	w := get(f, "/api/v1/jobs/"+id+"/output")
	if w.Code != 200 || w.Body.String() != "RIFFDATA" {
		t.Fatalf("status %d body %q", w.Code, w.Body)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") || strings.ContainsAny(cd, "\n\r") || !strings.Contains(cd, ".wav") ||
		!strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("bad disposition %q", cd)
	}
	// Content-Type comes from the preset's container, never from the user's filename (.mp4 here).
	if ct := w.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("content-type %q", ct)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff missing on download")
	}
	w = get(f, "/api/v1/jobs/"+id+"/output", "Range", "bytes=0-3")
	if w.Code != 206 || w.Body.String() != "RIFF" {
		t.Fatalf("range: %d %q", w.Code, w.Body)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("range response must keep nosniff and attachment")
	}
}

func TestOutputUsesRecordedOutputPath(t *testing.T) {
	f := newFixture(t, 1<<20)
	id := seedDone(t, f, "a.mp3", "audio-wav", "attempt-one")
	// A file named after the job id (no attempt suffix) must never be served in place of OutputPath.
	_ = os.WriteFile(filepath.Join(f.dir, "out", id+".wav"), []byte("WRONG"), 0o600)
	if w := get(f, "/api/v1/jobs/"+id+"/output"); w.Body.String() != "attempt-one" {
		t.Fatalf("served %q", w.Body)
	}
}

func TestOutputRemovedIs410WithoutPathLeak(t *testing.T) {
	f := newFixture(t, 1<<20)
	id := seedDone(t, f, "a.mp3", "audio-wav", "x")
	_ = os.Remove(outPathOf(t, f, id))
	w := get(f, "/api/v1/jobs/"+id+"/output")
	if w.Code != 410 {
		t.Fatalf("want 410 got %d", w.Code)
	}
	problemOf(t, w)
	if strings.Contains(w.Body.String(), f.dir) {
		t.Fatal("path leaked")
	}
	// Output path that is a directory is not a servable file either.
	id2 := seedDone(t, f, "b.mp3", "audio-wav", "x")
	p := outPathOf(t, f, id2)
	_ = os.Remove(p)
	_ = os.Mkdir(p, 0o700)
	if w := get(f, "/api/v1/jobs/"+id2+"/output"); w.Code != 410 {
		t.Fatalf("dir output: want 410 got %d", w.Code)
	}
}

func TestOutputNotReady(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte("x"))
	var v map[string]any
	_ = json.Unmarshal(post(f, body, ct).Body.Bytes(), &v)
	w := get(f, "/api/v1/jobs/"+v["id"].(string)+"/output")
	if w.Code != 409 {
		t.Fatalf("want 409 got %d", w.Code)
	}
	problemOf(t, w)
	// failed jobs are not downloadable either
	ctx := context.Background()
	id := uuid.NewString()
	_ = f.store.Create(ctx, &jobs.Job{ID: id, Filename: "f", InputPath: "/x", Options: jobs.Options{Preset: "audio-wav"}})
	_, _ = f.store.Claim(ctx, "w1")
	_ = f.store.Fail(ctx, id, "boom")
	if w := get(f, "/api/v1/jobs/"+id+"/output"); w.Code != 409 {
		t.Fatalf("failed job: want 409 got %d", w.Code)
	}
}

func TestDeleteSemantics(t *testing.T) {
	f := newFixture(t, 1<<20)
	ctx := context.Background()
	// done job: row and files removed
	id := seedDone(t, f, "a.mp3", "audio-wav", "x")
	out := outPathOf(t, f, id)
	in := filepath.Join(f.dir, "in", id)
	_ = os.WriteFile(in, []byte("in"), 0o600)
	if w := del(f, "/api/v1/jobs/"+id); w.Code != 204 {
		t.Fatalf("want 204 got %d", w.Code)
	}
	for _, p := range []string{out, in} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s should be deleted", p)
		}
	}
	if _, err := f.store.Get(ctx, id); err != jobs.ErrNotFound {
		t.Fatal("row should be deleted")
	}
	// running job: cancel requested, 202
	rid := uuid.NewString()
	_ = f.store.Create(ctx, &jobs.Job{ID: rid, Filename: "r", InputPath: "/x", Options: jobs.Options{Preset: "audio-wav"}})
	_, _ = f.store.Claim(ctx, "w1")
	if w := del(f, "/api/v1/jobs/"+rid); w.Code != 202 {
		t.Fatalf("want 202 got %d", w.Code)
	}
	if cancel, _ := f.store.Heartbeat(ctx, rid); !cancel {
		t.Fatal("cancel flag should be set")
	}
	w := del(f, "/api/v1/jobs/"+uuid.NewString())
	if w.Code != 404 {
		t.Fatalf("want 404 got %d", w.Code)
	}
	problemOf(t, w)
}

func TestDeleteToleratesMissingFilesAndNeverTouchesOutsideDataDir(t *testing.T) {
	f := newFixture(t, 1<<20)
	ctx := context.Background()
	// files already gone (retention race): still 204
	id := seedDone(t, f, "a.mp3", "audio-wav", "x")
	_ = os.Remove(outPathOf(t, f, id))
	if w := del(f, "/api/v1/jobs/"+id); w.Code != 204 {
		t.Fatalf("missing files: want 204 got %d", w.Code)
	}
	// a recorded path outside the data dir is never removed
	outside := filepath.Join(t.TempDir(), "precious")
	_ = os.WriteFile(outside, []byte("keep"), 0o600)
	oid := uuid.NewString()
	_ = f.store.Create(ctx, &jobs.Job{ID: oid, Filename: "o", InputPath: outside, Options: jobs.Options{Preset: "audio-wav"}})
	if w := del(f, "/api/v1/jobs/"+oid); w.Code != 204 {
		t.Fatalf("want 204 got %d", w.Code)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("file outside the data dir must not be deleted")
	}
}

func TestDeleteQueuedJobRemovesInput(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte("x"))
	var v map[string]any
	_ = json.Unmarshal(post(f, body, ct).Body.Bytes(), &v)
	if w := del(f, "/api/v1/jobs/"+v["id"].(string)); w.Code != 204 {
		t.Fatalf("got %d", w.Code)
	}
	if n := inFiles(t, f); len(n) != 0 {
		t.Fatalf("input not removed: %v", n)
	}
}

func TestPresetsListsOnlyRunnable(t *testing.T) {
	f := newFixture(t, 1<<20)
	var ps []map[string]any
	_ = json.Unmarshal(get(f, "/api/v1/presets").Body.Bytes(), &ps)
	ids := map[string]bool{}
	for _, p := range ps {
		ids[p["id"].(string)] = true
	}
	if !ids["audio-wav"] || !ids["audio-flac"] || !ids["audio-opus"] || ids["video-av1"] {
		t.Fatalf("unexpected presets %v", ids)
	}
}

func TestPresetsFilteredByFlavor(t *testing.T) {
	for flavor, want := range map[string]bool{"lgpl": false, "full": true, "": false} {
		f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) { c.Flavor = flavor })
		// a (misconfigured/stale) worker advertises patent-encumbered encoders
		_ = f.store.UpsertWorker(context.Background(), "w2", []string{"libx264", "aac", "pcm_s16le"})
		var ps []map[string]any
		_ = json.Unmarshal(get(f.fixture, "/api/v1/presets").Body.Bytes(), &ps)
		got := false
		for _, p := range ps {
			if p["id"] == "video-h264" {
				got = true
			}
		}
		if got != want {
			t.Errorf("flavor %q: video-h264 listed=%v want %v", flavor, got, want)
		}
	}
}

func TestPresetsEmptyIsArray(t *testing.T) {
	f := newFixture(t, 1<<20)
	_ = f.store.UpsertWorker(context.Background(), "w1", nil)
	if b := strings.TrimSpace(get(f, "/api/v1/presets").Body.String()); b != "[]" {
		t.Fatalf("want [] got %s", b)
	}
}

func TestHealthzAndMetrics(t *testing.T) {
	f := newFixture(t, 1<<20)
	hz := get(f, "/healthz")
	if hz.Code != 200 || hz.Header().Get("X-Content-Type-Options") != "nosniff" || hz.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("healthz %d %v", hz.Code, hz.Header())
	}
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "secret-name.wav", []byte("x"))
	post(f, body, ct)
	mw := get(f, "/metrics")
	m := mw.Body.String()
	if !strings.Contains(m, `convertflow_jobs{status="queued"} 1`) || !strings.Contains(m, "convertflow_workers_live 1") {
		t.Fatalf("bad metrics:\n%s", m)
	}
	if !strings.Contains(m, `convertflow_jobs{status="done"} 0`) {
		t.Fatal("all statuses must be present, zero-filled")
	}
	if !strings.HasPrefix(mw.Header().Get("Content-Type"), "text/plain; version=0.0.4") ||
		mw.Header().Get("X-Content-Type-Options") != "nosniff" || mw.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("metrics headers %v", mw.Header())
	}
	// Labels are a fixed vocabulary: nothing user-controlled may show up.
	if strings.Contains(m, "secret-name") || strings.Contains(m, f.dir) {
		t.Fatal("metrics must not contain filenames or paths")
	}
	re := regexp.MustCompile(`\{([a-z_]+)="`)
	for _, sm := range re.FindAllStringSubmatch(m, -1) {
		if sm[1] != "status" {
			t.Errorf("unexpected label %q", sm[1])
		}
	}
}

func TestHealthzUnhealthyWhenDBDown(t *testing.T) {
	pool := dbtest.New(t)
	store := jobs.NewStore(pool)
	h := api.NewHandler(api.Deps{Store: store, Cfg: config.Config{DataDir: t.TempDir()}, Audit: audit.New(io.Discard)})
	pool.Close()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 503 {
		t.Fatalf("want 503 got %d", w.Code)
	}
	problemOf(t, w)
}

func TestOpenAPIServed(t *testing.T) {
	f := newFixture(t, 1<<20)
	w := get(f, "/api/openapi.json")
	var doc map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil || doc["openapi"] != "3.1.0" {
		t.Fatalf("bad openapi: %d", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("content-type %q", w.Header().Get("Content-Type"))
	}
}

type respObj struct {
	Ref     string         `json:"$ref"`
	Content map[string]any `json:"content"`
}

// Every registered route must be documented, and every documented operation must be registered.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	f := newFixture(t, 1<<20)
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]respObj `json:"responses"`
		} `json:"paths"`
		Components struct {
			Responses map[string]respObj `json:"responses"`
		} `json:"components"`
	}
	if err := json.Unmarshal(get(f, "/api/openapi.json").Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	for p, ops := range doc.Paths {
		for m, op := range ops {
			documented[strings.ToUpper(m)+" "+p] = true
			for code, resp := range op.Responses {
				if resp.Ref != "" {
					resp = doc.Components.Responses[strings.TrimPrefix(resp.Ref, "#/components/responses/")]
				}
				n, _ := strconv.Atoi(code)
				if n >= 400 {
					if _, ok := resp.Content["application/problem+json"]; !ok {
						t.Errorf("%s %s: %s must be documented as application/problem+json", m, p, code)
					}
				}
			}
		}
	}
	registered := map[string]bool{}
	for _, r := range api.RoutePatterns() {
		registered[r] = true
	}
	for r := range registered {
		if !documented[r] {
			t.Errorf("route %q is not in the OpenAPI document", r)
		}
	}
	for d := range documented {
		if !registered[d] {
			t.Errorf("OpenAPI documents %q which is not registered", d)
		}
	}
}

// The upload error statuses added with the resource bounds must be documented.
func TestOpenAPIDocumentsUploadErrors(t *testing.T) {
	f := newFixture(t, 1<<20)
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]any `json:"responses"`
		} `json:"paths"`
	}
	_ = json.Unmarshal(get(f, "/api/openapi.json").Body.Bytes(), &doc)
	var have []string
	for c := range doc.Paths["/api/v1/jobs"]["post"].Responses {
		have = append(have, c)
	}
	sort.Strings(have)
	for _, c := range []string{"202", "400", "413", "422", "500", "503", "507"} {
		if !contains(have, c) {
			t.Errorf("POST /api/v1/jobs missing %s in %v", c, have)
		}
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestUnmatchedAPIAndMethodsAreProblemJSON(t *testing.T) {
	f := newFixture(t, 1<<20)
	for _, p := range []string{"/api", "/api/", "/api/nope", "/api/v1", "/api/v1/jobs/", "/api/v1/jobs/" + uuid.NewString() + "/nope", "/api/v2/jobs"} {
		w := get(f, p)
		if w.Code != 404 {
			t.Errorf("GET %s: want 404 got %d", p, w.Code)
			continue
		}
		problemOf(t, w)
	}
	for _, c := range []struct{ m, p, allow string }{
		{"PUT", "/api/v1/jobs", "GET"}, {"PATCH", "/api/v1/jobs/" + uuid.NewString(), "DELETE"},
		{"POST", "/api/v1/presets", "GET"}, {"POST", "/healthz", "GET"}, {"DELETE", "/metrics", "GET"},
		{"POST", "/api/openapi.json", "GET"}, {"POST", "/api/v1/jobs/" + uuid.NewString() + "/events", "GET"},
	} {
		w := do(f, c.m, c.p)
		if w.Code != 405 {
			t.Errorf("%s %s: want 405 got %d", c.m, c.p, w.Code)
			continue
		}
		problemOf(t, w)
		if !strings.Contains(w.Header().Get("Allow"), c.allow) {
			t.Errorf("%s %s: Allow=%q", c.m, c.p, w.Header().Get("Allow"))
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: security headers missing", c.m, c.p)
		}
	}
}

func TestNonAPIPathsStillServeUI(t *testing.T) {
	f := newFixture(t, 1<<20)
	w := get(f, "/")
	if w.Code != 200 || strings.HasPrefix(w.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("UI root: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestSSEStreamsUntilDone(t *testing.T) {
	f := newFixture(t, 1<<20)
	ctx := context.Background()
	id := uuid.NewString()
	_ = f.store.Create(ctx, &jobs.Job{ID: id, Filename: "a", InputPath: "/x", Options: jobs.Options{Preset: "audio-wav"}})
	srv := httptest.NewServer(f.h)
	defer srv.Close()

	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = f.store.Claim(ctx, "w1")
		_ = f.store.SetRunning(ctx, id, "pcm_s16le")
		_ = f.store.UpdateProgress(ctx, id, 50)
		time.Sleep(100 * time.Millisecond)
		_ = f.store.Complete(ctx, id, "/out")
	}()

	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/jobs/"+id+"/events", nil)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(cctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type %q", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing on SSE")
	}
	sc := bufio.NewScanner(resp.Body)
	sawDone := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"status":"done"`) {
			sawDone = true
			break
		}
	}
	if !sawDone {
		t.Fatal("stream ended without a done event")
	}
	// The server must end the stream by itself right after the terminal event.
	rest, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(rest), `"status":"running"`) {
		t.Fatal("events after terminal status")
	}
}

func queuedJob(t *testing.T, f fixture) string {
	t.Helper()
	id := uuid.NewString()
	if err := f.store.Create(context.Background(), &jobs.Job{ID: id, Filename: "a", InputPath: "/x", Options: jobs.Options{Preset: "audio-wav"}}); err != nil {
		t.Fatal(err)
	}
	return id
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSSEConcurrencyCapAndRelease(t *testing.T) {
	f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) { c.MaxSSEStreams = 2 })
	id := queuedJob(t, f.fixture)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	open := func() (*http.Response, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/jobs/"+id+"/events", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, cancel
	}
	r1, c1 := open()
	r2, c2 := open()
	defer c2()
	if r1.StatusCode != 200 || r2.StatusCode != 200 {
		t.Fatalf("first two streams: %d %d", r1.StatusCode, r2.StatusCode)
	}
	r3, c3 := open()
	defer c3()
	if r3.StatusCode != 503 || r3.Header.Get("Retry-After") == "" || !strings.HasPrefix(r3.Header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("third stream: %d %v", r3.StatusCode, r3.Header)
	}
	r3.Body.Close()
	// Dropping a client releases its slot.
	c1()
	r1.Body.Close()
	waitFor(t, "slot release", func() bool {
		r, c := open()
		defer c()
		defer r.Body.Close()
		return r.StatusCode == 200
	})
	if m := get(f.fixture, "/metrics").Body.String(); !strings.Contains(m, "convertflow_sse_streams") {
		t.Fatalf("sse gauge missing:\n%s", m)
	}
}

func TestSSELifetimeAndHeartbeat(t *testing.T) {
	f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) {
		d.SSEHeartbeat = 40 * time.Millisecond
		d.SSEMaxLifetime = 400 * time.Millisecond
		d.EventInterval = 10 * time.Millisecond
	})
	id := queuedJob(t, f.fixture)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/jobs/"+id+"/events", nil)
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body) // returns only when the server ends the stream
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("stream outlived its maximum lifetime")
	}
	s := string(b)
	if n := strings.Count(s, "data: "); n != 1 {
		t.Fatalf("unchanged job must be sent once, got %d:\n%s", n, s)
	}
	if !strings.Contains(s, ": keepalive") {
		t.Fatalf("no heartbeat comment:\n%s", s)
	}
}

func TestSSEStopsOnClientDisconnect(t *testing.T) {
	f := newFixture(t, 1<<20)
	id := queuedJob(t, f)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/jobs/"+id+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stream registered", func() bool {
		return strings.Contains(get(f, "/metrics").Body.String(), "convertflow_sse_streams 1")
	})
	cancel()
	resp.Body.Close()
	waitFor(t, "stream released", func() bool {
		return strings.Contains(get(f, "/metrics").Body.String(), "convertflow_sse_streams 0")
	})
}

func TestSSEUnknownJobIs404(t *testing.T) {
	f := newFixture(t, 1<<20)
	w := get(f, "/api/v1/jobs/"+uuid.NewString()+"/events")
	if w.Code != 404 {
		t.Fatalf("got %d", w.Code)
	}
	problemOf(t, w)
}

func TestSSEEndsWhenJobDeleted(t *testing.T) {
	f := newFixture(t, 1<<20)
	id := queuedJob(t, f)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/jobs/"+id+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = f.store.Delete(context.Background(), id)
	}()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("stream should end cleanly: %v", err)
	}
}

// Over a real connection the chunked, deadline-renewing writer must deliver the complete file
// and honour Range.
func TestOutputOverRealServerLargeAndRange(t *testing.T) {
	f := newFixture(t, 1<<20)
	big := strings.Repeat("0123456789abcdef", 200_000) // 3.2 MB, > several chunks
	id := seedDone(t, f, "big.mp3", "audio-wav", big)
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/jobs/" + id + "/output")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(b) != big {
		t.Fatalf("full download mismatch: %d bytes err=%v", len(b), err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/jobs/"+id+"/output", nil)
	req.Header.Set("Range", "bytes=1000000-1000015")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(b) != big[1000000:1000016] {
		t.Fatalf("range: %d %q", resp.StatusCode, b)
	}
}
