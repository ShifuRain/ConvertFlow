package main

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/blob/s3test"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/testutil"
	"github.com/ShifuRain/convertflow/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerModeFlagsOverrideEnv(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		args []string
		want worker.Mode
		err  bool
	}{
		{"default always-on", config.Config{}, nil, worker.Mode{}, false},
		{"once flag defaults idle", config.Config{}, []string{"--once"}, worker.Mode{Once: true, IdleExit: 30 * time.Second}, false},
		{"once env", config.Config{WorkerOnce: true}, nil, worker.Mode{Once: true, IdleExit: 30 * time.Second}, false},
		{"once env idle env", config.Config{WorkerOnce: true, WorkerIdleExit: time.Minute}, nil, worker.Mode{Once: true, IdleExit: time.Minute}, false},
		{"flag beats env", config.Config{WorkerOnce: true, WorkerIdleExit: time.Minute}, []string{"--once=false", "--idle-exit=5s"}, worker.Mode{IdleExit: 5 * time.Second}, false},
		{"idle only", config.Config{}, []string{"-idle-exit", "2m"}, worker.Mode{IdleExit: 2 * time.Minute}, false},
		{"idle zero", config.Config{}, []string{"--idle-exit=0s"}, worker.Mode{}, true},
		{"idle negative", config.Config{}, []string{"--idle-exit=-1s"}, worker.Mode{}, true},
		{"unknown flag", config.Config{}, []string{"--twice"}, worker.Mode{}, true},
		{"positional", config.Config{}, []string{"extra"}, worker.Mode{}, true},
	}
	for _, c := range cases {
		got, err := workerMode(c.cfg, c.args, &bytes.Buffer{})
		if (err != nil) != c.err || (err == nil && got != c.want) {
			t.Errorf("%s: got %+v %v, want %+v err=%v", c.name, got, err, c.want, c.err)
		}
	}
}

func TestExitCodes(t *testing.T) {
	once := worker.Mode{Once: true, IdleExit: time.Second}
	idle := worker.Mode{IdleExit: time.Second}
	cases := []struct {
		name string
		m    worker.Mode
		s    worker.Summary
		err  error
		want int
	}{
		{"fatal", once, worker.Summary{}, errors.New("db down"), exitFatal},
		{"once finished", once, worker.Summary{Jobs: 1, Finished: true}, nil, exitOK},
		{"once idle", once, worker.Summary{IdleExit: true}, nil, exitOK},
		{"once interrupted", once, worker.Summary{Jobs: 1, Interrupted: true}, nil, exitTempFail},
		{"once released (storage)", once, worker.Summary{Jobs: 1}, nil, exitTempFail},
		{"once signal while idle", once, worker.Summary{}, nil, exitOK},
		{"idle loop done", idle, worker.Summary{Jobs: 3, Finished: true, IdleExit: true}, nil, exitOK},
		{"idle loop interrupted", idle, worker.Summary{Jobs: 2, Interrupted: true}, nil, exitTempFail},
		{"always-on interrupted", worker.Mode{}, worker.Summary{Jobs: 2, Interrupted: true}, nil, exitOK},
		{"always-on fatal", worker.Mode{}, worker.Summary{}, errors.New("x"), exitFatal},
	}
	for _, c := range cases {
		if got := exitCode(c.m, c.s, c.err); got != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, got, c.want)
		}
	}
}

// ---- the real binary ----

var workerBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cf-worker-bin-")
	if err != nil {
		panic(err)
	}
	workerBin = filepath.Join(dir, "convertflow-worker")
	args := []string{"build", "-o", workerBin}
	if raceEnabled {
		args = append(args, "-race")
	}
	if out, err := exec.Command("go", append(args, ".")...).CombinedOutput(); err != nil {
		os.Stderr.Write(out)
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type binEnv struct {
	pool    *pgxpool.Pool
	store   *jobs.Store
	dbURL   string
	dataDir string
	tmp     string // TMPDIR of the worker processes: their scratch dirs live here
}

func newBinEnv(t *testing.T) binEnv {
	t.Helper()
	testutil.RequireTools(t)
	pool := dbtest.New(t)
	var schema string
	if err := pool.QueryRow(context.Background(), `SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	e := binEnv{pool: pool, store: jobs.NewStore(pool), dbURL: u.String(), dataDir: t.TempDir(), tmp: t.TempDir()}
	for _, d := range []string{"in", "out"} {
		_ = os.MkdirAll(filepath.Join(e.dataDir, d), 0o700)
	}
	return e
}

func (e binEnv) cmd(t *testing.T, extra map[string]string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	c := exec.Command(workerBin, args...)
	env := map[string]string{
		"PATH": os.Getenv("PATH"), "TMPDIR": e.tmp,
		"CONVERTFLOW_DATABASE_URL": e.dbURL, "CONVERTFLOW_DATA_DIR": e.dataDir,
		"CONVERTFLOW_FLAVOR": "full", "CONVERTFLOW_WORKER_POLL": "200ms",
	}
	for k, v := range extra {
		env[k] = v
	}
	for k, v := range env {
		c.Env = append(c.Env, k+"="+v)
	}
	var out bytes.Buffer
	c.Stdout, c.Stderr = &out, &out
	return c, &out
}

func exitStatus(t *testing.T, err error) int {
	t.Helper()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0
}

func (e binEnv) run(t *testing.T, extra map[string]string, args ...string) (int, string) {
	t.Helper()
	c, out := e.cmd(t, extra, args...)
	return exitStatus(t, c.Run()), out.String()
}

// tmpEmpty asserts that no worker left its scratch directory (or anything else) in TMPDIR.
func (e binEnv) tmpEmpty(t *testing.T) {
	t.Helper()
	ents, _ := os.ReadDir(e.tmp)
	if len(ents) != 0 {
		t.Fatalf("scratch leaked: %d entries in TMPDIR, first %q", len(ents), ents[0].Name())
	}
}

func (e binEnv) enqueueLocal(t *testing.T, src, preset string) string {
	t.Helper()
	id := uuid.NewString()
	in := filepath.Join(e.dataDir, "in", id)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Create(context.Background(), &jobs.Job{ID: id, Filename: "x", InputPath: in, Options: jobs.Options{Preset: preset}}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBinaryConfigErrorExits1(t *testing.T) {
	e := binEnv{tmp: t.TempDir(), dataDir: t.TempDir()}
	if code, out := e.run(t, map[string]string{"CONVERTFLOW_DATABASE_URL": "", "CONVERTFLOW_WORKER_IDLE_EXIT": "soon"}, "--once"); code != exitFatal {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	if code, out := e.run(t, map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}, "--idle-exit=0s"); code != exitFatal {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	e.tmpEmpty(t)
}

func TestBinaryDatabaseUnreachableExits1(t *testing.T) {
	e := binEnv{tmp: t.TempDir(), dataDir: t.TempDir()}
	code, out := e.run(t, map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2"}, "--once")
	if code != exitFatal {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	if strings.Contains(out, "u:p@") {
		t.Fatalf("credentials logged: %s", out)
	}
	e.tmpEmpty(t)
}

func TestBinaryOnceIdleExitsZero(t *testing.T) {
	e := newBinEnv(t)
	start := time.Now()
	code, out := e.run(t, nil, "--once", "--idle-exit=1s")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("idle exit took %s", took)
	}
	// The once worker registered its encoders before claiming (on-demand capability source).
	if ws, _ := e.store.LiveWorkerEncoders(context.Background(), time.Hour); len(ws) == 0 {
		t.Fatal("once worker did not register its encoders")
	}
	e.tmpEmpty(t)
}

// Real FFmpeg through the binary in once mode against the local store; a second queued job is
// left alone.
func TestBinaryOnceConvertsOneJob(t *testing.T) {
	e := newBinEnv(t)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueueLocal(t, src, "audio-flac")
	other := e.enqueueLocal(t, src, "audio-flac")
	code, out := e.run(t, map[string]string{"CONVERTFLOW_WORKER_ONCE": "true"})
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	ctx := context.Background()
	j, _ := e.store.Get(ctx, id)
	if j.Status != jobs.StatusDone {
		t.Fatalf("job %+v\n%s", j, out)
	}
	if fi, err := os.Stat(j.OutputPath); err != nil || fi.Size() == 0 {
		t.Fatalf("output: %v", err)
	}
	if o, _ := e.store.Get(ctx, other); o.Status != jobs.StatusQueued {
		t.Fatalf("second job touched: %+v", o)
	}
	e.tmpEmpty(t)
}

// SIGTERM mid-job: exit 75 within the shutdown bound, the job released at once (no terminal
// write, input kept, no partial output, no scratch left), and a second once worker completes it
// after the janitor's requeue pass.
func TestBinarySIGTERMReleasesJobForAnotherWorker(t *testing.T) {
	e := newBinEnv(t)
	long := filepath.Join(t.TempDir(), "long.mkv")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
		"testsrc=duration=30:size=1280x720:rate=25", "-c:v", "mpeg4", long).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	id := e.enqueueLocal(t, long, "video-vp9")
	c, out := e.cmd(t, map[string]string{"CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT": "5s"}, "--once")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for {
		if j, _ := e.store.Get(ctx, id); j != nil && j.Status == jobs.StatusRunning && j.Progress > 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatalf("job never ran\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	sent := time.Now()
	_ = c.Process.Signal(syscall.SIGTERM)
	code := exitStatus(t, c.Wait())
	took := time.Since(sent)
	t.Logf("SIGTERM to exit: %s", took)
	if took > 5*time.Second+hardStopSlack {
		t.Fatalf("shutdown took %s", took)
	}
	if code != exitTempFail {
		t.Fatalf("exit %d, want 75\n%s", code, out)
	}
	j, _ := e.store.Get(ctx, id)
	var released bool
	_ = e.pool.QueryRow(ctx, `SELECT heartbeat_at = '-infinity' FROM jobs WHERE id = $1::uuid`, id).Scan(&released)
	if j.Status != jobs.StatusRunning || j.Error != "" || !released {
		t.Fatalf("job not released: %+v released=%v\n%s", j, released, out)
	}
	if _, err := os.Stat(j.InputPath); err != nil {
		t.Fatal("input must be kept")
	}
	if ents, _ := os.ReadDir(filepath.Join(e.dataDir, "out")); len(ents) != 0 {
		t.Fatalf("partial output left: %v", ents)
	}
	e.tmpEmpty(t)

	if rq, _, err := e.store.RequeueStale(ctx, time.Hour, 3); err != nil || rq != 1 {
		t.Fatalf("requeue %d %v", rq, err)
	}
	if code, out := e.run(t, nil, "--once", "--idle-exit=5s"); code != exitOK {
		t.Fatalf("second worker exit %d\n%s", code, out)
	}
	j, _ = e.store.Get(ctx, id)
	if j.Status != jobs.StatusDone || j.Attempts != 2 {
		t.Fatalf("job %+v", j)
	}
	e.tmpEmpty(t)
}

// Once mode through a real bucket (gated like the other MinIO tests).
func TestBinaryOnceWithS3(t *testing.T) {
	s3e := s3test.New(t)
	e := newBinEnv(t)
	s := s3e.Store(t)
	ctx := context.Background()
	id := uuid.NewString()
	w, loc, err := s.CreateInput(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(testutil.MakeClip(t, t.TempDir(), "a.wav", "audio"))
	_, _ = w.Write(b)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Create(ctx, &jobs.Job{ID: id, Filename: "x", InputPath: loc, Options: jobs.Options{Preset: "audio-flac"}}); err != nil {
		t.Fatal(err)
	}
	code, out := e.run(t, map[string]string{
		"CONVERTFLOW_STORAGE":                 "s3",
		"CONVERTFLOW_S3_ENDPOINT":             s3e.Opts.Endpoint,
		"CONVERTFLOW_S3_BUCKET":               s3e.Opts.Bucket,
		"CONVERTFLOW_S3_USE_SSL":              strconv.FormatBool(s3e.Opts.UseSSL),
		"CONVERTFLOW_S3_ACCESS_KEY_FILE":      os.Getenv("CONVERTFLOW_TEST_S3_ACCESS_KEY_FILE"),
		"CONVERTFLOW_S3_SECRET_KEY_FILE":      os.Getenv("CONVERTFLOW_TEST_S3_SECRET_KEY_FILE"),
		"CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT": "5s",
	}, "--once", "--idle-exit=5s")
	if code != exitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	j, _ := e.store.Get(ctx, id)
	if j.Status != jobs.StatusDone || j.OutputPath != "out/"+id+".1.flac" {
		t.Fatalf("job %+v\n%s", j, out)
	}
	if _, err := s.StatInput(ctx, loc); err == nil {
		t.Fatal("input must be removed after completion")
	}
	e.tmpEmpty(t)
}
