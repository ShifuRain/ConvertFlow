// Package worker claims queued jobs and transcodes them with FFmpeg.
package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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
	errTimeout       = errors.New("conversion timed out")
	errOversize      = errors.New("output exceeds size limit")
	errInternal      = errors.New("internal error")
	errInputTooLarge = errors.New("input exceeds the upload size limit")
	errLeaseLost     = errors.New("lease lost") // never shown: the worker aborts quietly
)

// errStalled is the message stored on a job the stall watchdog killed.
type errStalled time.Duration

func (e errStalled) Error() string {
	return "conversion stalled (no progress for " + humanDuration(time.Duration(e)) + ")"
}

// humanDuration writes whole minutes as "N minute(s)" and anything else in Go notation.
func humanDuration(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		if n := int64(d / time.Minute); n != 1 {
			return strconv.FormatInt(n, 10) + " minutes"
		}
		return "1 minute"
	}
	return d.String()
}

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
	// StallTimeout kills and fails a run when FFmpeg reports no progress and is essentially idle
	// for this long (see ffmpeg.RunOpts.StallTimeout). Zero disables the watchdog; JobTimeout
	// stays the outer backstop.
	StallTimeout time.Duration
	// ShutdownTimeout bounds the store and blob writes after a job ended, in particular the
	// release of an interrupted job when the worker is shut down (default 10s), so a container
	// grace period is respected.
	ShutdownTimeout time.Duration

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
	// Refresh the liveness row first, also before a full detection: its trial encodes take a while,
	// and the janitor and API must not see the worker as gone meanwhile (issue #9).
	if err := w.Store.UpsertWorker(ctx, w.ID, encs); err != nil {
		slog.Warn("capability refresh failed", "err", err)
	}
	if age >= every || (w.redetect.Load() && age >= gap) {
		w.redetect.Store(false)
		if err := w.RegisterCapabilities(ctx); err != nil {
			slog.Warn("capability detection failed", "err", err)
		}
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

// Mode selects when Run returns. The zero value is the always-on default: process jobs until ctx
// ends.
type Mode struct {
	// Once claims at most one job, runs it to its end and returns. Without a claimable job Run
	// polls (every Poll) until IdleExit has passed, then returns.
	Once bool
	// IdleExit makes Run return once no job was claimed for this long (measured from the start, or
	// from the end of the last job). Zero means never; Once requires a positive value.
	IdleExit time.Duration
}

// Ephemeral reports whether the worker exits on its own (Once or IdleExit).
func (m Mode) Ephemeral() bool { return m.Once || m.IdleExit > 0 }

// Summary describes how Run ended.
type Summary struct {
	// Jobs is the number of jobs claimed.
	Jobs int
	// Finished reports whether the last claimed job got its terminal write (done, failed or
	// canceled). A job without one was released for retry, abandoned after a lost lease, or its
	// terminal write failed; the janitor requeues it (or fails it once attempts are exhausted).
	Finished bool
	// Interrupted reports that ctx ended (a shutdown signal) while a job was running; the job was
	// released for retry.
	Interrupted bool
	// IdleExit reports that Run returned because IdleExit passed without a claimable job.
	IdleExit bool
}

// Run registers capabilities (full detection at startup), keeps the registration alive and
// re-detects hourly or after a failed job (see tick), and processes jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	_, err := w.RunMode(ctx, Mode{})
	return err
}

