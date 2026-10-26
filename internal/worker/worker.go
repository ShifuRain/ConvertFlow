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
	"github.com/ShifuRain/convertflow/internal/blob"
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
	ID          string
	Store       *jobs.Store
	Audit       *audit.Logger
	FFmpegBin   string
	FFprobeBin  string
	Flavor      string
	VAAPIDevice string
	// Blob stores job inputs and outputs: the worker gets a local copy of the input and publishes
	// each attempt's output through it.
	Blob           blob.Store
	ScratchDir     string
	Limits         ffmpeg.Limits
	Poll           time.Duration
	HeartbeatEvery time.Duration
	// LeaseTimeout is how long heartbeats may keep failing with store errors (for example a
	// database outage) before the worker assumes the janitor has requeued its job: it then
	// stops FFmpeg and abandons the job without any terminal write. Set it to the janitor's
	// STALE_AFTER. Zero disables the check.
	LeaseTimeout time.Duration
	// JobTimeout is the wall-clock limit for one FFmpeg run (default 4h). It is the backstop
	// for inputs whose duration is unknown to ffprobe.
	JobTimeout time.Duration
	// MaxOutputBytes kills a run whose output file grows beyond it (default 16 GiB).
	MaxOutputBytes int64

	// DetectEvery is the longest time between full capability detections (which run a trial
	// encode per encoder); default 1h. FailureRedetectGap is the minimum time between two
	// detections triggered by a failed job; default 1m.
	DetectEvery        time.Duration
	FailureRedetectGap time.Duration

	redetect atomic.Bool // set after a job failed inside FFmpeg: capabilities may have changed

	mu           sync.RWMutex
	encoders     map[string]bool
	encoderList  []string // as advertised; re-sent by the cheap refresh. guarded by mu
	presetIDs    []string // presets resolvable with encoders; guarded by mu
	lastDetected time.Time
}

const (
	capabilityTick            = 15 * time.Second
	defaultDetectEvery        = time.Hour
	defaultFailureRedetectGap = time.Minute
)

func (w *Worker) RegisterCapabilities(ctx context.Context) error {
	encs, err := ffmpeg.DetectEncoders(ctx, w.FFmpegBin, w.Flavor, w.VAAPIDevice)
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(encs))
	for _, e := range encs {
		set[e] = true
	}
	ids := presets.IDs(presets.Available(set))
	w.mu.Lock()
	w.encoders = set
	w.encoderList = encs
	w.presetIDs = ids
	w.lastDetected = time.Now()
	w.mu.Unlock()
	return w.Store.UpsertWorker(ctx, w.ID, encs)
}

// tick runs every capabilityTick. It normally only refreshes the worker's liveness row
// (seen_at) from the cached encoder list, which spawns no process. The full detection with trial
// encodes runs only when it is older than DetectEvery, or (at most every FailureRedetectGap)
// after a job failed inside FFmpeg.
func (w *Worker) tick(ctx context.Context) {
	every, gap := w.DetectEvery, w.FailureRedetectGap
	if every <= 0 {
		every = defaultDetectEvery
	}
	if gap <= 0 {
		gap = defaultFailureRedetectGap
	}
	w.mu.RLock()
	age := time.Since(w.lastDetected)
	encs := w.encoderList
	w.mu.RUnlock()
	if age >= every || (w.redetect.Load() && age >= gap) {
		w.redetect.Store(false)
		if err := w.RegisterCapabilities(ctx); err != nil {
			slog.Warn("capability detection failed", "err", err)
		}
		return
	}
	if err := w.Store.UpsertWorker(ctx, w.ID, encs); err != nil {
		slog.Warn("capability refresh failed", "err", err)
	}
}

func (w *Worker) claimable() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.presetIDs
}

func (w *Worker) snapshot() map[string]bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.encoders
}

