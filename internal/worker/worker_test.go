package worker_test

import (
	"context"
	"github.com/ShifuRain/convertflow/internal/presets"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/testutil"
	"github.com/ShifuRain/convertflow/internal/worker"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type env struct {
	w     *worker.Worker
	store *jobs.Store
	dir   string
	pool  *pgxpool.Pool
}

func setup(t *testing.T, lim ffmpeg.Limits) env {
	t.Helper()
	testutil.RequireTools(t)
	pool := dbtest.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, "out"), 0o700)
	store := jobs.NewStore(pool)
	w := &worker.Worker{
		ID: "w1", Store: store, Audit: audit.New(&strings.Builder{}),
		FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", Flavor: "full",
		OutDir: filepath.Join(dir, "out"), ScratchDir: t.TempDir(), Limits: lim,
		Poll: 50 * time.Millisecond, HeartbeatEvery: 50 * time.Millisecond,
	}
	if err := w.RegisterCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	return env{w, store, dir, pool}
}

var defaultLimits = ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 8192 * 4320, MaxStreams: 16}

func (e env) enqueue(t *testing.T, src, preset string) string {
	t.Helper()
	id := uuid.NewString()
	in := filepath.Join(e.dir, "in", id)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(in, b, 0o600)
	if err := e.store.Create(context.Background(), &jobs.Job{ID: id, Filename: "x", InputPath: in, Options: jobs.Options{Preset: preset}}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWorkerConvertsAudio(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-flac")
	ok, err := e.w.ProcessOne(context.Background())
	if !ok || err != nil {
		t.Fatalf("ProcessOne: %v %v", ok, err)
	}
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusDone || j.EncoderUsed != "flac" || j.Progress != 100 {
		t.Fatalf("bad job %+v", j)
	}
	if st, err := os.Stat(j.OutputPath); err != nil || st.Size() == 0 {
		t.Fatalf("output missing: %v", err)
	}
	if _, err := os.Stat(j.InputPath); !os.IsNotExist(err) {
		t.Fatal("input should be removed after completion")
	}
}

func TestWorkerFailsOnGarbage(t *testing.T) {
	e := setup(t, defaultLimits)
	src := filepath.Join(t.TempDir(), "x.mp4")
	_ = os.WriteFile(src, []byte("not media"), 0o600)
	id := e.enqueue(t, src, "audio-wav")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || j.Error == "" || strings.Contains(j.Error, e.dir) {
		t.Fatalf("bad failure %+v", j)
	}
}

func TestWorkerFailsOnAudioOnlyForVideoPreset(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "video-vp9")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || !strings.Contains(j.Error, "no video stream") {
		t.Fatalf("bad failure %+v", j)
	}
}

func TestWorkerEnforcesLimits(t *testing.T) {
	e := setup(t, ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 100, MaxStreams: 16})
	src := testutil.MakeClip(t, t.TempDir(), "v.mkv", "video")
	id := e.enqueue(t, src, "video-vp9")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || !strings.Contains(j.Error, "dimensions") {
		t.Fatalf("bad failure %+v", j)
	}
}

func TestWorkerNoJob(t *testing.T) {
	e := setup(t, defaultLimits)
	ok, err := e.w.ProcessOne(context.Background())
	if ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestWatchCancelsOnRequest(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-wav")
	ctx := context.Background()
	if _, err := e.store.Claim(ctx, "w1", presets.IDs(presets.All())); err != nil {
		t.Fatal(err)
	}
	_ = e.store.RequestCancel(ctx, id)
	cctx, cancel := context.WithCancel(ctx)
	var canceled atomic.Bool
	done := make(chan struct{})
	go func() { e.w.Watch(cctx, id, cancel, &canceled); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not react to cancel request")
	}
	if !canceled.Load() || cctx.Err() == nil {
		t.Fatal("expected canceled flag and cancelled context")
	}
}

func (e env) outFiles(t *testing.T) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(e.dir, "out"))
	var n []string
	for _, x := range ents {
		n = append(n, x.Name())
	}
	return n
}

