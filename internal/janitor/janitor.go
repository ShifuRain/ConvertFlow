// Package janitor enforces retention, requeues jobs of dead workers and removes orphaned files.
package janitor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/jobs"
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
)

type Janitor struct {
	Store       *jobs.Store
	Audit       *audit.Logger
	DataDir     string
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
		j.removeData(deleted.ID, deleted.InputPath)
		j.removeData(deleted.ID, deleted.OutputPath)
		j.Audit.System("job_expired", e.ID)
	}
	for _, sub := range []string{"in", "out"} {
		j.sweepDir(ctx, sub)
	}
	return nil
}

// removeData deletes a file recorded on a job, but only strictly inside the data directory.
// A missing file is success; other failures are logged and skipped.
func (j *Janitor) removeData(id, p string) {
	if p == "" {
		return
	}
	root, err1 := filepath.Abs(j.DataDir)
	abs, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return
	}
	if rel, err := filepath.Rel(root, abs); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		slog.Warn("janitor refusing to remove path outside the data dir", "job_id", id)
		return
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
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

// sweepDir removes orphaned, superseded and finished-job files from DataDir/<sub>.
//
// Safety rules, all of which err on the side of keeping a file:
//   - only regular files are considered (entries come from ReadDir, which does not follow
//     symlinks); directories, symlinks and anything not named like our files are ignored;
//   - a file is never touched until its mtime is older than the grace period: uploads and FFmpeg
//     keep modifying their file, and a worker's attempt output (<id>.<attempt>.<ext>) is not
//     recorded in the DB until Complete, so freshness is the only signal that it is in use;
//   - files of queued/probing/running jobs: an input is kept (it is needed to run or retry); an
//     output of a probing/running job is kept until it is older than JobTimeout+margin.
func (j *Janitor) sweepDir(ctx context.Context, sub string) {
	dir := filepath.Join(j.DataDir, sub)
	ents, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Error("janitor reading data dir", "dir", sub, "err", err)
		}
		return
	}
	for _, e := range ents {
		if ctx.Err() != nil {
			return
		}
		if !e.Type().IsRegular() { // DirEntry type comes from lstat semantics: symlinks are not regular
			continue
		}
		id, ok := fileJobID(sub, e.Name())
		if !ok {
			continue // not ours
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue // vanished or replaced concurrently
		}
		age := time.Since(info.ModTime())
		if age < j.grace() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		reason, remove := j.decide(ctx, sub, path, id, age)
		if !remove {
			continue
		}
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				slog.Error("janitor removing file failed", "job_id", id, "err", err)
			}
			continue
		}
		j.Audit.System("orphan_removed", id, "dir", sub, "reason", reason)
	}
}

func (j *Janitor) decide(ctx context.Context, sub, path, id string, age time.Duration) (string, bool) {
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
		return "", false // queued or active: still needed
	}
	if active && age < j.activeLimit() {
		return "", false // a worker may be writing this attempt's output
	}
	if job.OutputPath != "" && filepath.Clean(job.OutputPath) == filepath.Clean(path) {
		return "", false // the recorded output; retention removes it with the row
	}
	return "superseded", true
}