// RunMode is Run with an exit policy (see Mode). It returns an error only when the worker cannot
// work at all: capability detection failed, or (in an ephemeral mode) every claim attempt of the
// idle period failed, for example because the database is unreachable.
func (w *Worker) RunMode(ctx context.Context, m Mode) (Summary, error) {
	var sum Summary
	if m.Once && m.IdleExit <= 0 {
		return sum, errors.New("once mode needs a positive idle exit")
	}
	if err := w.RegisterCapabilities(ctx); err != nil {
		return sum, fmt.Errorf("detecting encoders: %w", err)
	}
	tickCtx, stopTick := context.WithCancel(ctx)
	tickDone := make(chan struct{})
	defer func() { stopTick(); <-tickDone }()
	go func() {
		defer close(tickDone)
		t := time.NewTicker(capabilityTick)
		defer t.Stop()
		for {
			select {
			case <-tickCtx.Done():
				return
			case <-t.C:
				w.tick(tickCtx)
			}
		}
	}()
	idleSince := time.Now()
	var claimErr error // the last claim error; reset by any successful claim query
	for ctx.Err() == nil {
		claimed, oc, err := w.processOne(ctx)
		if err != nil {
			claimErr = err
			slog.Error("process", "err", err)
		} else {
			claimErr = nil
		}
		if claimed {
			sum.Jobs++
			sum.Finished = oc == outcomeFinished
			if oc == outcomeInterrupted {
				sum.Interrupted = true
				return sum, nil
			}
			if m.Once {
				return sum, nil
			}
			idleSince = time.Now()
			continue
		}
		wait := w.Poll
		if m.IdleExit > 0 {
			left := m.IdleExit - time.Since(idleSince)
			if left <= 0 {
				sum.IdleExit = true
				if claimErr != nil {
					return sum, fmt.Errorf("claiming jobs: %w", claimErr)
				}
				return sum, nil
			}
			wait = min(wait, left)
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	return sum, nil
}

// ProcessOne claims and processes at most one job. It reports whether a job was claimed.
func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	ok, _, err := w.processOne(ctx)
	return ok, err
}

func (w *Worker) processOne(ctx context.Context) (bool, outcome, error) {
	j, err := w.Store.Claim(ctx, w.ID, w.claimable())
	if err != nil {
		return false, 0, err
	}
	if j == nil {
		return false, 0, nil
	}
	return true, w.process(ctx, j), nil
}

// outcome is how one claimed job ended for this worker.
type outcome int

const (
	outcomeFinished    outcome = iota + 1 // terminal write made: done, failed or canceled
	outcomeReleased                       // released for retry (storage failure not the job's fault)
	outcomeInterrupted                    // shutdown while the job was active: released for retry
	outcomeAbandoned                      // lease lost, or the terminal write failed
)

// jobState carries the out-of-band signals that classify how a run ended.
type jobState struct {
	canceled atomic.Bool // the user requested cancellation
	lost     atomic.Bool // the store no longer lets this worker touch the job
}

func (w *Worker) process(ctx context.Context, j *jobs.Job) outcome {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var st jobState
	watchDone := make(chan struct{})
	go func() { w.watch(jobCtx, j.ID, cancel, &st); close(watchDone) }()

	w.Audit.System("job_started", j.ID, "worker", w.ID, "attempt", j.Attempts)
	var out output
	transient := false // a storage failure that is not the job's fault: retry instead of failing
	inPath, releaseIn, err := w.Blob.LocalInput(jobCtx, j.InputPath, w.ScratchDir)
	if err != nil {
		if jobCtx.Err() == nil {
			slog.Error("input unavailable", "job_id", j.ID, "worker", w.ID, "err", err)
			transient = transientStorageErr(err)
		}
		if errors.Is(err, blob.ErrTooLarge) {
			err = errInputTooLarge
		} else {
			err = errInternal
		}
	} else {
		defer releaseIn()
		out, err = w.run(jobCtx, cancel, &st, j, inPath)
	}
	// Publish the finished output while the heartbeat still runs (an upload to an object store
	// can take longer than STALE_AFTER); for the local store this is a no-op. A cancel request, a
	// lost lease or a shutdown cancels jobCtx and so the upload.
	var outLoc string
	committed := false
	if err == nil && jobCtx.Err() == nil {
		cctx, ccancel := context.WithTimeout(jobCtx, w.jobTimeout())
		outLoc, err = out.commit(cctx)
		ccancel()
		if err != nil {
			if jobCtx.Err() == nil {
				slog.Error("publishing output failed", "job_id", j.ID, "worker", w.ID, "err", err)
				transient = transientStorageErr(err)
			}
			err = errInternal
		} else {
			committed = true
		}
	}
	cancel()
	<-watchDone
	// From here on the heartbeat is stopped and every write is fenced by worker id and status, so
	// a job that was requeued in the meantime is never touched.

	// The post-run writes are bounded so a shutdown finishes within a container grace period.
	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), w.shutdownTimeout())
	defer bgCancel()
	// dropOutput removes this attempt's output: the local file and, once committed, the stored
	// object (per-attempt names: no other attempt's output is ever touched).
	dropOutput := func() {
		removeIf(out.path)
		if outLoc != "" {
			_ = w.Blob.Remove(bg, outLoc)
		}
	}
	if st.lost.Load() { // requeued, canceled or deleted behind our back: abort quietly
		dropOutput()
		w.Audit.System("job_lease_lost", j.ID, "worker", w.ID)
		return outcomeAbandoned
	}
	// A shutdown that cut the job short (anything but a finished, published output) releases it
	// for retry at once instead of leaving it to the janitor's stale-heartbeat requeue. A user
	// cancel request that arrived first still wins: that is a terminal write the user asked for.
	if ctx.Err() != nil && !st.canceled.Load() && !(err == nil && committed) {
		dropOutput()
		rerr := w.Store.ReleaseForRetry(bg, j.ID, w.ID)
		w.logStoreErr("release", j.ID, rerr)
		if rerr == nil {
			w.Audit.System("job_released", j.ID, "worker", w.ID, "reason", "shutdown")
		}
		return outcomeInterrupted
	}

	if transient && !st.canceled.Load() {
		// Release the job for retry through the stale/attempt mechanism: no terminal write, the
		// input is kept, only this attempt's output is removed.
		dropOutput()
		rerr := w.Store.ReleaseForRetry(bg, j.ID, w.ID)
		w.logStoreErr("release", j.ID, rerr)
		if rerr == nil {
			w.Audit.System("job_released", j.ID, "worker", w.ID, "reason", "storage_unavailable")
		}
		return outcomeReleased
	}

	// The input is deleted only after the fenced terminal write succeeds; if that write
	// fails (lease lost or store error) the input is kept, because the job may be requeued,
	// and only this attempt's own output is removed.
	var werr error
	switch {
	case st.canceled.Load():
		dropOutput()
		werr = w.Store.MarkCanceled(bg, j.ID, w.ID)
		w.logStoreErr("mark_canceled", j.ID, werr)
		if werr == nil {
			w.Audit.System("job_canceled", j.ID, "worker", w.ID)
		}
	case err != nil:
		removeIf(out.path)
		if !errors.Is(err, errTimeout) && !errors.Is(err, errOversize) && !errors.Is(err, errInternal) && !errors.Is(err, errInputTooLarge) && j.EncoderUsed != "" {
			w.redetect.Store(true) // FFmpeg itself failed or hung: an encoder may have stopped working
		}
		msg := ffmpeg.Sanitize(err.Error(), w.stripPaths(j.InputPath, inPath, out.path)...)
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
			dropOutput()
		}
	}
	if werr != nil {
		return outcomeAbandoned
	}
	_ = w.Blob.Remove(bg, j.InputPath)
	return outcomeFinished
}

