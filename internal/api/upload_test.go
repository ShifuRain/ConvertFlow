package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
)

type fixture struct {
	h     http.Handler
	store *jobs.Store
	dir   string
}

func newFixture(t *testing.T, maxUpload int64) fixture {
	t.Helper()
	pool := dbtest.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, "out"), 0o700)
	store := jobs.NewStore(pool)
	_ = store.UpsertWorker(context.Background(), "w1", []string{"pcm_s16le", "libopus", "flac"})
	cfg := config.Config{DataDir: dir, MaxUploadBytes: maxUpload, Retention: time.Hour}
	h := api.NewHandler(api.Deps{Store: store, Cfg: cfg, Audit: audit.New(io.Discard), EventInterval: 20 * time.Millisecond})
	return fixture{h, store, dir}
}

type field struct{ name, value string }

func multipartBody(t *testing.T, fields []field, fileName string, file []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		_ = mw.WriteField(f.name, f.value)
	}
	if fileName != "" || file != nil {
		fw, _ := mw.CreateFormFile("file", fileName)
		_, _ = fw.Write(file)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func post(f fixture, body io.Reader, ctype string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/v1/jobs", body)
	r.Header.Set("Content-Type", ctype)
	r.RemoteAddr = "192.0.2.7:1234"
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func inFiles(t *testing.T, f fixture) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(f.dir, "in"))
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func problemOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content-type %q", ct)
	}
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m
}

func TestUploadCreatesJob(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "song.mp3", []byte("data"))
	w := post(f, body, ct)
	if w.Code != 202 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	id, _ := v["id"].(string)
	if id == "" || v["status"] != "queued" || w.Header().Get("Location") != "/api/v1/jobs/"+id {
		t.Fatalf("bad response %v / %v", v, w.Header())
	}
	if names := inFiles(t, f); len(names) != 1 || names[0] != id {
		t.Fatalf("input files %v", names)
	}
	j, err := f.store.Get(context.Background(), id)
	if err != nil || j.Filename != "song.mp3" || j.Options.Preset != "audio-wav" {
		t.Fatalf("bad stored job %+v %v", j, err)
	}
}

func TestUploadOptionsJSON(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"options", `{"preset":"audio-flac"}`}}, "a.wav", []byte("data"))
	if w := post(f, body, ct); w.Code != 202 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	body, ct = multipartBody(t, []field{{"options", `{"preset":"audio-flac","evil":"1"}`}}, "a.wav", []byte("data"))
	if w := post(f, body, ct); w.Code != 422 {
		t.Fatalf("unknown option field must be 422, got %d", w.Code)
	}
}

func TestUploadRejectsFileBeforePreset(t *testing.T) {
	f := newFixture(t, 1<<20)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write([]byte("data"))
	_ = mw.WriteField("preset", "audio-wav")
	_ = mw.Close()
	w := post(f, &buf, mw.FormDataContentType())
	if w.Code != 400 {
		t.Fatalf("want 400 got %d", w.Code)
	}
	problemOf(t, w)
	if len(inFiles(t, f)) != 0 {
		t.Fatal("no file may be stored")
	}
}

func TestUploadRejectsMissingFile(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "", nil)
	if w := post(f, body, ct); w.Code != 400 {
		t.Fatalf("want 400 got %d", w.Code)
	}
}

func TestUploadRejectsEmptyFile(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "empty.mp4", []byte{})
	w := post(f, body, ct)
	if w.Code != 400 || len(inFiles(t, f)) != 0 {
		t.Fatalf("want 400 and no files, got %d %v", w.Code, inFiles(t, f))
	}
}

func TestUploadRejectsUnavailablePreset(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "video-av1"}}, "a.mp4", []byte("data"))
	w := post(f, body, ct)
	if w.Code != 422 {
		t.Fatalf("want 422 got %d", w.Code)
	}
	problemOf(t, w)
	body, ct = multipartBody(t, []field{{"preset", "nonexistent"}}, "a.mp4", []byte("data"))
	if w := post(f, body, ct); w.Code != 422 {
		t.Fatalf("unknown preset: want 422 got %d", w.Code)
	}
	if len(inFiles(t, f)) != 0 {
		t.Fatal("rejected uploads must not leave files")
	}
}

