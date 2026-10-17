// Package e2e_test wires the real stack in one process: real Postgres, the real API handler served
// by httptest, real worker.Run/ProcessOne/Watch loops and real FFmpeg/ffprobe processes.
//
// Running:
//
//	scripts/test-db.sh ./internal/e2e/ -race -count=1            # default suite (about 2 minutes)
//	scripts/test-db.sh ./internal/e2e/ -race -count=1 -short     # skips the slowest cases
//
// Opt-in: run the same tests against the FFmpeg/ffprobe 7.1.1 binaries shipped in a worker image
// (the binaries are dynamically linked, so the image root is exported and run through its own
// dynamic loader; no container is left running):
//
//	CONVERTFLOW_E2E_FFMPEG_IMAGE=localhost/convertflow-worker:dev-lgpl CONTAINER_RUNTIME=podman \
//	  scripts/test-db.sh ./internal/e2e/ -race -count=1
//
// Without TEST_DATABASE_URL, ffmpeg or ffprobe, every test skips.
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/testutil"
	"github.com/ShifuRain/convertflow/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Binaries the workers under test run. Host ffmpeg/ffprobe by default; wrapper scripts around
// the image's loader when CONVERTFLOW_E2E_FFMPEG_IMAGE is set.
var (
	workerFFmpeg  = "ffmpeg"
	workerFFprobe = "ffprobe"
	imageMode     string // image reference, empty in host mode
)

func TestMain(m *testing.M) {
	cleanup := func() {}
	if img := os.Getenv("CONVERTFLOW_E2E_FFMPEG_IMAGE"); img != "" {
		ffb, fpb, c, err := extractImageBins(img)
		if err != nil {
			fmt.Fprintln(os.Stderr, "e2e: cannot use image ffmpeg:", err)
			os.Exit(2)
		}
		workerFFmpeg, workerFFprobe, cleanup, imageMode = ffb, fpb, c, img
		out, _ := exec.Command(ffb, "-version").Output()
		fmt.Fprintf(os.Stderr, "e2e: worker ffmpeg from image %s: %s\n", img, strings.SplitN(string(out), "\n", 2)[0])
	}
	code := m.Run()
	if code == 0 {
		if leaks := settleGoroutines(8 * time.Second); len(leaks) > 0 {
			fmt.Fprintf(os.Stderr, "e2e: goroutines of the system under test outlived the suite:\n%s\n", strings.Join(leaks, "\n\n"))
			code = 1
		}
	}
	cleanup()
	os.Exit(code)
}

// extractImageBins exports the image root file system into a temp dir and returns wrapper scripts
// that run the image's ffmpeg/ffprobe through the image's dynamic loader and libraries.
func extractImageBins(image string) (ffmpegBin, ffprobeBin string, cleanup func(), err error) {
	rt := os.Getenv("CONTAINER_RUNTIME")
	if rt == "" {
		rt = "docker"
	}
	root, err := os.MkdirTemp("", "cf-e2e-image-")
	if err != nil {
		return "", "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(root) }
	name := fmt.Sprintf("cf-e2e-export-%d", os.Getpid())
	if out, err := exec.Command(rt, "create", "--name", name, image, "true").CombinedOutput(); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("%s create: %v: %s", rt, err, out)
	}
	defer func() { _ = exec.Command(rt, "rm", "-f", name).Run() }()
	exp := exec.Command(rt, "export", name)
	tarCmd := exec.Command("tar", "-x", "-C", root, "--exclude=./dev", "--exclude=./proc", "--exclude=./sys")
	pipe, err := exp.StdoutPipe()
	if err != nil {
		cleanup()
		return "", "", nil, err
	}
	tarCmd.Stdin = pipe
	if err := tarCmd.Start(); err != nil {
		cleanup()
		return "", "", nil, err
	}
	if err := exp.Run(); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("%s export: %v", rt, err)
	}
	if err := tarCmd.Wait(); err != nil {
		cleanup()
		return "", "", nil, fmt.Errorf("tar: %v", err)
	}
	ld := filepath.Join(root, "usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2")
	libs := filepath.Join(root, "usr/lib/x86_64-linux-gnu") + ":" + filepath.Join(root, "usr/local/lib")
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		script := fmt.Sprintf("#!/bin/sh\nexec %q --library-path %q %q \"$@\"\n", ld, libs, filepath.Join(root, "usr/local/bin", bin))
		p := filepath.Join(root, "run-"+bin)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil { // #nosec G306 -- test wrapper
			cleanup()
			return "", "", nil, err
		}
	}
	return filepath.Join(root, "run-ffmpeg"), filepath.Join(root, "run-ffprobe"), cleanup, nil
}

