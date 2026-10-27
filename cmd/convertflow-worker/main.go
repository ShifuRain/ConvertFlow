package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db"
	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/worker"
	"github.com/google/uuid"
)

// Exit codes (documented in docs/on-demand.md).
const (
	// exitOK: a job was processed to a terminal state (done, failed or canceled: those are job
	// results, not worker failures), or the worker exited idle, or an always-on worker stopped.
	exitOK = 0
	// exitFatal: configuration error or infrastructure the worker cannot work without (database
	// unreachable, encoder detection failed).
	exitFatal = 1
	// exitTempFail (EX_TEMPFAIL): an ephemeral worker stopped before its job reached a terminal
	// state, usually because it was signalled (scale-in, spot preemption); the job was released for
	// retry. A Kubernetes podFailurePolicy can treat it as ignorable.
	exitTempFail = 75
)

// hardStopSlack is added to the shutdown timeout before a stuck shutdown is cut short.
const hardStopSlack = 5 * time.Second

func main() {
	os.Exit(realMain(os.Args[1:], os.Getenv, os.Stderr))
}

// workerMode merges the flags over the environment configuration.
func workerMode(cfg config.Config, args []string, stderr io.Writer) (worker.Mode, error) {
	fs := flag.NewFlagSet("convertflow-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	once := fs.Bool("once", cfg.WorkerOnce, "claim at most one job, run it to its end and exit (env CONVERTFLOW_WORKER_ONCE)")
	idle := fs.Duration("idle-exit", cfg.WorkerIdleExit, "exit after this long without a claimable job; once mode defaults to 30s (env CONVERTFLOW_WORKER_IDLE_EXIT)")
	if err := fs.Parse(args); err != nil {
		return worker.Mode{}, err
	}
	if fs.NArg() > 0 {
		return worker.Mode{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	idleSet := cfg.WorkerIdleExit > 0
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "idle-exit" {
			idleSet = true
		}
	})
	if idleSet && *idle <= 0 {
		return worker.Mode{}, fmt.Errorf("--idle-exit must be a positive duration, got %s", *idle)
	}
	m := worker.Mode{Once: *once, IdleExit: *idle}
	if m.Once && m.IdleExit <= 0 {
		m.IdleExit = config.DefaultOnceIdleExit
	}
	return m, nil
}

// exitCode maps how the worker ended to the process exit status.
func exitCode(m worker.Mode, s worker.Summary, err error) int {
	switch {
	case err != nil:
		return exitFatal
	case !m.Ephemeral():
		return exitOK // an always-on worker stopped by a signal is a normal stop, as before
	case s.Interrupted, m.Once && s.Jobs > 0 && !s.Finished:
		return exitTempFail
	}
	return exitOK
}

func realMain(args []string, getenv func(string) string, stderr io.Writer) int {
	cfg, err := config.Load(getenv, os.ReadFile)
	if err == nil {
		err = cfg.Absolutize()
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		return exitFatal
	}
	m, err := workerMode(cfg, args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		return exitFatal
	}
	// The scratch dir holds everything this process writes besides the stored objects (S3 input
	// downloads, outputs before upload, FFmpeg's working directory). It is removed on every exit
	// path, including the hard stop below.
	scratch, err := os.MkdirTemp("", "cf-scratch-")
	if err == nil {
		scratch, err = filepath.Abs(scratch)
	}
	if err != nil {
		slog.Error("fatal", "err", err)
		return exitFatal
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Backstop for the grace period: the worker bounds its own shutdown by
	// CONVERTFLOW_WORKER_SHUTDOWN_TIMEOUT; should anything still hang, exit anyway (the job's
	// heartbeat stops, so the janitor requeues it).
	go func() {
		<-ctx.Done()
		time.Sleep(cfg.WorkerShutdownTimeout + hardStopSlack)
		slog.Error("shutdown did not finish in time; exiting", "timeout", cfg.WorkerShutdownTimeout)
		_ = os.RemoveAll(scratch)
		code := exitOK
		if m.Ephemeral() {
			code = exitTempFail
		}
		os.Exit(code)
	}()

	s, err := run(ctx, cfg, m, scratch)
	if err != nil {
		slog.Error("fatal", "err", err)
	}
	code := exitCode(m, s, err)
	slog.Info("worker exiting", "jobs", s.Jobs, "finished", s.Finished, "interrupted", s.Interrupted, "idle_exit", s.IdleExit, "code", code)
	return code
}

func run(ctx context.Context, cfg config.Config, m worker.Mode, scratch string) (worker.Summary, error) {
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return worker.Summary{}, err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return worker.Summary{}, err
	}
	// local: ensures in/ and out/ exist (0700). s3: inputs are downloaded to and outputs written in
	// the scratch dir; an input larger than the upload limit is refused.
	bs, err := blob.Open(ctx, cfg, scratch, cfg.MaxUploadBytes)
	if err != nil {
		return worker.Summary{}, err
	}
	host, _ := os.Hostname()
	w := &worker.Worker{
		ID: fmt.Sprintf("%s-%s", host, uuid.NewString()[:8]), Store: jobs.NewStore(pool), Audit: audit.New(os.Stdout),
		FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", Flavor: cfg.Flavor, VAAPIDevice: cfg.VAAPIDevice,
		Blob: bs, ScratchDir: scratch,
		Limits: ffmpeg.Limits{MaxDurationSec: cfg.MaxDurationSec, MaxPixels: cfg.MaxPixels, MaxStreams: cfg.MaxStreams},
		Poll:   cfg.WorkerPoll, HeartbeatEvery: 2 * time.Second,
		LeaseTimeout: cfg.StaleAfter,
		JobTimeout:   cfg.JobTimeout, MaxOutputBytes: cfg.MaxOutputBytes,
		StallTimeout: cfg.JobStallTimeout, ShutdownTimeout: cfg.WorkerShutdownTimeout,
	}
	slog.Info("worker starting", "id", w.ID, "flavor", cfg.Flavor, "storage", cfg.Storage,
		"once", m.Once, "idle_exit", m.IdleExit, "stall_timeout", cfg.JobStallTimeout)
	return w.RunMode(ctx, m)
}