func TestUploadTooLarge(t *testing.T) {
	f := newFixture(t, 1000)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "big.wav", bytes.Repeat([]byte("x"), 200_000))
	w := post(f, body, ct)
	if w.Code != 413 {
		t.Fatalf("want 413 got %d", w.Code)
	}
	problemOf(t, w)
	if len(inFiles(t, f)) != 0 {
		t.Fatalf("partial file left behind: %v", inFiles(t, f))
	}
}

type failingReader struct{ r io.Reader }

func (f failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

func TestUploadAborted(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte("x"), 5000))
	truncated := failingReader{io.LimitReader(body, int64(body.Len()-200))}
	w := post(f, truncated, ct)
	if w.Code != 400 {
		t.Fatalf("want 400 got %d", w.Code)
	}
	if len(inFiles(t, f)) != 0 {
		t.Fatalf("partial file left behind: %v", inFiles(t, f))
	}
	jobsList, _ := f.store.List(context.Background(), 10)
	if len(jobsList) != 0 {
		t.Fatal("no job may be created for an aborted upload")
	}
}

func TestUploadHostileFilenameStaysInsideDataDir(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "../../evil\"x.mp4", []byte("data"))
	w := post(f, body, ct)
	if w.Code != 202 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	name, _ := v["filename"].(string)
	if strings.ContainsAny(name, "/\\\n\r\"") || name == "" {
		t.Fatalf("unsafe stored filename %q", name)
	}
	parent := filepath.Dir(f.dir)
	if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
		t.Fatal("path traversal succeeded")
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"song.mp3":         "song.mp3",
		"../../etc/passwd": "passwd",
		`C:\Users\x\a.mp4`: "a.mp4",
		"a\nb\r.mp4":       "ab.mp4",
		`we"ird.mp4`:       "weird.mp4",
		"":                 "upload",
		"..":               "upload",
		"   ":              "upload",
		"Müller – Ünï.mp4": "Müller – Ünï.mp4",
	}
	for in, want := range cases {
		if got := api.SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	long := api.SanitizeFilename(strings.Repeat("ü", 400) + ".mp4")
	if len(long) > 200 || !strings.HasSuffix(long, ".mp4") && len(long) == 0 {
		t.Errorf("long name not truncated sensibly: %d bytes", len(long))
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t, 1<<20)
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	h := w.Header()
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" ||
		h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing security headers: %v", h)
	}
}

func TestAuditLogOnCreate(t *testing.T) {
	pool := dbtest.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	store := jobs.NewStore(pool)
	_ = store.UpsertWorker(context.Background(), "w1", []string{"pcm_s16le"})
	var logBuf bytes.Buffer
	h := api.NewHandler(api.Deps{Store: store, Cfg: config.Config{DataDir: dir, MaxUploadBytes: 1 << 20}, Audit: audit.New(&logBuf)})
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte("SECRET"))
	r := httptest.NewRequest("POST", "/api/v1/jobs", body)
	r.Header.Set("Content-Type", ct)
	r.RemoteAddr = "192.0.2.9:1"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !strings.Contains(logBuf.String(), `"event":"job_created"`) || !strings.Contains(logBuf.String(), "192.0.2.9") ||
		strings.Contains(logBuf.String(), "SECRET") {
		t.Fatalf("bad audit log: %s", logBuf.String())
	}
}

