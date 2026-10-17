// Package worker claims queued jobs and transcodes them with FFmpeg.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
)

const (
	defaultJobTimeout     = 4 * time.Hour
	defaultMaxOutputBytes = int64(16) << 30
	sizeCheckEvery        = 500 * time.Millisecond
)

// Errors whose text is stored on the job; they must never contain paths.
var (
	errTimeout   = errors.New("conversion timed out")
	errOversize  = errors.New("output exceeds size limit")
	errInternal  = errors.New("internal error")
	errLeaseLost = errors.New("lease lost") // never shown: the worker aborts quietly
)

type Worker struct {
	ID             string
	Store          *jobs.Store
	Audit          *audit.Logger
	FFmpegBin      string
	FFprobeBin     string
	Flavor         string
	VAAPIDevice    string
	OutDir         string
	ScratchDir     string
	Limits         ffmpeg.Limits
	Poll           time.Duration
	HeartbeatEvery time.Duration
	// JobTimeout is the wall-clock limit for one FFmpeg run (default 4h). It is the backstop
	// for inputs whose duration is unknown to ffprobe.
	JobTimeout time.Duration
	// MaxOutputBytes kills a run whose output file grows beyond it (default 16 GiB).
	MaxOutputBytes int64

	mu       sync.RWMutex
	encoders map[string]bool
}

func (w *Worker) RegisterCapabilities(ctx context.Context) error {
	encs, err := ffmpeg.DetectEncoders(ctx, w.FFmpegBin, w.Flavor, w.VAAPIDevice)
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(encs))
	for _, e := range encs {
		set[e] = true
	}
	w.mu.Lock()
	w.encoders = set
	w.mu.Unlock()
	return w.Store.UpsertWorker(ctx, w.ID, encs)
}

func (w *Worker) snapshot() map[string]bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.encoders
}

// Run registers capabilities, refreshes them periodically and processes jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.RegisterCapabilities(ctx); err != nil {
		return fmt.Errorf("detecting encoders: %w", err)
	}
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := w.RegisterCapabilities(ctx); err != nil {
					slog.Warn("capability refresh failed", "err", err)
				}
			}
		}
	}()
	for ctx.Err() == nil {
		ok, err := w.ProcessOne(ctx)
		if err != nil {
			slog.Error("process", "err", err)
		}
		if !ok {
			select {
			case <-ctx.Done():
			case <-time.After(w.Poll):
			}
		}
	}
	return nil
}

// ProcessOne claims and processes at most one job. It reports whether a job was claimed.
func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	j, err := w.Store.Claim(ctx, w.ID)
	if err != nil {
		return false, err
	}
	if j == nil {
		return false, nil
	}
	w.process(ctx, j)
	return true, nil
}

// jobState carries the out-of-band signals that classify how a run ended.
type jobState struct {
	canceled atomic.Bool // the user requested cancellation
	lost     atomic.Bool // the store no longer lets this worker touch the job
}

func (w *Worker) process(ctx context.Context, j *jobs.Job) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var st jobState
	watchDone := make(chan struct{})
	go func() { w.watch(jobCtx, j.ID, cancel, &st); close(watchDone) }()

	w.Audit.System("job_started", j.ID, "worker", w.ID, "attempt", j.Attempts)
	outPath, err := w.run(jobCtx, cancel, &st, j)
	cancel()
	<-watchDone

	if outPath != "" && (err != nil || st.lost.Load() || st.canceled.Load() || ctx.Err() != nil) {
		_ = os.Remove(outPath)
	}
	if ctx.Err() != nil { // worker shutting down: leave the job (and its input) for requeue
		return
	}
	if st.lost.Load() { // requeued, canceled or deleted behind our back: abort quietly
		w.Audit.System("job_lease_lost", j.ID, "worker", w.ID)
		return
	}
	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer bgCancel()
	_ = os.Remove(j.InputPath)
	switch {
	case st.canceled.Load():
		if e := w.Store.MarkCanceled(bg, j.ID); e == nil {
			w.Audit.System("job_canceled", j.ID, "worker", w.ID)
		}
	case err != nil:
		msg := ffmpeg.Sanitize(err.Error(), filepath.Dir(j.InputPath), w.OutDir, w.ScratchDir)
		if e := w.Store.Fail(bg, j.ID, msg); e == nil {
			w.Audit.System("job_failed", j.ID, "worker", w.ID, "error", msg)
		}
	default:
		if e := w.Store.Complete(bg, j.ID, outPath); e != nil {
			_ = os.Remove(outPath) // job vanished or was requeued: do not leave an orphan
			return
		}
		w.Audit.System("job_finished", j.ID, "worker", w.ID, "encoder", j.EncoderUsed)
	}
}