// output is an attempt's local output file and the func that publishes it to the blob store.
type output struct {
	path   string
	commit func(context.Context) (string, error)
}

// stripPaths lists what Sanitize removes from a message stored on the job: the directory of
// every local (absolute) path, the scratch directory, and any other locator (an object key such as
// in/<id>) in full. Only directories of absolute paths are stripped: the directory of a key ("in")
// would mangle ordinary words.
func (w *Worker) stripPaths(paths ...string) []string {
	out := make([]string, 0, len(paths)+1)
	for _, p := range paths {
		switch {
		case p == "":
		case filepath.IsAbs(p):
			out = append(out, filepath.Dir(p))
		default:
			out = append(out, p)
		}
	}
	if w.ScratchDir != "" {
		out = append(out, w.ScratchDir)
	}
	return out
}

// transientStorageErr reports whether a blob store error may go away on retry. A missing input,
// a locator the store does not own and an oversized input are permanent.
func transientStorageErr(err error) bool {
	return !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, blob.ErrInvalidLocator) && !errors.Is(err, blob.ErrTooLarge)
}

const defaultShutdownTimeout = 10 * time.Second

func (w *Worker) shutdownTimeout() time.Duration {
	if w.ShutdownTimeout <= 0 {
		return defaultShutdownTimeout
	}
	return w.ShutdownTimeout
}

func (w *Worker) jobTimeout() time.Duration {
	if w.JobTimeout <= 0 {
		return defaultJobTimeout
	}
	return w.JobTimeout
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

	timeout := w.jobTimeout()
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
	tail, err := ffmpeg.RunWith(runCtx, w.FFmpegBin, args, w.ScratchDir, ffmpeg.RunOpts{DurationSec: dur, StallTimeout: w.StallTimeout, OnProgress: func(p float64) {
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
	}})
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
	case errors.Is(err, ffmpeg.ErrStalled):
		// The stderr tail goes to the operator log only (sanitized like a stored message); the
		// user sees the fixed stall message.
		slog.Warn("ffmpeg stalled; job failed", "job_id", j.ID, "worker", w.ID, "stall_timeout", w.StallTimeout,
			"stderr_tail", ffmpeg.Sanitize(tail, w.stripPaths(j.InputPath, inPath, outPath)...))
		return out, errStalled(w.StallTimeout)
	case err != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return out, errTimeout
	case err != nil:
		return out, fmt.Errorf("transcode failed: %s", ffmpeg.Sanitize(tail, w.stripPaths(j.InputPath, inPath, outPath)...))
	}
	return out, nil
}