func TestSanitizeFilenameHostile(t *testing.T) {
	cases := map[string]string{
		"a\x00b.mp4":             "ab.mp4",
		"evil\r\nSet-Cookie: x":  "evilSet-Cookie: x",
		".":                      "upload",
		"/":                      "upload",
		"/etc/passwd":            "passwd",
		"a\u202eexe.mp4":         "aexe.mp4", // RTL override stripped
		"a\u2066b\u2069.mp4":     "ab.mp4",   // bidi isolates stripped
		"bad\xff\xfeutf8.mp4":    "badutf8.mp4",
		"\u200b\u200e":           "upload", // zero-width / LRM only
		"dir/../..":              "upload",
		"..\\..\\windows\\a.mp4": "a.mp4",
	}
	for in, want := range cases {
		if got := api.SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{strings.Repeat("a", 5000), strings.Repeat("\u202e\U0001F600", 300) + ".mp4"} {
		got := api.SanitizeFilename(in)
		if len(got) > 200 || !utf8.ValidString(got) || got == "" {
			t.Errorf("bad truncation: %d bytes valid=%v", len(got), utf8.ValidString(got))
		}
	}
}

func TestUploadHostileFilenamesNeverEscapeInputDir(t *testing.T) {
	names := []string{
		"../../etc/passwd", "/etc/passwd", `C:\Windows\system32\x.exe`,
		strings.Repeat("A", 100000) + ".mp4", "\u202egpj.mp4", `q"uote.mp4`,
		".", "..", "", "...", " ", "bad\xffutf8.mp4",
	}
	for _, name := range names {
		f := newFixture(t, 1<<20)
		body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, name, []byte("data"))
		w := post(f, body, ct)
		if w.Code != 202 {
			t.Fatalf("%q: status %d: %s", name, w.Code, w.Body)
		}
		var v map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		id, _ := v["id"].(string)
		// Only the server-generated id exists under in/, and nothing else anywhere in the data dir tree.
		var found []string
		_ = filepath.WalkDir(f.dir, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				rel, _ := filepath.Rel(f.dir, p)
				found = append(found, rel)
			}
			return nil
		})
		if len(found) != 1 || found[0] != filepath.Join("in", id) {
			t.Fatalf("%q: unexpected files %v", name, found)
		}
		j, err := f.store.Get(context.Background(), id)
		if err != nil || j.InputPath != filepath.Join(f.dir, "in", id) {
			t.Fatalf("%q: stored input path %+v %v", name, j, err)
		}
		fn, _ := v["filename"].(string)
		if fn == "" || strings.ContainsAny(fn, "/\\\x00\r\n\"") || !utf8.ValidString(fn) || len(fn) > 200 {
			t.Fatalf("%q: unsafe stored filename %q", name, fn)
		}
		if strings.Contains(w.Body.String(), f.dir) {
			t.Fatalf("response leaks filesystem path: %s", w.Body)
		}
	}
}

func TestContentDispositionSafe(t *testing.T) {
	hostile := []string{"x\r\nSet-Cookie: a=b", `a"b;c.mp4`, "Müller – Ünï.mp4", "\u202egpj.exe", "100%.mp4", "a\\b", "über;filename=x.mp4"}
	for _, name := range hostile {
		got := api.ContentDisposition(name)
		if strings.ContainsAny(got, "\r\n\x00") {
			t.Errorf("%q: header injection in %q", name, got)
		}
		if !strings.HasPrefix(got, "attachment; filename=\"") || !strings.Contains(got, "; filename*=UTF-8''") {
			t.Errorf("%q: missing RFC 6266 forms: %q", name, got)
		}
		for _, r := range got {
			if r > 0x7e || r < 0x20 {
				t.Errorf("%q: non-ASCII byte in header %q", name, got)
				break
			}
		}
		// The quoted fallback must contain no stray quote or backslash that would end/escape it early.
		q := got[len(`attachment; filename="`):]
		q = q[:strings.Index(q, `"`)]
		if strings.ContainsAny(q, `\;`) {
			t.Errorf("%q: unsafe fallback %q", name, q)
		}
	}
	got := api.ContentDisposition("Müller – Ünï.mp4")
	if !strings.Contains(got, "filename*=UTF-8''M%C3%BCller%20%E2%80%93%20%C3%9Cn%C3%AF.mp4") {
		t.Errorf("bad filename*: %q", got)
	}
	if got := api.ContentDisposition("song.mp3"); got != `attachment; filename="song.mp3"; filename*=UTF-8''song.mp3` {
		t.Errorf("plain: %q", got)
	}
}

func TestUploadMissingPresetWritesNothing(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, nil, "a.wav", []byte("data"))
	w := post(f, body, ct)
	if w.Code != 400 && w.Code != 422 {
		t.Fatalf("want 400/422 got %d", w.Code)
	}
	problemOf(t, w)
	if len(inFiles(t, f)) != 0 {
		t.Fatal("no bytes may be written without a preset")
	}
	l, _ := f.store.List(context.Background(), 10)
	if len(l) != 0 {
		t.Fatal("no job row")
	}
}

func TestUploadTooLargeChunkedNoLeftovers(t *testing.T) {
	f := newFixture(t, 1000)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "big.wav", bytes.Repeat([]byte("x"), 300_000))
	// MultiReader hides the length, so no Content-Length pre-check is possible.
	w := post(f, io.MultiReader(body), ct)
	if w.Code != 413 {
		t.Fatalf("want 413 got %d", w.Code)
	}
	body2 := problemOf(t, w)
	if s, _ := json.Marshal(body2); strings.Contains(string(s), f.dir) {
		t.Fatalf("problem leaks path: %s", s)
	}
	if len(inFiles(t, f)) != 0 {
		t.Fatalf("partial file left: %v", inFiles(t, f))
	}
	if l, _ := f.store.List(context.Background(), 10); len(l) != 0 {
		t.Fatal("no job row may exist")
	}
}

