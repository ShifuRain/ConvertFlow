package worker

import (
	"context"

	"github.com/ShifuRain/convertflow/internal/jobs"
)

// ProcessClaimed runs an already claimed job, bypassing the capability filter of Claim.
func (w *Worker) ProcessClaimed(ctx context.Context, j *jobs.Job) { w.process(ctx, j) }
