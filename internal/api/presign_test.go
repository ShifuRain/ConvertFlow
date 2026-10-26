package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
	"github.com/google/uuid"
)

// fakePresign is a Store + Presigner: multipart uploads still go to a LocalFS, while "bucket"
// objects (in/<id>, out/...) live in a map.
type fakePresign struct {
	*blob.LocalFS
	mu       sync.Mutex
	objects  map[string]int64
	lastDL   blob.DownloadHeaders
	lastTTL  time.Duration
	upTTL    time.Duration
	upMax    int64
	statErr  error
	statHits int
}

const (
	fakePolicy = "SECRET-POLICY-VALUE"
	fakeSig    = "SECRET-SIGNATURE-VALUE"
)

func newFakePresign(dir string) *fakePresign {
	return &fakePresign{LocalFS: blob.OpenLocalFS(dir), objects: map[string]int64{}}
}

func (f *fakePresign) put(key string, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = size
}

func (f *fakePresign) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[key]
	return ok
}

func (f *fakePresign) PresignUpload(_ context.Context, id string, maxBytes int64, ttl time.Duration) (blob.PresignedPost, error) {
	f.mu.Lock()
	f.upTTL, f.upMax = ttl, maxBytes
	f.mu.Unlock()
	key := "in/" + id
	return blob.PresignedPost{URL: "https://bucket.example/cf", Key: key,
		Fields: map[string]string{"key": key, "policy": fakePolicy, "x-amz-signature": fakeSig}}, nil
}

func (f *fakePresign) StatInput(_ context.Context, key string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statHits++
	if f.statErr != nil {
		return 0, f.statErr
	}
	n, ok := f.objects[key]
	if !ok {
		return 0, fs.ErrNotExist
	}
	return n, nil
}

func (f *fakePresign) PresignDownload(_ context.Context, key string, h blob.DownloadHeaders, ttl time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; !ok {
		return "", fs.ErrNotExist
	}
	f.lastDL, f.lastTTL = h, ttl
	return "https://bucket.example/cf/" + key + "?X-Amz-Signature=" + fakeSig, nil
}

func (f *fakePresign) Remove(ctx context.Context, loc string) error {
	if strings.HasPrefix(loc, "in/") || strings.HasPrefix(loc, "out/") {
		f.mu.Lock()
		delete(f.objects, loc)
		f.mu.Unlock()
		return nil
	}
	return f.LocalFS.Remove(ctx, loc)
}

func (f *fakePresign) FreeBytes(context.Context) (int64, error) { return 0, errors.ErrUnsupported }

type presignFixture struct {
	auditFixture
	fake *fakePresign
}

func newPresignFixture(t *testing.T, maxUpload int64, mod func(*config.Config)) presignFixture {
	t.Helper()
	var fake *fakePresign
	f := newFixtureWith(t, maxUpload, func(c *config.Config, d *api.Deps) {
		c.MinFreeBytes = 1 << 40 // must be skipped: the store has no free-space figure
		c.S3.PresignTTL = 10 * time.Minute
		if mod != nil {
			mod(c)
		}
		fake = newFakePresign(c.DataDir)
		d.Blob = fake
	})
	return presignFixture{f, fake}
}

