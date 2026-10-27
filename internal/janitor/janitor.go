// Package janitor enforces retention, requeues jobs of dead workers and removes orphaned files.
package janitor

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
	"github.com/google/uuid"
)

const (
	// DefaultOrphanGrace is how old (by mtime) a file must be before the orphan sweep may touch
	// it. An upload or an FFmpeg run keeps modifying its file, so a file that has not been written
	// for this long is not in flight; the API aborts uploads after a short idle limit, so 6h is
	// very conservative.
	DefaultOrphanGrace = 6 * time.Hour
	// DefaultJobTimeout mirrors the worker's default wall-clock limit for one run.
	DefaultJobTimeout = 4 * time.Hour
	// activeMargin is added to JobTimeout before a probing/running job's files are considered dead.
	activeMargin = 30 * time.Minute
	// UploadGrace is added to UploadTTL before an uploading job is failed: an object store checks
	// the form's expiry when an upload starts, so a large upload begun just before expiry may still
	// be arriving afterwards.
	UploadGrace = time.Hour
	// DefaultLiveWindow is the minimum window in which a worker counts as live for queue expiry.
	DefaultLiveWindow = 2 * time.Minute
	// DefaultWorkerRetention keeps worker registrations for a week after their last refresh; it
	// matches config.OnDemandCapabilityWindow, the window the API offers presets from when workers
	// run on demand.
	DefaultWorkerRetention = 7 * 24 * time.Hour
)

type Janitor struct {
	Store       *jobs.Store
	Audit       *audit.Logger
	Blob        blob.Store // where job inputs and outputs live
	Retention   time.Duration
	StaleAfter  time.Duration
	Interval    time.Duration
	MaxAttempts int
	// OrphanGrace is the minimum file age (mtime) before an unreferenced or superseded file is
	// removed. Default DefaultOrphanGrace.
	OrphanGrace time.Duration
	// JobTimeout is the worker's run limit; files of probing/running jobs are never removed
	// until older than JobTimeout plus a margin. Default DefaultJobTimeout.
	JobTimeout time.Duration
	// MaxQueueAge fails jobs that stayed queued this long (for example because no live worker
	// offers their preset) and removes their input. Zero disables the check.
	MaxQueueAge time.Duration
	// Flavor and LiveWindow decide which presets a live worker can run (see presets.Runnable);
	// queued jobs of such presets never expire by age. LiveWindow defaults to
	// max(DefaultLiveWindow, 2*StaleAfter), so a short gap in a worker's registration refreshes
	// does not count as "no live worker" (issue #9).
	Flavor     string
	LiveWindow time.Duration
	// OnDemand declares that workers are started on demand (scale to zero): there is no live
	// worker most of the time, so queued jobs expire purely by age (MaxQueueAge) instead of only
	// when no live worker can run their preset.
	OnDemand bool
	// WorkerRetention is how long a worker registration is kept after its last refresh before it
	// is deleted. Default DefaultWorkerRetention.
	WorkerRetention time.Duration
	// UploadTTL is the lifetime of presigned upload forms (S3 storage). Jobs still 'uploading'
	// UploadTTL+UploadGrace after they were created are failed and whatever was uploaded is
	// removed. Zero disables the check (local storage has no uploading jobs).
	UploadTTL time.Duration
}