// ---------------------------------------------------------------------------------------------
// Goroutine leak check (local, dependency free).

var sutFrames = regexp.MustCompile(`convertflow/internal/(api|worker|ffmpeg|janitor|jobs|audit)\.`)

// sutGoroutines returns the stacks of goroutines currently executing code of the system under test.
func sutGoroutines() []string {
	var b bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&b, 2)
	var out []string
	for _, g := range strings.Split(b.String(), "\n\n") {
		if sutFrames.MatchString(g) {
			out = append(out, strings.TrimSpace(g))
		}
	}
	return out
}

// settleGoroutines waits until no goroutine of the system under test remains, or returns the
// survivors after the timeout.
func settleGoroutines(timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		leaks := sutGoroutines()
		if len(leaks) == 0 || time.Now().After(deadline) {
			return leaks
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------------------------
// Stack

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

var defaultLimits = ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 8192 * 4320, MaxStreams: 16}

// opts configures a stack. The zero value is one fast worker with default limits.
type opts struct {
	workers        int  // number of workers started (default 1)
	noWorkers      bool // start none
	limits         *ffmpeg.Limits
	maxUpload      int64 // default 50 MiB
	maxUploads     int   // concurrent upload slots (default 16)
	slow           bool  // run ffmpeg in real time (-re) so jobs last as long as their media
	hide           string
	jobTimeout     time.Duration
	maxOutputBytes int64
	maxQueued      int // MaxQueuedJobs cap of the API (0 = unlimited)
}

type workerHandle struct {
	W       *worker.Worker
	cancel  context.CancelFunc
	done    chan struct{}
	scratch string
}

// stop cancels the worker (as a shutdown/crash would) and waits for Run to return.
func (h *workerHandle) stop() {
	h.cancel()
	<-h.done
}

type stack struct {
	t      *testing.T
	base   string
	dir    string
	pool   *pgxpool.Pool
	store  *jobs.Store
	client *http.Client
	logs   *syncBuf // audit records of API and workers plus slog output of everything
	limits ffmpeg.Limits
	o      opts

	srv      *httptest.Server
	workers  []*workerHandle
	prevLog  *slog.Logger
	downOnce sync.Once
}

func start(t *testing.T, o opts) *stack {
	t.Helper()
	testutil.RequireTools(t)
	pool := dbtest.New(t)
	root := t.TempDir()
	dir := filepath.Join(root, "data") // the parent (root) is ours too, so escapes from data/ are visible
	for _, sub := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s := &stack{t: t, dir: dir, pool: pool, store: jobs.NewStore(pool), logs: &syncBuf{}, o: o, limits: defaultLimits}
	if o.limits != nil {
		s.limits = *o.limits
	}
	s.prevLog = slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { s.shutdown() })

	maxUpload := o.maxUpload
	if maxUpload == 0 {
		maxUpload = 50 << 20
	}
	maxUploads := o.maxUploads
	if maxUploads == 0 {
		maxUploads = 16
	}
	h := api.NewHandler(api.Deps{
		Store: s.store, Audit: audit.New(s.logs), EventInterval: 50 * time.Millisecond, SSEHeartbeat: time.Second,
		Cfg: config.Config{DataDir: dir, Flavor: "full", MaxUploadBytes: maxUpload, MaxConcurrentUploads: maxUploads, MaxQueuedJobs: o.maxQueued},
	})
	s.srv = httptest.NewServer(h)
	s.base = s.srv.URL
	s.client = &http.Client{Transport: &http.Transport{}, Timeout: 120 * time.Second}

	if !o.noWorkers {
		n := o.workers
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			s.addWorker(fmt.Sprintf("e2e-%d", i+1), workerOpts{slow: o.slow, hide: o.hide})
		}
		s.waitWorkers(len(s.workers))
	}
	return s
}

// shutdown stops workers and the server and closes every connection. It is idempotent.
func (s *stack) shutdown() {
	s.downOnce.Do(func() {
		for _, w := range s.workers {
			w.stop()
		}
		s.client.CloseIdleConnections()
		s.srv.Close()
		s.pool.Close()
		slog.SetDefault(s.prevLog)
	})
}

type workerOpts struct {
	slow bool
	hide string // extra regexp alternation of encoder names to hide from `ffmpeg -encoders`
}