func postJSON(f fixture, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = "192.0.2.7:1234"
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

type presignResp struct {
	ID     string `json:"id"`
	Upload struct {
		URL    string            `json:"url"`
		Fields map[string]string `json:"fields"`
	} `json:"upload"`
	ExpiresAt time.Time `json:"expires_at"`
}

func createPresigned(t *testing.T, f presignFixture, body string) presignResp {
	t.Helper()
	w := postJSON(f.fixture, "/api/v1/jobs", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var v presignResp
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func complete(f fixture, id string) *httptest.ResponseRecorder {
	return postJSON(f, "/api/v1/jobs/"+id+"/complete", "")
}

func countJobs(t *testing.T, f fixture) int {
	t.Helper()
	js, err := f.store.List(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(js)
}

func TestPresignCreate(t *testing.T) {
	f := newPresignFixture(t, 1000, nil)
	before := time.Now()
	w := postJSON(f.fixture, "/api/v1/jobs", `{"preset":"audio-wav","filename":"../x/so‮ng.mp3","size":4}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var v presignResp
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v.ID == "" || w.Header().Get("Location") != "/api/v1/jobs/"+v.ID {
		t.Fatalf("response %s / %v", w.Body, w.Header())
	}
	if v.Upload.URL != "https://bucket.example/cf" || v.Upload.Fields["key"] != "in/"+v.ID || v.Upload.Fields["policy"] != fakePolicy {
		t.Fatalf("upload form %+v", v.Upload)
	}
	if d := v.ExpiresAt.Sub(before); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("expires_at %v", v.ExpiresAt)
	}
	if f.fake.upTTL != 10*time.Minute || f.fake.upMax != 1000 {
		t.Fatalf("presign called with ttl %v max %d", f.fake.upTTL, f.fake.upMax)
	}
	j, err := f.store.Get(context.Background(), v.ID)
	if err != nil || j.Status != jobs.StatusUploading || j.Filename != "song.mp3" || j.InputPath != "in/"+v.ID ||
		j.DeclaredSize != 4 || j.Options.Preset != "audio-wav" {
		t.Fatalf("stored job %+v %v", j, err)
	}
	// The job is visible as uploading, without an output URL.
	g := get(f.fixture, "/api/v1/jobs/"+v.ID)
	if !strings.Contains(g.Body.String(), `"status":"uploading"`) || strings.Contains(g.Body.String(), "in/") {
		t.Fatalf("job view %s", g.Body)
	}
	log := f.log.String()
	if !strings.Contains(log, `"event":"upload_presigned"`) || !strings.Contains(log, v.ID) {
		t.Fatalf("missing audit: %s", log)
	}
	for _, secret := range []string{fakePolicy, fakeSig, "bucket.example"} {
		if strings.Contains(log, secret) {
			t.Fatalf("audit log leaks %q: %s", secret, log)
		}
	}
	// options form works as well
	w = postJSON(f.fixture, "/api/v1/jobs", `{"options":{"preset":"audio-flac"},"filename":"a.wav","size":1000}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("options: %d %s", w.Code, w.Body)
	}
	// Content-Type parameters are accepted.
	r := httptest.NewRequest("POST", "/api/v1/jobs", strings.NewReader(`{"preset":"audio-wav","size":1}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	if rec.Code != http.StatusCreated {
		t.Fatalf("charset param: %d", rec.Code)
	}
}

// The JSON path validates like the multipart path: same statuses for the same problems, and no
// job is created for a refused request.
func TestPresignCreateValidation(t *testing.T) {
	f := newPresignFixture(t, 1000, nil)
	cases := []struct {
		name, body string
		code       int
		multipart  []field // the equivalent multipart request, when there is one
	}{
		{"both", `{"preset":"audio-wav","options":{"preset":"audio-wav"},"size":4}`, 400, []field{{"preset", "audio-wav"}, {"options", `{"preset":"audio-wav"}`}}},
		{"neither", `{"filename":"a","size":4}`, 400, nil},
		{"empty preset", `{"preset":"  ","size":4}`, 400, nil},
		{"long preset", `{"preset":"` + strings.Repeat("a", 129) + `","size":4}`, 400, nil},
		{"unknown field", `{"preset":"audio-wav","size":4,"acl":"public-read"}`, 422, nil},
		{"unknown option", `{"options":{"preset":"audio-wav","x":1},"size":4}`, 422, []field{{"options", `{"preset":"audio-wav","x":1}`}}},
		{"bad crf", `{"options":{"preset":"video-vp9","crf":99},"size":4}`, 422, []field{{"options", `{"preset":"video-vp9","crf":99}`}}},
		{"unknown preset", `{"preset":"nope","size":4}`, 422, []field{{"preset", "nope"}}},
		{"preset not runnable", `{"preset":"video-vp9","size":4}`, 422, []field{{"preset", "video-vp9"}}},
		{"trailing data", `{"preset":"audio-wav","size":4} {}`, 422, nil},
		{"not json", `preset=audio-wav`, 422, nil},
		{"missing size", `{"preset":"audio-wav"}`, 400, nil},
		{"zero size", `{"preset":"audio-wav","size":0}`, 400, nil},
		{"negative size", `{"preset":"audio-wav","size":-5}`, 400, nil},
		{"oversize", `{"preset":"audio-wav","size":1001}`, 413, nil},
		{"huge body", `{"preset":"audio-wav","size":4,"filename":"` + strings.Repeat("a", 9000) + `"}`, 400, nil},
	}
	for _, c := range cases {
		w := postJSON(f.fixture, "/api/v1/jobs", c.body)
		if w.Code != c.code {
			t.Errorf("%s: status %d, want %d: %s", c.name, w.Code, c.code, w.Body)
			continue
		}
		problemOf(t, w)
		if c.multipart != nil {
			b, ct := multipartBody(t, c.multipart, "a.wav", []byte("data"))
			if m := post(f.fixture, b, ct); m.Code != c.code {
				t.Errorf("%s: multipart status %d differs from JSON %d", c.name, m.Code, c.code)
			}
		}
	}
	if n := countJobs(t, f.fixture); n != 0 {
		t.Fatalf("%d jobs created by refused requests", n)
	}
	// Oversize parity: the multipart path refuses a file above the limit with the same status.
	b, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte("x"), 1001))
	if m := post(f.fixture, b, ct); m.Code != 413 {
		t.Fatalf("multipart oversize: %d", m.Code)
	}
}

// Presigned and multipart uploads share the queue cap; uploading jobs count against it.
func TestPresignQueueFullParity(t *testing.T) {
	f := newPresignFixture(t, 1000, func(c *config.Config) { c.MaxQueuedJobs = 1 })
	createPresigned(t, f, `{"preset":"audio-wav","size":4}`)
	w := postJSON(f.fixture, "/api/v1/jobs", `{"preset":"audio-wav","size":4}`)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("JSON over the cap: %d %v", w.Code, w.Header())
	}
	problemOf(t, w)
	b, ct := okBody(t)
	if m := post(f.fixture, b, ct); m.Code != 503 {
		t.Fatalf("multipart over the cap: %d", m.Code)
	}
	if !hasRejectAudit(f.auditFixture, 503, "queue full") {
		t.Fatalf("missing reject audit: %s", f.log)
	}
}

// The upload slot is shared too: a JSON request is refused while all slots are taken.
func TestPresignUsesUploadSlot(t *testing.T) {
	f := newPresignFixture(t, 1<<20, func(c *config.Config) { c.MaxConcurrentUploads = 1 })
	pr, pw := ioPipe()
	_, ct := okBody(t)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(f.fixture, pr, ct) }()
	_, _ = pw.Write([]byte("--x\r\n"))
	var w *httptest.ResponseRecorder
	for i := 0; i < 200; i++ {
		if w = postJSON(f.fixture, "/api/v1/jobs", `{"preset":"audio-wav","size":4}`); w.Code == 503 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w.Code != 503 {
		t.Fatalf("JSON create with no free slot: %d", w.Code)
	}
	_ = pw.CloseWithError(errors.New("gone"))
	<-done
	createPresigned(t, f, `{"preset":"audio-wav","size":4}`)
}

func TestPresignCompleteFlow(t *testing.T) {
	f := newPresignFixture(t, 1000, nil)
	v := createPresigned(t, f, `{"preset":"audio-wav","filename":"a.wav","size":4}`)

	// Nothing uploaded yet.
	w := complete(f.fixture, v.ID)
	if w.Code != http.StatusConflict {
		t.Fatalf("complete before upload: %d %s", w.Code, w.Body)
	}
	problemOf(t, w)
	if j, _ := f.store.Get(context.Background(), v.ID); j.Status != jobs.StatusUploading {
		t.Fatalf("job left uploading: %s", j.Status)
	}

	f.fake.put("in/"+v.ID, 4)
	w = complete(f.fixture, v.ID)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"queued"`) {
		t.Fatalf("complete: %d %s", w.Code, w.Body)
	}
	// Idempotent: the second call returns the job unchanged, without another stat or audit.
	hits := f.fake.statHits
	w = complete(f.fixture, v.ID)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"queued"`) || f.fake.statHits != hits {
		t.Fatalf("second complete: %d %s", w.Code, w.Body)
	}
	if n := strings.Count(f.log.String(), `"event":"job_created"`); n != 1 {
		t.Fatalf("job_created audited %d times", n)
	}
	// The queued job is claimable by a worker and still returned (200) once it moved on.
	c, err := f.store.Claim(context.Background(), "w1", presets.IDs(presets.All()))
	if err != nil || c == nil || c.ID != v.ID || c.InputPath != "in/"+v.ID {
		t.Fatalf("claim %+v %v", c, err)
	}
	if w = complete(f.fixture, v.ID); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"probing"`) {
		t.Fatalf("complete on a claimed job: %d %s", w.Code, w.Body)
	}

	for _, id := range []string{uuid.NewString(), "not-a-uuid", strings.ToUpper(v.ID)} {
		if w := complete(f.fixture, id); w.Code != http.StatusNotFound {
			t.Errorf("complete(%q): %d", id, w.Code)
		}
	}
	if w := do(f.fixture, "GET", "/api/v1/jobs/"+v.ID+"/complete"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET complete: %d", w.Code)
	}
}

// The stored size is the authority: empty, oversize and mismatching objects are refused and
// removed, and the job stays uploading so the client can retry while the form is valid.
func TestPresignCompleteSizeChecks(t *testing.T) {
	f := newPresignFixture(t, 1000, nil)
	cases := []struct {
		name     string
		declared int64
		stored   int64
		code     int
	}{
		{"empty object", 4, 0, 400},
		{"oversize object", 1000, 1001, 413},
		{"smaller than declared", 4, 3, 409},
		{"larger than declared", 4, 5, 409},
	}
	for _, c := range cases {
		v := createPresigned(t, f, `{"preset":"audio-wav","size":`+itoa(c.declared)+`}`)
		key := "in/" + v.ID
		f.fake.put(key, c.stored)
		w := complete(f.fixture, v.ID)
		if w.Code != c.code {
			t.Errorf("%s: %d, want %d: %s", c.name, w.Code, c.code, w.Body)
			continue
		}
		problemOf(t, w)
		if f.fake.has(key) {
			t.Errorf("%s: refused object not removed", c.name)
		}
		if j, _ := f.store.Get(context.Background(), v.ID); j.Status != jobs.StatusUploading {
			t.Errorf("%s: job status %s", c.name, j.Status)
		}
		// A correct re-upload is then accepted.
		f.fake.put(key, c.declared)
		if w := complete(f.fixture, v.ID); w.Code != 200 {
			t.Errorf("%s: retry: %d %s", c.name, w.Code, w.Body)
		}
	}
	// A store failure is a 500 with a generic body.
	v := createPresigned(t, f, `{"preset":"audio-wav","size":4}`)
	f.fake.statErr = errors.New("dial tcp minio.internal:9000: secret-bucket refused")
	w := complete(f.fixture, v.ID)
	f.fake.statErr = nil
	if w.Code != 500 || strings.Contains(w.Body.String(), "minio") || strings.Contains(w.Body.String(), "secret-bucket") {
		t.Fatalf("stat failure: %d %s", w.Code, w.Body)
	}
}

func TestPresignCompleteAfterExpiry(t *testing.T) {
	f := newPresignFixture(t, 1000, nil)
	v := createPresigned(t, f, `{"preset":"audio-wav","size":4}`)
	if _, err := f.store.ExpireUploading(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	f.fake.put("in/"+v.ID, 4)
	w := complete(f.fixture, v.ID)
	if w.Code != http.StatusConflict {
		t.Fatalf("complete after expiry: %d %s", w.Code, w.Body)
	}
	problemOf(t, w)
}

func TestPresignDeleteUploadingJob(t *testing.T) {
	f := newPresignFixture(t, 1000, nil)
	v := createPresigned(t, f, `{"preset":"audio-wav","size":4}`)
	f.fake.put("in/"+v.ID, 4)
	if w := del(f.fixture, "/api/v1/jobs/"+v.ID); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if f.fake.has("in/" + v.ID) {
		t.Fatal("uploaded object not removed with the job")
	}
	if w := complete(f.fixture, v.ID); w.Code != http.StatusNotFound {
		t.Fatalf("complete after delete: %d", w.Code)
	}
}

// Multipart uploads keep working with a presigning store; the free-space check is skipped
// because the store has no free-space figure.
func TestPresignStoreStillAcceptsMultipart(t *testing.T) {
	f := newPresignFixture(t, 1000, nil)
	b, ct := okBody(t)
	if w := post(f.fixture, b, ct); w.Code != 202 {
		t.Fatalf("multipart: %d %s", w.Code, w.Body)
	}
}

func seedDoneAt(t *testing.T, f fixture, filename, preset, outLoc string) string {
	t.Helper()
	ctx := context.Background()
	id := strings.SplitN(strings.TrimPrefix(outLoc, "out/"), ".", 2)[0]
	if err := f.store.Create(ctx, &jobs.Job{ID: id, Filename: filename, InputPath: "in/" + id, Options: jobs.Options{Preset: preset}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Claim(ctx, "w1", presets.IDs(presets.All())); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetRunning(ctx, id, "w1", "pcm_s16le"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Complete(ctx, id, "w1", outLoc); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPresignOutputRedirect(t *testing.T) {
	f := newPresignFixture(t, 1000, func(c *config.Config) { c.S3.PresignTTL = time.Hour })
	id := uuid.NewString()
	loc := "out/" + id + ".1.wav"
	seedDoneAt(t, f.fixture, `my "song".flac`, "audio-wav", loc)
	f.fake.put(loc, 10)

	w := get(f.fixture, "/api/v1/jobs/"+id+"/output")
	if w.Code != http.StatusFound {
		t.Fatalf("output: %d %s", w.Code, w.Body)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil || u.Host != "bucket.example" || u.Path != "/cf/"+loc {
		t.Fatalf("Location %q", w.Header().Get("Location"))
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	want := api.ContentDisposition(`my "song".wav`)
	if f.fake.lastDL.ContentDisposition != want || f.fake.lastDL.ContentType != "audio/wav" {
		t.Fatalf("download headers %+v, want %q", f.fake.lastDL, want)
	}
	if f.fake.lastTTL <= 0 || f.fake.lastTTL > 5*time.Minute {
		t.Fatalf("download URL lifetime %v must be short", f.fake.lastTTL)
	}
	if strings.Contains(f.log.String(), fakeSig) {
		t.Fatal("presigned URL logged")
	}

	// Missing object: 410 like a removed local file.
	id2 := uuid.NewString()
	seedDoneAt(t, f.fixture, "b.wav", "audio-wav", "out/"+id2+".1.wav")
	if w := get(f.fixture, "/api/v1/jobs/"+id2+"/output"); w.Code != http.StatusGone {
		t.Fatalf("missing output: %d", w.Code)
	}
	// Not done: 409, no redirect.
	v := createPresigned(t, f, `{"preset":"audio-wav","size":4}`)
	if w := get(f.fixture, "/api/v1/jobs/"+v.ID+"/output"); w.Code != http.StatusConflict {
		t.Fatalf("output of an uploading job: %d", w.Code)
	}
}

// Without a presigning store (local storage) the direct-upload API does not exist.
func TestPresignDisabledWithLocalStorage(t *testing.T) {
	f := newFixture(t, 1000)
	w := postJSON(f, "/api/v1/jobs", `{"preset":"audio-wav","size":4}`)
	if w.Code != http.StatusBadRequest { // unchanged: expected multipart/form-data
		t.Fatalf("JSON create in local mode: %d %s", w.Code, w.Body)
	}
	problemOf(t, w)
	if w := complete(f, uuid.NewString()); w.Code != http.StatusNotFound {
		t.Fatalf("complete in local mode: %d", w.Code)
	}
	if n := countJobs(t, f); n != 0 {
		t.Fatalf("%d jobs created", n)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func ioPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// abortRecorder is a streaming-store writer (like the S3 store's) that records how it was ended.
type abortRecorder struct {
	bytes.Buffer
	closed, aborted bool
}

func (a *abortRecorder) Close() error               { a.closed = true; return nil }
func (a *abortRecorder) CloseWithError(error) error { a.aborted = true; return nil }

// A rejected multipart upload aborts a streaming store's write instead of committing it.
func TestMultipartAbortsStreamingStoreOnRejection(t *testing.T) {
	var last *abortRecorder
	f := newFixtureWith(t, 100, func(c *config.Config, d *api.Deps) {
		d.Blob = hookBlob{LocalFS: blob.OpenLocalFS(c.DataDir), create: func(_ context.Context, id string) (io.WriteCloser, string, error) {
			last = &abortRecorder{}
			return last, "in/" + id, nil
		}}
	})
	cases := []struct {
		name string
		body []byte
		code int
	}{
		{"oversize", bytes.Repeat([]byte("x"), 101), 413},
		{"empty", nil, 400},
		{"ok", []byte("data"), 202},
	}
	for _, c := range cases {
		b, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", c.body)
		if c.body == nil {
			b, ct = multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte{})
		}
		w := post(f.fixture, b, ct)
		if w.Code != c.code {
			t.Fatalf("%s: %d", c.name, w.Code)
		}
		if c.code == 202 {
			if !last.closed || last.aborted {
				t.Fatalf("%s: accepted upload must be closed, not aborted", c.name)
			}
		} else if !last.aborted || last.closed {
			t.Fatalf("%s: rejected upload must be aborted, not committed", c.name)
		}
	}
	// A client that disconnects mid-file aborts too.
	b, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte("y"), 80))
	if w := post(f.fixture, failingReader{io.LimitReader(b, int64(b.Len()-60))}, ct); w.Code != 400 {
		t.Fatalf("interrupted: %d", w.Code)
	}
	if !last.aborted || last.closed {
		t.Fatal("interrupted upload must be aborted")
	}
}
