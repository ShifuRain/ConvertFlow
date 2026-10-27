package worker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/blob/s3test"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/testutil"
	"github.com/ShifuRain/convertflow/internal/worker"
	"github.com/google/uuid"
)

func scratchEmpty(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		var names []string
		for _, en := range ents {
			names = append(names, en.Name())
		}
		t.Fatalf("scratch not cleaned: %v", names)
	}
}

func TestRunModeOnceProcessesExactlyOneJob(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	first := e.enqueue(t, src, "audio-flac")
	second := e.enqueue(t, src, "audio-flac")
	s, err := e.w.RunMode(context.Background(), worker.Mode{Once: true, IdleExit: 5 * time.Second})
	if err != nil || s.Jobs != 1 || !s.Finished || s.Interrupted || s.IdleExit {
		t.Fatalf("summary %+v err %v", s, err)
	}
	ctx := context.Background()
	if j, _ := e.store.Get(ctx, first); j.Status != jobs.StatusDone {
		t.Fatalf("first job %+v", j)
	}
	if j, _ := e.store.Get(ctx, second); j.Status != jobs.StatusQueued {
		t.Fatalf("a once worker must leave the second job alone: %+v", j)
	}
	scratchEmpty(t, e.w.ScratchDir)
}

// A failed job is a job result, not a worker failure: Finished is true.
func TestRunModeOnceFailedJobIsFinished(t *testing.T) {
	e := setup(t, defaultLimits)
	src := filepath.Join(t.TempDir(), "x.mp4")
	_ = os.WriteFile(src, []byte("not media"), 0o600)
	id := e.enqueue(t, src, "audio-wav")
	s, err := e.w.RunMode(context.Background(), worker.Mode{Once: true, IdleExit: time.Second})
	if err != nil || s.Jobs != 1 || !s.Finished {
		t.Fatalf("summary %+v err %v", s, err)
	}
	if j, _ := e.store.Get(context.Background(), id); j.Status != jobs.StatusFailed {
		t.Fatalf("job %+v", j)
	}
}

func TestRunModeOnceIdleExitWithoutJob(t *testing.T) {
	e := setup(t, defaultLimits)
	start := time.Now()
	s, err := e.w.RunMode(context.Background(), worker.Mode{Once: true, IdleExit: 400 * time.Millisecond})
	took := time.Since(start)
	if err != nil || s.Jobs != 0 || !s.IdleExit {
		t.Fatalf("summary %+v err %v", s, err)
	}
	if took < 400*time.Millisecond || took > 3*time.Second {
		t.Fatalf("idle exit after %s, want about 400ms", took)
	}
}

// An autoscaler may start the worker before the job is committed: it polls until IdleExit.
func TestRunModeOnceFindsJobQueuedAfterStart(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	idc := make(chan string, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		idc <- e.enqueue(t, src, "audio-flac")
	}()
	s, err := e.w.RunMode(context.Background(), worker.Mode{Once: true, IdleExit: 10 * time.Second})
	if err != nil || s.Jobs != 1 || !s.Finished {
		t.Fatalf("summary %+v err %v", s, err)
	}
	if j, _ := e.store.Get(context.Background(), <-idc); j.Status != jobs.StatusDone {
		t.Fatalf("job %+v", j)
	}
}

func TestRunModeIdleExitKeepsProcessingThenExits(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	ids := []string{e.enqueue(t, src, "audio-flac"), e.enqueue(t, src, "audio-wav"), e.enqueue(t, src, "audio-flac")}
	start := time.Now()
	s, err := e.w.RunMode(context.Background(), worker.Mode{IdleExit: 500 * time.Millisecond})
	if err != nil || s.Jobs != 3 || !s.Finished || !s.IdleExit {
		t.Fatalf("summary %+v err %v", s, err)
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("took %s", took)
	}
	for _, id := range ids {
		if j, _ := e.store.Get(context.Background(), id); j.Status != jobs.StatusDone {
			t.Fatalf("job %+v", j)
		}
	}
	scratchEmpty(t, e.w.ScratchDir)
}

func TestRunModeAlwaysOnRunsUntilCanceled(t *testing.T) {
	e := setup(t, defaultLimits)
	// Long enough for the startup detection (trial encodes) plus several idle polls.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s, err := e.w.RunMode(ctx, worker.Mode{})
	if err != nil || s.IdleExit || s.Jobs != 0 {
		t.Fatalf("summary %+v err %v", s, err)
	}
	if ctx.Err() == nil {
		t.Fatal("always-on mode returned before ctx ended")
	}
}

func TestRunModeOnceRequiresIdleExit(t *testing.T) {
	e := setup(t, defaultLimits)
	if _, err := e.w.RunMode(context.Background(), worker.Mode{Once: true}); err == nil {
		t.Fatal("once without idle exit must be rejected")
	}
}

