package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db"
	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/worker"
	"github.com/google/uuid"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Getenv, os.ReadFile)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	outDir := filepath.Join(cfg.DataDir, "out")
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "cf-scratch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	host, _ := os.Hostname()
	w := &worker.Worker{
		ID: fmt.Sprintf("%s-%s", host, uuid.NewString()[:8]), Store: jobs.NewStore(pool), Audit: audit.New(os.Stdout),
		FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", Flavor: cfg.Flavor, VAAPIDevice: cfg.VAAPIDevice,
		OutDir: outDir, ScratchDir: scratch,
		Limits: ffmpeg.Limits{MaxDurationSec: cfg.MaxDurationSec, MaxPixels: cfg.MaxPixels, MaxStreams: cfg.MaxStreams},
		Poll:   cfg.WorkerPoll, HeartbeatEvery: 2 * time.Second,
		JobTimeout: cfg.JobTimeout, MaxOutputBytes: cfg.MaxOutputBytes,
	}
	slog.Info("worker starting", "id", w.ID, "flavor", cfg.Flavor)
	return w.Run(ctx)
}