func TestWorkerFailsOnEmptyFile(t *testing.T) {
	e := setup(t, defaultLimits)
	src := filepath.Join(t.TempDir(), "x.mp4")
	_ = os.WriteFile(src, nil, 0o600)
	id := e.enqueue(t, src, "audio-wav")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || j.Error == "" || strings.Contains(j.Error, e.dir) || strings.Contains(j.Error, "/") {
		t.Fatalf("bad failure %+v", j)
	}
	if _, err := os.Stat(j.InputPath); !os.IsNotExist(err) {
		t.Fatal("input should be removed after failure")
	}
}

func TestWorkerAudioOnlyMessageExact(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "video-vp9")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Error != "input has no video stream" {
		t.Fatalf("got %q", j.Error)
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatal("no output expected")
	}
}

func TestWorkerCoverArtMP3ToAudioPreset(t *testing.T) {
	e := setup(t, defaultLimits)
	d := t.TempDir()
	mp3 := filepath.Join(d, "c.mp3")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x64:rate=1",
		"-map", "0:a", "-map", "1:v", "-frames:v", "1", "-c:a", "libmp3lame", "-c:v", "mjpeg",
		"-disposition:v", "attached_pic", "-id3v2_version", "3", mp3)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build cover-art mp3: %v %s", err, b)
	}
	id := e.enqueue(t, mp3, "audio-flac")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusDone {
		t.Fatalf("bad job %+v", j)
	}
	if st, err := os.Stat(j.OutputPath); err != nil || st.Size() == 0 {
		t.Fatalf("output missing: %v", err)
	}
}

func TestWorkerTimeoutFailsJob(t *testing.T) {
	e := setup(t, defaultLimits)
	e.w.JobTimeout = time.Millisecond
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-wav")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || j.Error != "conversion timed out" {
		t.Fatalf("bad job %+v", j)
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatal("partial output must be removed")
	}
}

func TestWorkerOutputCapFailsJob(t *testing.T) {
	e := setup(t, defaultLimits)
	e.w.MaxOutputBytes = 1000
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-wav")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || j.Error != "output exceeds size limit" {
		t.Fatalf("bad job %+v", j)
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatal("oversize output must be removed")
	}
}

func TestWorkerUnresolvablePresetFails(t *testing.T) {
	e := setup(t, defaultLimits)
	e.w.Flavor = "lgpl" // libx264/aac are not allowed under lgpl
	if err := e.w.RegisterCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	src := testutil.MakeClip(t, t.TempDir(), "v.mkv", "av")
	id := e.enqueue(t, src, "video-h264")
	// Capability-aware claiming keeps the job queued; the worker-side failure is defence in depth.
	if ok, _ := e.w.ProcessOne(context.Background()); ok {
		t.Fatal("worker claimed a job it cannot encode")
	}
	if j, _ := e.store.Get(context.Background(), id); j.Status != jobs.StatusQueued {
		t.Fatalf("job should stay queued, got %s", j.Status)
	}
	claimed, err := e.store.Claim(context.Background(), "w1", presets.IDs(presets.All()))
	if err != nil || claimed == nil {
		t.Fatal(err)
	}
	e.w.ProcessClaimed(context.Background(), claimed)
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || !strings.Contains(j.Error, "no encoder available") {
		t.Fatalf("bad job %+v", j)
	}
}