// When every claim of the idle period fails (database trouble), the worker reports an error
// instead of a clean idle exit.
func TestRunModeIdleExitReportsPersistentClaimErrors(t *testing.T) {
	e := setup(t, defaultLimits)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `ALTER TABLE jobs RENAME TO jobs_gone`); err != nil {
		t.Fatal(err)
	}
	s, err := e.w.RunMode(ctx, worker.Mode{Once: true, IdleExit: 300 * time.Millisecond})
	if err == nil || s.Jobs != 0 {
		t.Fatalf("want a claim error, got %+v %v", s, err)
	}
}

// SIGTERM (ctx canceled) mid-job: FFmpeg is stopped, the job is released at once (lease expired,
// no terminal write, input kept, partial output removed), and another worker completes it after
// the janitor's next requeue pass. The attempt counts like a crash would, no more.
func TestShutdownReleasesJobAndAnotherWorkerCompletesIt(t *testing.T) {
	e := setup(t, defaultLimits)
	var log strings.Builder
	e.w.Audit = audit.New(&log)
	id := e.enqueue(t, longClip(t, 20), "video-vp9")
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		e.waitStatus(t, id, jobs.StatusRunning)
		time.Sleep(300 * time.Millisecond)
		stop()
	}()
	s, err := e.w.RunMode(ctx, worker.Mode{Once: true, IdleExit: 10 * time.Second})
	if err != nil || s.Jobs != 1 || s.Finished || !s.Interrupted {
		t.Fatalf("summary %+v err %v", s, err)
	}
	bg := context.Background()
	j, _ := e.store.Get(bg, id)
	if j.Status != jobs.StatusRunning || j.Error != "" || j.Attempts != 1 || !exists(j.InputPath) {
		t.Fatalf("job must be left for retry with its input: %+v", j)
	}
	var released bool
	_ = e.pool.QueryRow(bg, `SELECT heartbeat_at = '-infinity' FROM jobs WHERE id = $1::uuid`, id).Scan(&released)
	if !released {
		t.Fatal("the lease must be expired at once, not left for STALE_AFTER")
	}
	if len(e.outFiles(t)) != 0 {
		t.Fatalf("partial output left: %v", e.outFiles(t))
	}
	if !strings.Contains(log.String(), `"event":"job_released"`) || !strings.Contains(log.String(), `"reason":"shutdown"`) {
		t.Fatalf("audit: %s", log.String())
	}
	scratchEmpty(t, e.w.ScratchDir)

	// The janitor requeues it on its next pass, however long STALE_AFTER is.
	if rq, failed, err := e.store.RequeueStale(bg, time.Hour, 3); err != nil || rq != 1 || failed != 0 {
		t.Fatalf("requeue: %d %d %v", rq, failed, err)
	}
	w2 := &worker.Worker{
		ID: "w2", Store: e.store, Audit: audit.New(&strings.Builder{}),
		FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", Flavor: "full",
		Blob: e.w.Blob, ScratchDir: t.TempDir(), Limits: defaultLimits,
		Poll: 50 * time.Millisecond, HeartbeatEvery: 50 * time.Millisecond,
	}
	s2, err := w2.RunMode(bg, worker.Mode{Once: true, IdleExit: 5 * time.Second})
	if err != nil || s2.Jobs != 1 || !s2.Finished {
		t.Fatalf("second worker: %+v %v", s2, err)
	}
	j, _ = e.store.Get(bg, id)
	if j.Status != jobs.StatusDone || j.WorkerID != "w2" || j.Attempts != 2 {
		t.Fatalf("second worker must complete the job on attempt 2: %+v", j)
	}
}

// A user cancel request that arrived before the shutdown is still honored (terminal write).
func TestShutdownAfterCancelRequestMarksCanceled(t *testing.T) {
	e := setup(t, defaultLimits)
	id := e.enqueue(t, longClip(t, 20), "video-vp9")
	ctx, stop := context.WithCancel(context.Background())
	go func() {
		e.waitStatus(t, id, jobs.StatusRunning)
		_ = e.store.RequestCancel(context.Background(), id)
		time.Sleep(200 * time.Millisecond) // a heartbeat (50ms) sees the request first
		stop()
	}()
	_, _ = e.w.RunMode(ctx, worker.Mode{Once: true, IdleExit: 10 * time.Second})
	if j, _ := e.store.Get(context.Background(), id); j.Status != jobs.StatusCanceled {
		t.Fatalf("want canceled, got %+v", j)
	}
}