func TestUploadExactlyAtLimitAccepted(t *testing.T) {
	f := newFixture(t, 1000)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte("x"), 1000))
	if w := post(f, body, ct); w.Code != 202 {
		t.Fatalf("want 202 got %d: %s", w.Code, w.Body)
	}
	body, ct = multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte("x"), 1001))
	if w := post(f, io.MultiReader(body), ct); w.Code != 413 {
		t.Fatalf("limit+1 want 413 got %d", w.Code)
	}
	if len(inFiles(t, f)) != 1 {
		t.Fatalf("want exactly the accepted file: %v", inFiles(t, f))
	}
}

func TestUploadAbortedBeforeAnyFileByte(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte("x"), 5000))
	// Cut inside the part headers of the file field.
	cut := bytes.Index(body.Bytes(), []byte("filename=")) + 4
	w := post(f, failingReader{io.LimitReader(body, int64(cut))}, ct)
	if w.Code != 400 || len(inFiles(t, f)) != 0 {
		t.Fatalf("want 400 and no files, got %d %v", w.Code, inFiles(t, f))
	}
}

func TestUploadNotMultipart(t *testing.T) {
	f := newFixture(t, 1<<20)
	w := post(f, strings.NewReader(`{"preset":"audio-wav"}`), "application/json")
	if w.Code != 400 {
		t.Fatalf("want 400 got %d", w.Code)
	}
	problemOf(t, w)
}

func TestSecurityHeadersOnProblemAndAPI(t *testing.T) {
	f := newFixture(t, 1<<20)
	w := post(f, strings.NewReader("x"), "text/plain")
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers missing on API error: %v", w.Header())
	}
	csp := w.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") || !strings.Contains(csp, "script-src 'self'") ||
		!strings.Contains(csp, "frame-ancestors 'none'") {
		t.Fatalf("bad CSP %q", csp)
	}
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("HSTS must not be set without TLS")
	}
}

func TestPanicBecomesGenericProblem(t *testing.T) {
	h := api.Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom at /secret/path") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 500 || strings.Contains(w.Body.String(), "boom") || strings.Contains(w.Body.String(), "/secret") {
		t.Fatalf("leaky panic response: %d %s", w.Code, w.Body)
	}
}

// Raw CR/LF/NUL inside a multipart header value make the part header malformed (Go's reader and
// every conforming parser reject it). Either outcome is safe; what matters is no stray files/rows.
func TestUploadRawControlCharsInFilenameRejectedOrSanitized(t *testing.T) {
	for _, name := range []string{"a\x00b.mp4", "x\r\nInjected: 1.mp4", "../../evil\n\"x.mp4"} {
		f := newFixture(t, 1<<20)
		body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, name, []byte("data"))
		w := post(f, body, ct)
		l, _ := f.store.List(context.Background(), 10)
		switch w.Code {
		case 202:
			if len(l) != 1 || strings.ContainsAny(l[0].Filename, "\x00\r\n\"/\\") {
				t.Fatalf("%q: unsafe stored job %+v", name, l)
			}
		case 400:
			if len(l) != 0 || len(inFiles(t, f)) != 0 {
				t.Fatalf("%q: 400 must leave nothing behind", name)
			}
			problemOf(t, w)
		default:
			t.Fatalf("%q: unexpected status %d", name, w.Code)
		}
	}
}

// ---- resource bounds, write-side errors, strict parsing ----

type auditFixture struct {
	fixture
	log *bytes.Buffer
}

func newFixtureWith(t *testing.T, maxUpload int64, mod func(*config.Config, *api.Deps)) auditFixture {
	t.Helper()
	pool := dbtest.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	store := jobs.NewStore(pool)
	_ = store.UpsertWorker(context.Background(), "w1", []string{"pcm_s16le", "libopus", "flac"})
	cfg := config.Config{DataDir: dir, MaxUploadBytes: maxUpload, Retention: time.Hour}
	var logBuf bytes.Buffer
	d := api.Deps{Store: store, Audit: audit.New(&logBuf), EventInterval: 20 * time.Millisecond}
	mod(&cfg, &d)
	d.Cfg = cfg
	return auditFixture{fixture{api.NewHandler(d), store, dir}, &logBuf}
}