// hardware encoders are always hidden so results do not depend on the host's GPUs.
const hideHardware = "av1_nvenc|av1_qsv|av1_vaapi"

// wrapBins returns ffmpeg/ffprobe paths for a worker. The ffmpeg wrapper can hide encoders from
// capability detection (simulating a build without them) and run encodes in real time.
func wrapBins(t *testing.T, wo workerOpts) (string, string) {
	t.Helper()
	hide := hideHardware
	if wo.hide != "" {
		hide += "|" + wo.hide
	}
	slow := 0
	if wo.slow {
		slow = 1
	}
	script := fmt.Sprintf(`#!/bin/sh
REAL=%q
HIDE=%q
SLOW=%d
RUN=
for a in "$@"; do
  case "$a" in
    -encoders) "$REAL" "$@" | grep -Ev "$HIDE"; exit 0 ;;
    -progress) RUN=1 ;;
  esac
done
if [ -n "$RUN" ] && [ "$SLOW" = 1 ]; then exec "$REAL" -re "$@"; fi
exec "$REAL" "$@"
`, resolveBin(t, workerFFmpeg), " ("+hide+") ", slow)
	p := filepath.Join(t.TempDir(), "ffmpeg-wrapper")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil { // #nosec G306 -- test wrapper
		t.Fatal(err)
	}
	return p, resolveBin(t, workerFFprobe)
}

func resolveBin(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available", name)
	}
	abs, _ := filepath.Abs(p)
	return abs
}