func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := j.Sweep(ctx); err != nil && ctx.Err() == nil {
			slog.Error("janitor sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (j *Janitor) grace() time.Duration {
	if j.OrphanGrace > 0 {
		return j.OrphanGrace
	}
	return DefaultOrphanGrace
}

func (j *Janitor) liveWindow() time.Duration {
	if j.LiveWindow > 0 {
		return j.LiveWindow
	}
	return max(DefaultLiveWindow, 2*j.StaleAfter)
}

func (j *Janitor) activeLimit() time.Duration {
	t := j.JobTimeout
	if t <= 0 {
		t = DefaultJobTimeout
	}
	return t + activeMargin
}

// Sweep performs one maintenance pass. Files are removed with a plain unlink; use an encrypted
// volume if remnants on the underlying storage are a concern (see docs/operations.md).
// Problems with individual files or jobs are logged and skipped; only store-level failures of
// the requeue/expiry queries are returned.
func (j *Janitor) Sweep(ctx context.Context) error {
	requeued, failed, err := j.Store.RequeueStale(ctx, j.StaleAfter, j.MaxAttempts)
	if err != nil {
		return err
	}
	if requeued+failed > 0 {
		j.Audit.System("jobs_recovered", "", "requeued", requeued, "failed", failed)
	}
	if j.MaxQueueAge > 0 {
		var stranded []jobs.Job
		if j.OnDemand {
			stranded, err = j.Store.ExpireQueuedByAge(ctx, j.MaxQueueAge)
		} else {
			var ws [][]string
			if ws, err = j.Store.LiveWorkerEncoders(ctx, j.liveWindow()); err != nil {
				return err
			}
			stranded, err = j.Store.ExpireQueued(ctx, j.MaxQueueAge, presets.IDs(presets.Runnable(ws, j.Flavor)))
		}
		if err != nil {
			return err
		}
		for _, e := range stranded {
			j.removeData(ctx, e.ID, e.InputPath)
			j.Audit.System("job_queue_expired", e.ID)
		}
	}
	if j.UploadTTL > 0 {
		abandoned, err := j.Store.ExpireUploading(ctx, j.UploadTTL+UploadGrace)
		if err != nil {
			return err
		}
		for _, e := range abandoned {
			j.removeData(ctx, e.ID, e.InputPath)
			j.Audit.System("job_upload_expired", e.ID)
		}
	}
	retention := j.WorkerRetention
	if retention <= 0 {
		retention = DefaultWorkerRetention
	}
	if _, err := j.Store.PruneWorkers(ctx, retention); err != nil {
		return err
	}
	expired, err := j.Store.Expired(ctx, j.Retention)
	if err != nil {
		return err
	}
	for _, e := range expired {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		deleted, err := j.Store.Delete(ctx, e.ID)
		if errors.Is(err, jobs.ErrBusy) || errors.Is(err, jobs.ErrNotFound) {
			continue // changed state or already gone since the query: skip
		}
		if err != nil {
			slog.Error("janitor delete expired job", "job_id", e.ID, "err", err)
			continue
		}
		j.removeData(ctx, deleted.ID, deleted.InputPath)
		j.removeData(ctx, deleted.ID, deleted.OutputPath)
		j.Audit.System("job_expired", e.ID)
	}
	for _, kind := range []blob.Kind{blob.KindIn, blob.KindOut} {
		j.sweepKind(ctx, kind)
	}
	return nil
}

// removeData deletes a file recorded on a job through the blob store, which refuses anything
// outside the data directory. A missing file is success; other failures are logged and skipped.
func (j *Janitor) removeData(ctx context.Context, id, loc string) {
	if loc == "" {
		return
	}
	err := j.Blob.Remove(ctx, loc)
	switch {
	case errors.Is(err, blob.ErrInvalidLocator):
		slog.Warn("janitor refusing to remove a locator that does not belong to the store", "job_id", id)
	case err != nil:
		slog.Error("janitor removing job file failed", "job_id", id, "err", err)
	}
}

// fileJobID extracts the job id from a data-dir file name. Inputs are named exactly <uuid>;
// outputs are <uuid>.<attempt>.<ext>. The id is the part before the first '.', and must be a
// canonical lowercase UUID (uuid.Parse alone also accepts braces, urn: and upper case forms).
func fileJobID(sub, name string) (string, bool) {
	id := name
	if sub == "out" {
		id, _, _ = strings.Cut(name, ".")
	}
	u, err := uuid.Parse(id)
	if err != nil || u.String() != id || id != name && sub == "in" {
		return "", false
	}
	return id, true
}

// sweepKind removes orphaned, superseded and finished-job files from one blob namespace
// (DataDir/in or DataDir/out for the local store).
//
// Safety rules, all of which err on the side of keeping a file:
//   - only regular files are considered (the store lists without following symlinks and skips
//     directories, symlinks and other non-regular entries); anything not named like our files is
//     ignored;
//   - a file is never touched until its mtime is older than the grace period: uploads and FFmpeg
//     keep modifying their file, and a worker's attempt output (<id>.<attempt>.<ext>) is not
//     recorded in the DB until Complete, so freshness is the only signal that it is in use;
//   - files of queued/probing/running jobs: an input is kept (it is needed to run or retry); an
//     output of a probing/running job is kept until it is older than JobTimeout+margin.
func (j *Janitor) sweepKind(ctx context.Context, kind blob.Kind) {
	sub := kind.String()
	ents, err := j.Blob.List(ctx, kind)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && ctx.Err() == nil {
			slog.Error("janitor reading data dir", "dir", sub, "err", err)
		}
		return
	}
	for _, e := range ents {
		if ctx.Err() != nil {
			return
		}
		id, ok := fileJobID(sub, e.Name)
		if !ok {
			continue // not ours
		}
		age := time.Since(e.Info.ModTime)
		if age < j.grace() {
			continue
		}
		reason, remove := j.decide(ctx, sub, e.Locator, id, age)
		if !remove {
			continue
		}
		if err := j.Blob.Remove(ctx, e.Locator); err != nil {
			slog.Error("janitor removing file failed", "job_id", id, "err", err)
			continue
		}
		j.Audit.System("orphan_removed", id, "dir", sub, "reason", reason)
	}
}

func (j *Janitor) decide(ctx context.Context, sub, loc, id string, age time.Duration) (string, bool) {
	job, err := j.Store.Get(ctx, id)
	if errors.Is(err, jobs.ErrNotFound) {
		return "no_job", true
	}
	if err != nil {
		slog.Error("janitor looking up job", "job_id", id, "err", err)
		return "", false
	}
	active := job.Status == jobs.StatusProbing || job.Status == jobs.StatusRunning
	if sub == "in" {
		if job.Status.Terminal() {
			return "job_finished", true
		}
		return "", false // uploading, queued or active: still needed
	}
	if active && age < j.activeLimit() {
		return "", false // a worker may be writing this attempt's output
	}
	if job.OutputPath != "" && filepath.Clean(job.OutputPath) == filepath.Clean(loc) {
		return "", false // the recorded output; retention removes it with the row
	}
	return "superseded", true
}
