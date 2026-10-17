package worker

import (
	"context"

	"github.com/ShifuRain/convertflow/internal/jobs"
)

// ProcessClaimed runs an already claimed job, bypassing the capability filter of Claim.
func (w *Worker) ProcessClaimed(ctx context.Context, j *jobs.Job) { w.process(ctx, j) }

// Tick runs one capability tick.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

// FlagRedetect simulates a job that failed inside FFmpeg.
func (w *Worker) FlagRedetect() { w.redetect.Store(true) }