// addWorker starts a real worker.Run loop. Its capability detection is the real one.
func (s *stack) addWorker(id string, wo workerOpts) *workerHandle {
	s.t.Helper()
	ffb, fpb := wrapBins(s.t, wo)
	scratch := s.t.TempDir()
	w := &worker.Worker{
		ID: id, Store: s.store, Audit: audit.New(s.logs), FFmpegBin: ffb, FFprobeBin: fpb, Flavor: "full",
		OutDir: filepath.Join(s.dir, "out"), ScratchDir: scratch, Limits: s.limits,
		Poll: 50 * time.Millisecond, HeartbeatEvery: 100 * time.Millisecond,
		JobTimeout: s.o.jobTimeout, MaxOutputBytes: s.o.maxOutputBytes,
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &workerHandle{W: w, cancel: cancel, done: make(chan struct{}), scratch: scratch}
	go func() { defer close(h.done); _ = w.Run(ctx) }()
	s.workers = append(s.workers, h)
	return h
}

// waitWorkers blocks until n workers have registered their capabilities.
func (s *stack) waitWorkers(n int) {
	s.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := s.store.LiveWorkers(context.Background(), time.Minute); err == nil && c >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("%d workers did not register in time", n)
}

// ---------------------------------------------------------------------------------------------
// HTTP helpers

type resp struct {
	Code int
	Body map[string]any
	Raw  []byte
	Head http.Header
}

func decode(raw []byte) map[string]any {
	var v map[string]any
	_ = json.Unmarshal(raw, &v)
	return v
}

func quoteEscape(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }

// multipartBody builds an upload body. The file name is placed into the header byte for byte
// (only quote and backslash are escaped, exactly like mime/multipart), so hostile names such as
// NUL or CRLF reach the server unmodified.
func multipartBody(preset, filename string, content []byte) (string, *bytes.Buffer) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if preset != "" {
		_ = mw.WriteField("preset", preset)
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="file"; filename="`+quoteEscape(filename)+`"`)
	h.Set("Content-Type", "application/octet-stream")
	fw, _ := mw.CreatePart(h)
	_, _ = fw.Write(content)
	_ = mw.Close()
	return mw.FormDataContentType(), &buf
}

func (s *stack) do(req *http.Request) resp {
	s.t.Helper()
	r, err := s.client.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, decode(raw), raw, r.Header}
}

func (s *stack) get(path string) resp {
	s.t.Helper()
	req, _ := http.NewRequest("GET", s.base+path, nil)
	return s.do(req)
}

func (s *stack) del(id string) resp {
	s.t.Helper()
	req, _ := http.NewRequest("DELETE", s.base+"/api/v1/jobs/"+id, nil)
	return s.do(req)
}

// upload posts a file through the real API and returns status code and decoded JSON.
func (s *stack) upload(preset, name string, content []byte) (int, map[string]any) {
	s.t.Helper()
	ct, body := multipartBody(preset, name, content)
	req, _ := http.NewRequest("POST", s.base+"/api/v1/jobs", body)
	req.Header.Set("Content-Type", ct)
	r := s.do(req)
	return r.Code, r.Body
}

// mustUpload uploads and requires 202; it returns the job id.
func (s *stack) mustUpload(preset, name string, content []byte) string {
	s.t.Helper()
	code, v := s.upload(preset, name, content)
	if code != http.StatusAccepted {
		s.t.Fatalf("upload %q: status %d %v", name, code, v)
	}
	return v["id"].(string)
}

func isTerminal(st string) bool { return st == "done" || st == "failed" || st == "canceled" }

// waitStatus polls until the job reaches one of the statuses (or any terminal one when none given).
func (s *stack) waitStatus(id string, timeout time.Duration, want ...string) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		r := s.get("/api/v1/jobs/" + id)
		last = r.Body
		st, _ := last["status"].(string)
		if len(want) == 0 && isTerminal(st) {
			return last
		}
		for _, w := range want {
			if st == w {
				return last
			}
		}
		if len(want) > 0 && isTerminal(st) {
			s.t.Fatalf("job %s ended as %v while waiting for %v: %v", id, st, want, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("job %s: timeout waiting for %v, last state %v", id, want, last)
	return nil
}

func (s *stack) wait(id string) map[string]any { s.t.Helper(); return s.waitStatus(id, 90*time.Second) }

func (s *stack) download(v map[string]any) (resp, string) {
	s.t.Helper()
	u, _ := v["output_url"].(string)
	if u == "" {
		s.t.Fatalf("job has no output_url: %v", v)
	}
	r := s.get(u)
	path := filepath.Join(s.t.TempDir(), "download")
	if r.Code == http.StatusOK {
		if err := os.WriteFile(path, r.Raw, 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	return r, path
}

// ---------------------------------------------------------------------------------------------
// File system helpers

func (s *stack) names(sub string) []string {
	s.t.Helper()
	ents, err := os.ReadDir(filepath.Join(s.dir, sub))
	if err != nil {
		s.t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// waitDirEmpty waits for a data sub directory to become empty (inputs are removed just after the
// terminal state is written).
func (s *stack) waitDirEmpty(sub string) {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := s.names(sub)
		if len(n) == 0 {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("data/%s is not empty: %v", sub, n)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (s *stack) jobCount() int {
	s.t.Helper()
	r := s.get("/api/v1/jobs")
	var arr []any
	_ = json.Unmarshal(r.Raw, &arr)
	return len(arr)
}

// assertNoLeak fails when any of the secrets (or this stack's directories) appear in text.
func (s *stack) assertNoLeak(what, text string, secrets ...string) {
	s.t.Helper()
	all := append([]string{s.dir, filepath.Dir(s.dir), "/etc/passwd"}, secrets...)
	for _, h := range s.workers {
		all = append(all, h.scratch)
	}
	for _, sec := range all {
		if sec != "" && strings.Contains(text, sec) {
			s.t.Errorf("%s leaks %q: %s", what, sec, text)
		}
	}
}

// ---------------------------------------------------------------------------------------------
// Media helpers (host FFmpeg; they only create inputs and inspect outputs)

func ff(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// clip makes a media file in a fresh temp dir and returns its bytes. kind: av, audio, video.
func clip(t *testing.T, kind string) []byte {
	t.Helper()
	ext := map[string]string{"av": "mkv", "audio": "wav", "video": "mkv"}[kind]
	return readFile(t, testutil.MakeClip(t, t.TempDir(), "c."+ext, kind))
}

// tone makes a wav with a distinct frequency and duration.
func tone(t *testing.T, freq int, dur float64) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tone.wav")
	ff(t, "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:duration=%.3f", freq, dur), "-c:a", "pcm_s16le", p)
	return readFile(t, p)
}

type probed struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
	} `json:"streams"`
}

func probe(t *testing.T, path string) probed {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p probed
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p probed) count(kind string) int {
	n := 0
	for _, s := range p.Streams {
		if s.CodecType == kind {
			n++
		}
	}
	return n
}

// pcmMD5 returns the md5 of the decoded first audio stream as s16le PCM, a content fingerprint
// that is identical for a lossless conversion.
func pcmMD5(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", "0:a:0", "-c:a", "pcm_s16le", "-f", "md5", "-").Output()
	if err != nil {
		t.Fatalf("md5 %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

var pathLike = regexp.MustCompile(`/[A-Za-z0-9._-]+/`)