// Watch heartbeats the job and triggers cancel when the API requested cancellation.
// It also returns (cancelling the context without setting canceled) when the worker no
// longer owns the job.
func (w *Worker) Watch(ctx context.Context, id string, cancel context.CancelFunc, canceled *atomic.Bool) {
	st := &jobState{}
	w.watch(ctx, id, cancel, st)
	if st.canceled.Load() {
		canceled.Store(true)
	}
}

func (w *Worker) watch(ctx context.Context, id string, cancel context.CancelFunc, st *jobState) {
	t := time.NewTicker(w.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			req, err := w.Store.Heartbeat(ctx, id)
			if errors.Is(err, jobs.ErrNotFound) {
				st.lost.Store(true)
				cancel()
				return
			}
			if err == nil && req {
				st.canceled.Store(true)
				cancel()
				return
			}
		}
	}
}

// run performs probe, validate and encode. Returned errors are safe to show to users
// (they still pass through Sanitize before being stored).
func (w *Worker) run(ctx context.Context, cancel context.CancelFunc, st *jobState, j *jobs.Job) (string, error) {
	pr, raw, err := ffmpeg.Probe(ctx, w.FFprobeBin, j.InputPath, w.ScratchDir)
	if err != nil {
		return "", err
	}
	if err := pr.Validate(w.Limits); err != nil {
		return "", err
	}
	if err := w.Store.SetProbe(ctx, j.ID, raw); errors.Is(err, jobs.ErrNotFound) {
		st.lost.Store(true)
		return "", errLeaseLost
	}

	preset, ok := presets.Get(j.Options.Preset)
	if !ok {
		return "", fmt.Errorf("unknown preset %q", j.Options.Preset)
	}
	if preset.Kind == presets.KindVideo && !pr.HasVideo() {
		return "", ffmpeg.ErrNoVideo
	}
	if preset.Kind == presets.KindAudio && !pr.HasAudio() {
		return "", ffmpeg.ErrNoAudio
	}
	cand, ok := preset.Resolve(w.snapshot())
	if !ok {
		return "", fmt.Errorf("no encoder available on this worker for preset %q", preset.ID)
	}
	encoder := cand.Audio
	if cand.Video != "" {
		encoder = cand.Video
	}
	if err := w.Store.SetRunning(ctx, j.ID, encoder); err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			st.lost.Store(true)
			return "", errLeaseLost
		}
		return "", errInternal
	}
	j.EncoderUsed = encoder

	outPath := filepath.Join(w.OutDir, j.ID+"."+preset.Ext)
	args, err := ffmpeg.BuildArgs(ffmpeg.Spec{
		Input: j.InputPath, Output: outPath, Preset: preset, Cand: cand, Opts: j.Options,
		HasVideo: pr.HasVideo(), HasAudio: pr.HasAudio(), VAAPIDevice: w.VAAPIDevice,
	})
	if err != nil {
		return "", err
	}

	timeout := w.JobTimeout
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	maxOut := w.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = defaultMaxOutputBytes
	}
	runCtx, runCancel := context.WithTimeout(ctx, timeout)
	defer runCancel()

	var oversize atomic.Bool
	checkSize := func() {
		if fi, err := os.Stat(outPath); err == nil && fi.Size() > maxOut {
			oversize.Store(true)
			runCancel()
		}
	}
	monDone := make(chan struct{})
	monStop := make(chan struct{})
	go func() {
		defer close(monDone)
		t := time.NewTicker(sizeCheckEvery)
		defer t.Stop()
		for {
			select {
			case <-monStop:
				return
			case <-runCtx.Done():
				return
			case <-t.C:
				checkSize()
			}
		}
	}()

	// An unknown duration yields 0, which makes Run skip progress (indeterminate).
	dur := 0.0
	if pr.DurationKnown() {
		dur = pr.DurationSeconds()
	}
	var last time.Time
	tail, err := ffmpeg.Run(runCtx, w.FFmpegBin, args, w.ScratchDir, dur, func(p float64) {
		if time.Since(last) >= time.Second {
			last = time.Now()
			if e := w.Store.UpdateProgress(ctx, j.ID, p); errors.Is(e, jobs.ErrNotFound) {
				st.lost.Store(true)
				cancel()
			}
		}
	})
	close(monStop)
	<-monDone
	if err == nil {
		checkSize() // final check: the run may finish between ticks
	}

	// ffmpeg.Run reports "signal: killed" on cancel, so classify from our own state.
	switch {
	case st.lost.Load():
		return outPath, errLeaseLost
	case ctx.Err() != nil:
		return outPath, ctx.Err()
	case oversize.Load():
		return outPath, errOversize
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return outPath, errTimeout
	case err != nil:
		return outPath, fmt.Errorf("transcode failed: %s", ffmpeg.Sanitize(tail, filepath.Dir(j.InputPath), w.OutDir, w.ScratchDir))
	}
	return outPath, nil
}