// stallFFmpeg is a stand-in for an FFmpeg whose encoder failed to start but that did not exit.
func stallFFmpeg(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffmpeg")
	// Capability detection (no -progress) goes to the real ffmpeg; a conversion hangs.
	real, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = -progress ]; then\n" +
		"echo 'svt: thread creation failed in /very/secret/path' >&2\nprintf 'frame=0\\nout_time_us=0\\nprogress=continue\\n'\nexec sleep 60\nfi; done\n" +
		"exec " + real + " \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStallWatchdogFailsHungJob(t *testing.T) {
	e := setup(t, defaultLimits) // capabilities detected with the real ffmpeg
	e.w.FFmpegBin = stallFFmpeg(t)
	e.w.StallTimeout = time.Second
	id := e.enqueue(t, testutil.MakeClip(t, t.TempDir(), "a.wav", "audio"), "audio-flac")
	start := time.Now()
	s, err := e.w.RunMode(context.Background(), worker.Mode{Once: true, IdleExit: time.Second})
	if err != nil || !s.Finished {
		t.Fatalf("summary %+v err %v", s, err)
	}
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("stalled job took %s", took)
	}
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || j.Error != "conversion stalled (no progress for 1s)" {
		t.Fatalf("job %s %q", j.Status, j.Error)
	}
	if len(e.outFiles(t)) != 0 || exists(j.InputPath) {
		t.Fatal("a failed job's input and output must be removed")
	}
	scratchEmpty(t, e.w.ScratchDir)
}

// A real encode with a stall timeout far below its run time completes: progress keeps resetting
// the watchdog.
func TestStallWatchdogSparesProgressingEncode(t *testing.T) {
	e := setup(t, defaultLimits)
	e.w.StallTimeout = 700 * time.Millisecond
	id := e.enqueue(t, longClip(t, 8), "video-vp9")
	start := time.Now()
	if ok, err := e.w.ProcessOne(context.Background()); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusDone {
		t.Fatalf("progressing encode was killed: %s %q", j.Status, j.Error)
	}
	if took := time.Since(start); took < 2*e.w.StallTimeout {
		t.Logf("encode took only %s; the test proves less than intended", took)
	}
}

// Once mode through a real bucket, including a shutdown mid-job: nothing is left in the scratch
// directory (input download, partial output) and the released job keeps its stored input.
func TestOnceModeWithS3LeavesNoScratchFiles(t *testing.T) {
	s3e := s3test.New(t)
	e := setup(t, defaultLimits)
	s3e.Opts.ScratchDir = e.w.ScratchDir
	s3e.Opts.MaxInputBytes = 1 << 30
	s := s3e.Store(t)
	e.w.Blob = s
	bg := context.Background()
	enqueue := func(src, preset string) string {
		id := uuid.NewString()
		w, loc, err := s.CreateInput(bg, id)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(src)
		_, _ = w.Write(b)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := e.store.Create(bg, &jobs.Job{ID: id, Filename: "x", InputPath: loc, Options: jobs.Options{Preset: preset}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := enqueue(testutil.MakeClip(t, t.TempDir(), "a.wav", "audio"), "audio-flac")
	if sum, err := e.w.RunMode(bg, worker.Mode{Once: true, IdleExit: 5 * time.Second}); err != nil || !sum.Finished {
		t.Fatalf("%+v %v", sum, err)
	}
	if j, _ := e.store.Get(bg, id); j.Status != jobs.StatusDone {
		t.Fatalf("job %+v", j)
	}
	scratchEmpty(t, e.w.ScratchDir)

	long := enqueue(longClip(t, 20), "video-vp9")
	ctx, stop := context.WithCancel(bg)
	go func() {
		e.waitStatus(t, long, jobs.StatusRunning)
		time.Sleep(300 * time.Millisecond)
		stop()
	}()
	sum, err := e.w.RunMode(ctx, worker.Mode{Once: true, IdleExit: 10 * time.Second})
	if err != nil || !sum.Interrupted {
		t.Fatalf("%+v %v", sum, err)
	}
	j, _ := e.store.Get(bg, long)
	if j.Status != jobs.StatusRunning {
		t.Fatalf("job %+v", j)
	}
	if _, err := s.StatInput(bg, j.InputPath); err != nil {
		t.Fatalf("stored input must be kept for the retry: %v", err)
	}
	scratchEmpty(t, e.w.ScratchDir)
}

func TestStallMessage(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Minute: "conversion stalled (no progress for 10 minutes)",
		time.Minute:      "conversion stalled (no progress for 1 minute)",
		90 * time.Second: "conversion stalled (no progress for 1m30s)",
	} {
		if got := worker.StallMessage(d); got != want {
			t.Errorf("%s: %q", d, got)
		}
	}
}