func okBody(t *testing.T) (*bytes.Buffer, string) {
	return multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte("data"))
}

func hasRejectAudit(f auditFixture, status int, reason string) bool {
	s := f.log.String()
	return strings.Contains(s, `"event":"upload_rejected"`) && strings.Contains(s, "192.0.2.7") &&
		strings.Contains(s, `"status":`+strconv.Itoa(status)) && strings.Contains(s, reason)
}

func TestUploadConcurrencyLimitAndRelease(t *testing.T) {
	f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) { c.MaxConcurrentUploads = 1 })
	pr, pw := io.Pipe()
	body, ct := okBody(t)
	_ = body
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(f.fixture, pr, ct) }()
	_, _ = pw.Write([]byte("--x\r\n")) // handler is now blocked reading the body
	var w *httptest.ResponseRecorder
	for i := 0; i < 200; i++ {
		b, c := okBody(t)
		if w = post(f.fixture, b, c); w.Code == 503 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("want 503 + Retry-After, got %d %v", w.Code, w.Header())
	}
	problemOf(t, w)
	if !hasRejectAudit(f, 503, "too many") {
		t.Fatalf("missing reject audit: %s", f.log)
	}
	_ = pw.CloseWithError(errors.New("client gone"))
	if r := <-done; r.Code != 400 {
		t.Fatalf("aborted holder: %d", r.Code)
	}
	b, c := okBody(t)
	if w := post(f.fixture, b, c); w.Code != 202 {
		t.Fatalf("slot not released after abort: %d", w.Code)
	}
}

func TestUploadSemaphoreReleasedOnEveryExitPath(t *testing.T) {
	panicNext := false
	f := newFixtureWith(t, 1000, func(c *config.Config, d *api.Deps) {
		c.MaxConcurrentUploads = 1
		d.OpenInput = func(p string) (io.WriteCloser, error) {
			if panicNext {
				panic("boom")
			}
			return api.DefaultOpenInput(p)
		}
	})
	ok := func(step string) {
		t.Helper()
		b, c := okBody(t)
		if w := post(f.fixture, b, c); w.Code != 202 {
			t.Fatalf("after %s: slot leaked, got %d", step, w.Code)
		}
	}
	ok("start")
	big, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "b.wav", bytes.Repeat([]byte("x"), 5000))
	if w := post(f.fixture, io.MultiReader(big), ct); w.Code != 413 {
		t.Fatalf("want 413 got %d", w.Code)
	}
	ok("413")
	b, c := okBody(t)
	if w := post(f.fixture, failingReader{io.LimitReader(b, int64(b.Len()-100))}, c); w.Code != 400 {
		t.Fatalf("want 400 got %d", w.Code)
	}
	ok("abort")
	panicNext = true
	b, c = okBody(t)
	if w := post(f.fixture, b, c); w.Code != 500 {
		t.Fatalf("want 500 got %d", w.Code)
	}
	panicNext = false
	ok("panic")
}

func TestUploadQueueCap(t *testing.T) {
	f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) { c.MaxQueuedJobs = 2 })
	for i := 0; i < 2; i++ {
		b, c := okBody(t)
		if w := post(f.fixture, b, c); w.Code != 202 {
			t.Fatalf("job %d: %d", i, w.Code)
		}
	}
	b, c := okBody(t)
	w := post(f.fixture, b, c)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("want 503+Retry-After got %d %v", w.Code, w.Header())
	}
	problemOf(t, w)
	if len(inFiles(t, f.fixture)) != 2 {
		t.Fatalf("rejected upload wrote bytes: %v", inFiles(t, f.fixture))
	}
	if !hasRejectAudit(f, 503, "queue full") {
		t.Fatalf("missing reject audit: %s", f.log)
	}
}

func TestUploadInsufficientDiskSpace(t *testing.T) {
	free := uint64(100)
	f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) {
		c.MinFreeBytes = 1000
		d.FreeBytes = func(string) (uint64, error) { return free, nil }
	})
	b, c := okBody(t)
	w := post(f.fixture, b, c)
	if w.Code != 507 {
		t.Fatalf("want 507 got %d", w.Code)
	}
	problemOf(t, w)
	if len(inFiles(t, f.fixture)) != 0 || strings.Contains(w.Body.String(), f.dir) {
		t.Fatal("bytes written or path leaked")
	}
	if !hasRejectAudit(f, 507, "insufficient storage") {
		t.Fatalf("missing reject audit: %s", f.log)
	}
	free = 1 << 20
	b, c = okBody(t)
	if w := post(f.fixture, b, c); w.Code != 202 {
		t.Fatalf("enough space: %d", w.Code)
	}
}