func TestWorkerAbortsQuietlyWhenLeaseLost(t *testing.T) {
	e := setup(t, defaultLimits)
	d := t.TempDir()
	long := filepath.Join(d, "long.mkv")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
		"testsrc=duration=60:size=1280x720:rate=25", "-c:v", "mpeg4", long).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	id := e.enqueue(t, long, "video-vp9")
	ctx := context.Background()
	go func() {
		for i := 0; i < 600; i++ {
			j, err := e.store.Get(ctx, id)
			if err == nil && j.Status == jobs.StatusRunning {
				// simulate the janitor requeueing the job after a missed lease
				_, _ = e.pool.Exec(ctx, `UPDATE jobs SET status='queued', worker_id='' WHERE id=$1::uuid`, id)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	start := time.Now()
	ok, err := e.w.ProcessOne(ctx)
	if !ok || err != nil {
		t.Fatalf("ProcessOne: %v %v", ok, err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("worker did not stop ffmpeg after losing the lease")
	}
	j, _ := e.store.Get(ctx, id)
	if j.Status != jobs.StatusQueued || j.Error != "" {
		t.Fatalf("job must be left alone, got %+v", j)
	}
	if _, err := os.Stat(j.InputPath); err != nil {
		t.Fatal("input must be kept for the requeued job")
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatal("partial output must be removed")
	}
}

func TestWatchStopsWhenLeaseLost(t *testing.T) {
	e := setup(t, defaultLimits)
	ctx := context.Background()
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-wav")
	if _, err := e.store.Claim(ctx, "w1", presets.IDs(presets.All())); err != nil {
		t.Fatal(err)
	}
	_ = e.store.Fail(ctx, id, "w1", "x")
	cctx, cancel := context.WithCancel(ctx)
	var canceled atomic.Bool
	done := make(chan struct{})
	go func() { e.w.Watch(cctx, id, cancel, &canceled); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not stop")
	}
	if canceled.Load() || cctx.Err() == nil {
		t.Fatal("lease loss must cancel the context without marking user cancel")
	}
}

// longClip makes a clip that takes a while to encode with video-vp9.
func longClip(t *testing.T, secs int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "long.mkv")
	if b, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
		"testsrc=duration="+strconv.Itoa(secs)+":size=1280x720:rate=25", "-c:v", "mpeg4", p).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	return p
}

func (e env) waitStatus(t *testing.T, id string, st jobs.Status) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if j, err := e.store.Get(context.Background(), id); err == nil && j.Status == st {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job never reached %s", st)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestProcessOneUserCancelRemovesInputAndOutput(t *testing.T) {
	e := setup(t, defaultLimits)
	id := e.enqueue(t, longClip(t, 60), "video-vp9")
	ctx := context.Background()
	go func() {
		e.waitStatus(t, id, jobs.StatusRunning)
		_ = e.store.RequestCancel(ctx, id)
	}()
	if ok, err := e.w.ProcessOne(ctx); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	j, _ := e.store.Get(ctx, id)
	if j.Status != jobs.StatusCanceled {
		t.Fatalf("want canceled, got %+v", j)
	}
	if exists(j.InputPath) || len(e.outFiles(t)) != 0 {
		t.Fatalf("input/output must be removed; out=%v", e.outFiles(t))
	}
}

func TestProcessOneShutdownLeavesJobAndInput(t *testing.T) {
	e := setup(t, defaultLimits)
	id := e.enqueue(t, longClip(t, 60), "video-vp9")
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		e.waitStatus(t, id, jobs.StatusRunning)
		time.Sleep(300 * time.Millisecond)
		stop()
	}()
	if _, err := e.w.ProcessOne(ctx); err != nil {
		t.Fatal(err)
	}
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusRunning || j.Error != "" {
		t.Fatalf("job must be left in place, got %+v", j)
	}
	if !exists(j.InputPath) {
		t.Fatal("input must be kept for requeue")
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatal("partial output must be removed")
	}
}

func TestProcessOneOutputCapKillsMidRun(t *testing.T) {
	e := setup(t, defaultLimits)
	e.w.MaxOutputBytes = 20000
	id := e.enqueue(t, longClip(t, 120), "video-vp9")
	start := time.Now()
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || j.Error != "output exceeds size limit" {
		t.Fatalf("bad job %+v", j)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("run was not cut short")
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatal("oversize output must be removed")
	}
}