// Run registers capabilities (full detection at startup), keeps the registration alive and
// re-detects hourly or after a failed job (see tick), and processes jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.RegisterCapabilities(ctx); err != nil {
		return fmt.Errorf("detecting encoders: %w", err)
	}
	go func() {
		t := time.NewTicker(capabilityTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.tick(ctx)
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
	j, err := w.Store.Claim(ctx, w.ID, w.claimable())
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
	var out output
	inPath, releaseIn, err := w.Blob.LocalInput(jobCtx, j.InputPath, w.ScratchDir)
	if err != nil {
		if jobCtx.Err() == nil {
			slog.Error("input unavailable", "job_id", j.ID, "worker", w.ID, "err", err)
		}
		err = errInternal
	} else {
		defer releaseIn()
		out, err = w.run(jobCtx, cancel, &st, j, inPath)
	}
	cancel()
	<-watchDone

	if ctx.Err() != nil { // worker shutting down: leave the job (and its input) for requeue
		removeIf(out.path)
		return
	}
	if st.lost.Load() { // requeued, canceled or deleted behind our back: abort quietly
		removeIf(out.path)
		w.Audit.System("job_lease_lost", j.ID, "worker", w.ID)
		return
	}
	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer bgCancel()

	// Publish the finished output (a no-op for the local store, where it already lies in out/).
	var outLoc string
	if err == nil && !st.canceled.Load() {
		if outLoc, err = out.commit(bg); err != nil {
			slog.Error("publishing output failed", "job_id", j.ID, "worker", w.ID, "err", err)
			err = errInternal
		}
	}

	// The input is deleted only after the fenced terminal write succeeds; if that write
	// fails (lease lost or store error) the input is kept, because the job may be requeued,
	// and only this attempt's own output is removed.
	var werr error
	switch {
	case st.canceled.Load():
		removeIf(out.path)
		werr = w.Store.MarkCanceled(bg, j.ID, w.ID)
		w.logStoreErr("mark_canceled", j.ID, werr)
		if werr == nil {
			w.Audit.System("job_canceled", j.ID, "worker", w.ID)
		}
	case err != nil:
		removeIf(out.path)
		if !errors.Is(err, errTimeout) && !errors.Is(err, errOversize) && !errors.Is(err, errInternal) && j.EncoderUsed != "" {
			w.redetect.Store(true) // FFmpeg itself failed: an encoder may have stopped working
		}
		msg := ffmpeg.Sanitize(err.Error(), dirOf(j.InputPath), dirOf(inPath), dirOf(out.path), w.ScratchDir)
		werr = w.Store.Fail(bg, j.ID, w.ID, msg)
		w.logStoreErr("fail", j.ID, werr)
		if werr == nil {
			w.Audit.System("job_failed", j.ID, "worker", w.ID, "error", msg)
		}
	default:
		werr = w.Store.Complete(bg, j.ID, w.ID, outLoc)
		w.logStoreErr("complete", j.ID, werr)
		if werr == nil {
			w.Audit.System("job_finished", j.ID, "worker", w.ID, "encoder", j.EncoderUsed)
		} else {
			_ = w.Blob.Remove(bg, outLoc)
			removeIf(out.path)
		}
	}
	if werr == nil {
		_ = w.Blob.Remove(bg, j.InputPath)
	}
}

// output is an attempt's local output file and the func that publishes it to the blob store.
type output struct {
	path   string
	commit func(context.Context) (string, error)
}

// dirOf returns the directory of p, or "" for an empty p (so Sanitize never strips ".").
func dirOf(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

func maxOutBytes(v int64) int64 {
	if v <= 0 {
		return defaultMaxOutputBytes
	}
	return v
}

func removeIf(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// logStoreErr logs unexpected store failures (job id and error only, no job paths).
// ErrNotFound means the lease was lost and is handled quietly by the callers.
func (w *Worker) logStoreErr(op, id string, err error) {
	if err != nil && !errors.Is(err, jobs.ErrNotFound) {
		slog.Error("store write failed", "op", op, "job_id", id, "worker", w.ID, "err", err)
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
	lastOK := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hctx, hcancel := ctx, context.CancelFunc(func() {})
			if w.LeaseTimeout > 0 { // a hung database connection must not stall the lease check
				hctx, hcancel = context.WithTimeout(ctx, max(w.HeartbeatEvery, w.LeaseTimeout/3))
			}
			req, err := w.Store.Heartbeat(hctx, id, w.ID)
			hcancel()
			if errors.Is(err, jobs.ErrNotFound) {
				st.lost.Store(true)
				cancel()
				return
			}
			if err == nil {
				lastOK = time.Now()
				if req {
					st.canceled.Store(true)
					cancel()
					return
				}
				continue
			}
			if ctx.Err() != nil {
				return
			}
			// The janitor requeues a job after STALE_AFTER without heartbeat, so after that long
			// without a successful one this worker must assume it lost the job.
			if w.LeaseTimeout > 0 && time.Since(lastOK) >= w.LeaseTimeout {
				slog.Error("heartbeat failing past the lease timeout; abandoning job", "job_id", id, "worker", w.ID, "err", err)
				st.lost.Store(true)
				cancel()
				return
			}
		}
	}
}

// run performs probe, validate and encode. Returned errors are safe to show to users
// (they still pass through Sanitize before being stored).
//
// inPath is the local input file; the returned output names the attempt's local output file once
// one was allocated (also on error, so the caller can remove it).
func (w *Worker) run(ctx context.Context, cancel context.CancelFunc, st *jobState, j *jobs.Job, inPath string) (output, error) {
	pr, raw, err := ffmpeg.Probe(ctx, w.FFprobeBin, inPath, w.ScratchDir)
	if err != nil {
		return output{}, err
	}
	if err := pr.Validate(w.Limits); err != nil {
		return output{}, err
	}
	if err := w.Store.SetProbe(ctx, j.ID, w.ID, raw); err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			st.lost.Store(true)
			return output{}, errLeaseLost
		}
		w.logStoreErr("set_probe", j.ID, err)
	}

	preset, ok := presets.Get(j.Options.Preset)
	if !ok {
		return output{}, fmt.Errorf("unknown preset %q", j.Options.Preset)
	}
	if preset.Kind == presets.KindVideo && !pr.HasVideo() {
		return output{}, ffmpeg.ErrNoVideo
	}
	if preset.Kind == presets.KindAudio && !pr.HasAudio() {
		return output{}, ffmpeg.ErrNoAudio
	}
	cand, ok := preset.Resolve(w.snapshot())
	if !ok {
		return output{}, fmt.Errorf("no encoder available on this worker for preset %q", preset.ID)
	}
	encoder := cand.Audio
	if cand.Video != "" {
		encoder = cand.Video
	}
	if err := w.Store.SetRunning(ctx, j.ID, w.ID, encoder); err != nil {
		if errors.Is(err, jobs.ErrNotFound) {
			st.lost.Store(true)
			return output{}, errLeaseLost
		}
		w.logStoreErr("set_running", j.ID, err)
		return output{}, errInternal
	}
	j.EncoderUsed = encoder

	// Per-attempt name: a stale worker's cleanup can never touch a later attempt's output.
	outPath, commit, err := w.Blob.NewOutput(ctx, j.ID, j.Attempts, preset.Ext)
	if err != nil {
		slog.Error("allocating output failed", "job_id", j.ID, "worker", w.ID, "err", err)
		return output{}, errInternal
	}
	out := output{path: outPath, commit: commit}
	args, err := ffmpeg.BuildArgs(ffmpeg.Spec{
		Input: inPath, Output: outPath, Preset: preset, Cand: cand, Opts: j.Options,
		HasVideo: pr.HasVideo(), HasAudio: pr.HasAudio(), VAAPIDevice: w.VAAPIDevice,
		MaxOutputBytes: maxOutBytes(w.MaxOutputBytes),
	})
	if err != nil {
		return output{}, err
	}

	timeout := w.JobTimeout
	if timeout <= 0 {
		timeout = defaultJobTimeout
	}
	maxOut := maxOutBytes(w.MaxOutputBytes)
	runCtx, runCancel := context.WithTimeout(ctx, timeout)
	defer runCancel()

	var oversize atomic.Bool
	checkSize := func() {
		if fi, err := os.Stat(outPath); err == nil && fi.Size() >= maxOut {
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
			e := w.Store.UpdateProgress(ctx, j.ID, w.ID, p)
			if errors.Is(e, jobs.ErrNotFound) {
				st.lost.Store(true)
				cancel()
			} else if ctx.Err() == nil {
				w.logStoreErr("update_progress", j.ID, e)
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
		return out, errLeaseLost
	case ctx.Err() != nil:
		return out, ctx.Err()
	case oversize.Load():
		return out, errOversize
	case err != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return out, errTimeout
	case err != nil:
		return out, fmt.Errorf("transcode failed: %s", ffmpeg.Sanitize(tail, dirOf(j.InputPath), dirOf(inPath), filepath.Dir(outPath), w.ScratchDir))
	}
	return out, nil
}