type failWriter struct {
	f   *os.File
	err error
	n   int
}

func (w *failWriter) Write(p []byte) (int, error) {
	if w.n += len(p); w.n > 10 {
		return 0, w.err
	}
	return w.f.Write(p)
}
func (w *failWriter) Close() error { return w.f.Close() }

func TestUploadDiskFullAndWriteErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"ENOSPC", &os.PathError{Op: "write", Path: "/secret/data/in/x", Err: syscall.ENOSPC}, 507},
		{"EIO", &os.PathError{Op: "write", Path: "/secret/data/in/x", Err: syscall.EIO}, 500},
	}
	for _, tc := range cases {
		f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) {
			d.OpenInput = func(p string) (io.WriteCloser, error) {
				file, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				return &failWriter{f: file, err: tc.err}, err
			}
		})
		body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte("x"), 5000))
		w := post(f.fixture, body, ct)
		if w.Code != tc.want {
			t.Fatalf("%s: want %d got %d: %s", tc.name, tc.want, w.Code, w.Body)
		}
		problemOf(t, w)
		if strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("%s: path leaked: %s", tc.name, w.Body)
		}
		if len(inFiles(t, f.fixture)) != 0 {
			t.Fatalf("%s: partial file left: %v", tc.name, inFiles(t, f.fixture))
		}
		if l, _ := f.store.List(context.Background(), 10); len(l) != 0 {
			t.Fatalf("%s: job row created", tc.name)
		}
	}
}

func TestUploadOpenFileFailureIs500(t *testing.T) {
	f := newFixtureWith(t, 1<<20, func(c *config.Config, d *api.Deps) {
		d.OpenInput = func(string) (io.WriteCloser, error) { return nil, errors.New("open /secret/dir: denied") }
	})
	b, c := okBody(t)
	w := post(f.fixture, b, c)
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	problemOf(t, w)
}

func TestUploadStrictFieldParsing(t *testing.T) {
	cases := map[string][]field{
		"unknown field":      {{"preset", "audio-wav"}, {"surprise", "1"}},
		"duplicate preset":   {{"preset", "audio-wav"}, {"preset", "audio-flac"}},
		"preset and options": {{"preset", "audio-wav"}, {"options", `{"preset":"audio-flac"}`}},
		"duplicate options":  {{"options", `{"preset":"audio-wav"}`}, {"options", `{"preset":"audio-wav"}`}},
		"trailing json":      {{"options", `{"preset":"audio-wav"}{"preset":"audio-flac"}`}},
		"trailing garbage":   {{"options", `{"preset":"audio-wav"} xyz`}},
	}
	for name, fields := range cases {
		f := newFixtureWith(t, 1<<20, func(*config.Config, *api.Deps) {})
		body, ct := multipartBody(t, fields, "a.wav", []byte("data"))
		w := post(f.fixture, body, ct)
		if w.Code != 400 && w.Code != 422 {
			t.Errorf("%s: want 400/422 got %d", name, w.Code)
			continue
		}
		problemOf(t, w)
		if len(inFiles(t, f.fixture)) != 0 {
			t.Errorf("%s: bytes written", name)
		}
		if !strings.Contains(f.log.String(), `"event":"upload_rejected"`) {
			t.Errorf("%s: no reject audit", name)
		}
	}
}

func TestUploadRejectionsAreAudited(t *testing.T) {
	f := newFixtureWith(t, 1000, func(*config.Config, *api.Deps) {})
	big, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "big.wav", bytes.Repeat([]byte("x"), 200_000))
	post(f.fixture, io.MultiReader(big), ct)
	b, c := multipartBody(t, []field{{"preset", "nope"}}, "a.wav", []byte("SECRETDATA"))
	post(f.fixture, b, c)
	if !hasRejectAudit(f, 413, "too large") || !hasRejectAudit(f, 422, "invalid options") || strings.Contains(f.log.String(), "SECRETDATA") {
		t.Fatalf("audit: %s", f.log)
	}
}