// blockTerminal installs a trigger that intercepts writes setting the job to done.
func (e env) blockTerminal(t *testing.T, action string) {
	t.Helper()
	ctx := context.Background()
	body := "RETURN NULL;"
	if action == "error" {
		body = "RAISE EXCEPTION 'boom';"
	}
	for _, q := range []string{
		`CREATE FUNCTION cf_block() RETURNS trigger AS $$ BEGIN IF NEW.status = 'done' THEN ` + body + ` END IF; RETURN NEW; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER cf_block BEFORE UPDATE ON jobs FOR EACH ROW EXECUTE FUNCTION cf_block()`,
	} {
		if _, err := e.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTerminalWriteLeaseLostKeepsInput(t *testing.T) {
	e := setup(t, defaultLimits)
	e.blockTerminal(t, "skip") // Complete matches no row => ErrNotFound
	id := e.enqueue(t, testutil.MakeClip(t, t.TempDir(), "a.wav", "audio"), "audio-wav")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusRunning {
		t.Fatalf("status should be untouched, got %+v", j)
	}
	if !exists(j.InputPath) {
		t.Fatal("input must be kept when the terminal write is rejected")
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatal("own output must be removed")
	}
}

func TestTerminalWriteErrorKeepsInput(t *testing.T) {
	e := setup(t, defaultLimits)
	e.blockTerminal(t, "error")
	id := e.enqueue(t, testutil.MakeClip(t, t.TempDir(), "a.wav", "audio"), "audio-wav")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusRunning || !exists(j.InputPath) || len(e.outFiles(t)) != 0 {
		t.Fatalf("input kept, output removed expected: %+v out=%v", j, e.outFiles(t))
	}
}

func TestPerAttemptOutputNamesDiffer(t *testing.T) {
	e := setup(t, defaultLimits)
	ctx := context.Background()
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-wav")
	_, _ = e.w.ProcessOne(ctx)
	j1, _ := e.store.Get(ctx, id)
	b, _ := os.ReadFile(src)
	_ = os.WriteFile(j1.InputPath, b, 0o600)
	if _, err := e.pool.Exec(ctx, `UPDATE jobs SET status='queued', worker_id='', finished_at=NULL WHERE id=$1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	_, _ = e.w.ProcessOne(ctx)
	j2, _ := e.store.Get(ctx, id)
	if j1.Status != jobs.StatusDone || j2.Status != jobs.StatusDone || j1.OutputPath == j2.OutputPath {
		t.Fatalf("outputs must differ: %q vs %q", j1.OutputPath, j2.OutputPath)
	}
	if !exists(j1.OutputPath) || !exists(j2.OutputPath) {
		t.Fatal("both attempts' outputs should exist (cleanup is the janitor's job)")
	}
}

// The late terminal write of a zombie worker must neither change the re-claimed job nor delete its input.
func TestZombieTerminalWriteKeepsInputAndJob(t *testing.T) {
	e := setup(t, defaultLimits)
	ctx := context.Background()
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-wav")
	j, err := e.store.Claim(ctx, "w1", presets.IDs(presets.All()))
	if err != nil || j == nil {
		t.Fatal(err)
	}
	// the janitor requeues (lease expired) and another worker claims the job
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET heartbeat_at = now() - interval '10 minutes'`)
	if r, _, err := e.store.RequeueStale(ctx, time.Minute, 3); err != nil || r != 1 {
		t.Fatalf("requeue %d %v", r, err)
	}
	if c, err := e.store.Claim(ctx, "other", presets.IDs(presets.All())); err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	// worker w1 (zombie) finishes its attempt: process() must find the lease lost
	e.w.ProcessClaimed(ctx, j)
	got, _ := e.store.Get(ctx, id)
	if got.Status != jobs.StatusProbing || got.WorkerID != "other" {
		t.Fatalf("other worker's job was touched: %+v", got)
	}
	if _, err := os.Stat(j.InputPath); err != nil {
		t.Fatalf("zombie deleted the input of the re-claimed job: %v", err)
	}
}

// If heartbeats keep failing with store errors (database outage) for longer than LeaseTimeout the worker
// must assume the janitor requeued the job: stop FFmpeg, remove its partial output, keep the input and
// write nothing.
func TestWorkerAbandonsJobWhenHeartbeatsFailPastLeaseTimeout(t *testing.T) {
	e := setup(t, defaultLimits)
	ctx := context.Background()
	flaky, err := pgxpool.NewWithConfig(ctx, e.pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer flaky.Close()
	w := &worker.Worker{
		ID: "w2", Store: jobs.NewStore(flaky), Audit: audit.New(&strings.Builder{}),
		FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", Flavor: "full",
		OutDir: e.w.OutDir, ScratchDir: t.TempDir(), Limits: defaultLimits,
		Poll: 50 * time.Millisecond, HeartbeatEvery: 50 * time.Millisecond, LeaseTimeout: 600 * time.Millisecond,
	}
	if err := w.RegisterCapabilities(ctx); err != nil {
		t.Fatal(err)
	}
	id := e.enqueue(t, longClip(t, 60), "video-vp9")
	go func() {
		for i := 0; i < 600; i++ {
			if j, err := e.store.Get(ctx, id); err == nil && j.Status == jobs.StatusRunning {
				flaky.Close() // the worker loses its database connection mid-run
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	start := time.Now()
	if ok, err := w.ProcessOne(ctx); !ok || err != nil {
		t.Fatalf("ProcessOne: %v %v", ok, err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("worker did not stop ffmpeg after heartbeats failed past the lease timeout")
	}
	j, _ := e.store.Get(ctx, id)
	if j.Status != jobs.StatusRunning || j.Error != "" {
		t.Fatalf("a worker without database access cannot have written anything: %+v", j)
	}
	if _, err := os.Stat(j.InputPath); err != nil {
		t.Fatal("input must be kept for the requeue")
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatal("partial output must be removed")
	}
}

// countingFFmpeg is a fake ffmpeg that records every invocation and advertises a few encoders.
func countingFFmpeg(t *testing.T) (bin, counter string) {
	t.Helper()
	d := t.TempDir()
	counter = filepath.Join(d, "calls")
	bin = filepath.Join(d, "ffmpeg")
	script := "#!/bin/sh\necho x >> " + counter + "\nfor a in \"$@\"; do if [ \"$a\" = \"-encoders\" ]; then cat <<'EOT'\n" +
		" Encoders:\n ------\n A....D pcm_s16le   PCM\n A....D libopus     Opus\n A....D flac        FLAC\nEOT\nexit 0; fi; done\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, counter
}

func calls(t *testing.T, counter string) int {
	t.Helper()
	b, err := os.ReadFile(counter)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "x\n")
}

func TestCapabilityTickDoesNotSpawnFFmpeg(t *testing.T) {
	pool := dbtest.New(t)
	bin, counter := countingFFmpeg(t)
	w := &worker.Worker{ID: "wt", Store: jobs.NewStore(pool), Audit: audit.New(&strings.Builder{}), FFmpegBin: bin, Flavor: "lgpl",
		DetectEvery: time.Hour, FailureRedetectGap: time.Hour}
	ctx := context.Background()
	if err := w.RegisterCapabilities(ctx); err != nil {
		t.Fatal(err)
	}
	base := calls(t, counter)
	if base < 3 { // -encoders plus one trial encode per advertised encoder
		t.Fatalf("startup detection ran %d ffmpeg processes", base)
	}
	_, _ = pool.Exec(ctx, `UPDATE workers SET seen_at = now() - interval '1 hour' WHERE id = 'wt'`)
	for i := 0; i < 5; i++ {
		w.Tick(ctx)
	}
	if got := calls(t, counter); got != base {
		t.Fatalf("ticks spawned ffmpeg: %d -> %d", base, got)
	}
	var fresh bool
	_ = pool.QueryRow(ctx, `SELECT seen_at > now() - interval '1 minute' AND cardinality(encoders) = 3 FROM workers WHERE id = 'wt'`).Scan(&fresh)
	if !fresh {
		t.Fatal("tick must refresh seen_at and keep the encoder list")
	}
	// A failed job asks for a re-detection, but not within the minimum gap...
	w.FlagRedetect()
	w.Tick(ctx)
	if got := calls(t, counter); got != base {
		t.Fatalf("re-detection ignored the gap: %d -> %d", base, got)
	}
}

func TestCapabilityRedetectionWhenDueOrAfterFailure(t *testing.T) {
	pool := dbtest.New(t)
	bin, counter := countingFFmpeg(t)
	w := &worker.Worker{ID: "wt", Store: jobs.NewStore(pool), Audit: audit.New(&strings.Builder{}), FFmpegBin: bin, Flavor: "lgpl",
		DetectEvery: 150 * time.Millisecond, FailureRedetectGap: 50 * time.Millisecond}
	ctx := context.Background()
	if err := w.RegisterCapabilities(ctx); err != nil {
		t.Fatal(err)
	}
	base := calls(t, counter)
	time.Sleep(200 * time.Millisecond)
	w.Tick(ctx) // older than DetectEvery
	afterDue := calls(t, counter)
	if afterDue <= base {
		t.Fatalf("due detection did not run: %d -> %d", base, afterDue)
	}
	w.DetectEvery = time.Hour
	time.Sleep(80 * time.Millisecond)
	w.FlagRedetect()
	w.Tick(ctx) // failure flag, gap elapsed
	if got := calls(t, counter); got <= afterDue {
		t.Fatalf("failure-triggered detection did not run: %d -> %d", afterDue, got)
	}
}
