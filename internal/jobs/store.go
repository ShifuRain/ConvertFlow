package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("job not found")
	ErrBusy     = errors.New("job is active")
)

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const jobCols = `id::text, status, filename, input_path, output_path, options, progress, encoder_used,
 probe, error, attempts, worker_id, cancel_requested, created_at, updated_at, finished_at`

func scanJob(row pgx.Row) (*Job, error) {
	var j Job
	var opts []byte
	err := row.Scan(&j.ID, &j.Status, &j.Filename, &j.InputPath, &j.OutputPath, &opts, &j.Progress,
		&j.EncoderUsed, &j.Probe, &j.Error, &j.Attempts, &j.WorkerID, &j.CancelRequested,
		&j.CreatedAt, &j.UpdatedAt, &j.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(opts, &j.Options); err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *Store) Create(ctx context.Context, j *Job) error {
	opts, err := json.Marshal(j.Options)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO jobs (id, status, filename, input_path, options) VALUES ($1::uuid, 'queued', $2, $3, $4)`,
		j.ID, j.Filename, j.InputPath, opts)
	return err
}

func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1::uuid`, id))
}

func (s *Store) List(ctx context.Context, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+` FROM jobs ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

func collect(rows pgx.Rows) ([]Job, error) {
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *Store) Claim(ctx context.Context, workerID string) (*Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx, `
UPDATE jobs SET status = 'probing', worker_id = $1, attempts = attempts + 1,
  heartbeat_at = now(), updated_at = now()
WHERE id = (SELECT id FROM jobs WHERE status = 'queued' ORDER BY created_at
            FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING `+jobCols, workerID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return j, err
}

func (s *Store) exec(ctx context.Context, q string, args ...any) error {
	_, err := s.pool.Exec(ctx, q, args...)
	return err
}

// guarded runs a transition and returns ErrNotFound when no row matched, i.e. the job
// is gone or no longer in a state that allows it (for workers: the lease was lost).
func (s *Store) guarded(ctx context.Context, q string, args ...any) error {
	tag, err := s.pool.Exec(ctx, q, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetProbe(ctx context.Context, id string, probe []byte) error {
	return s.guarded(ctx, `UPDATE jobs SET probe = $2, updated_at = now() WHERE id = $1::uuid AND status = 'probing'`, id, probe)
}

func (s *Store) SetRunning(ctx context.Context, id, encoder string) error {
	return s.guarded(ctx, `UPDATE jobs SET status = 'running', encoder_used = $2, updated_at = now() WHERE id = $1::uuid AND status = 'probing'`, id, encoder)
}

func (s *Store) UpdateProgress(ctx context.Context, id string, pct float64) error {
	return s.guarded(ctx, `UPDATE jobs SET progress = $2, updated_at = now() WHERE id = $1::uuid AND status = 'running'`, id, pct)
}

// Heartbeat refreshes the liveness timestamp and reports whether cancellation was requested.
func (s *Store) Heartbeat(ctx context.Context, id string) (bool, error) {
	var c bool
	err := s.pool.QueryRow(ctx, `UPDATE jobs SET heartbeat_at = now() WHERE id = $1::uuid AND status IN ('probing','running') RETURNING cancel_requested`, id).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return c, err
}

func (s *Store) Complete(ctx context.Context, id, outputPath string) error {
	return s.guarded(ctx, `UPDATE jobs SET status = 'done', progress = 100, output_path = $2, finished_at = now(), updated_at = now() WHERE id = $1::uuid AND status = 'running'`, id, outputPath)
}

func (s *Store) Fail(ctx context.Context, id, msg string) error {
	return s.guarded(ctx, `UPDATE jobs SET status = 'failed', error = $2, finished_at = now(), updated_at = now() WHERE id = $1::uuid AND status IN ('probing','running')`, id, msg)
}

func (s *Store) MarkCanceled(ctx context.Context, id string) error {
	return s.guarded(ctx, `UPDATE jobs SET status = 'canceled', finished_at = now(), updated_at = now() WHERE id = $1::uuid AND status IN ('queued','probing','running')`, id)
}

// RequestCancel cancels a queued job immediately and flags an active one for its worker.
func (s *Store) RequestCancel(ctx context.Context, id string) error {
	return s.exec(ctx, `
UPDATE jobs SET cancel_requested = true, updated_at = now(),
  status = CASE WHEN status = 'queued' THEN 'canceled' ELSE status END,
  finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END
WHERE id = $1::uuid AND status IN ('queued','probing','running')`, id)
}

// Delete removes an idle (queued or terminal) job and returns it so callers can remove its files.
func (s *Store) Delete(ctx context.Context, id string) (*Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx,
		`DELETE FROM jobs WHERE id = $1::uuid AND status IN ('queued','done','failed','canceled') RETURNING `+jobCols, id))
	if errors.Is(err, ErrNotFound) {
		if _, gerr := s.Get(ctx, id); gerr == nil {
			return nil, ErrBusy
		}
		return nil, ErrNotFound
	}
	return j, err
}

// RequeueStale handles jobs whose worker stopped heartbeating. Stale jobs with a pending
// cancel request become canceled; they are counted in neither requeued nor failed.
func (s *Store) RequeueStale(ctx context.Context, staleAfter time.Duration, maxAttempts int) (int, int, error) {
	var requeued, failed int
	err := s.pool.QueryRow(ctx, `
WITH s AS (
  UPDATE jobs SET
    status = CASE WHEN cancel_requested THEN 'canceled' WHEN attempts < $2 THEN 'queued' ELSE 'failed' END,
    error = CASE WHEN cancel_requested OR attempts < $2 THEN error ELSE 'worker lost; attempts exhausted' END,
    finished_at = CASE WHEN cancel_requested OR attempts >= $2 THEN now() ELSE NULL END,
    progress = 0, worker_id = '', updated_at = now()
  WHERE status IN ('probing','running') AND heartbeat_at < now() - make_interval(secs => $1)
  RETURNING status)
SELECT count(*) FILTER (WHERE status = 'queued'), count(*) FILTER (WHERE status = 'failed') FROM s`,
		staleAfter.Seconds(), maxAttempts).Scan(&requeued, &failed)
	return requeued, failed, err
}

func (s *Store) Expired(ctx context.Context, retention time.Duration) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+` FROM jobs
WHERE status IN ('done','failed','canceled') AND finished_at < now() - make_interval(secs => $1)`, retention.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

func (s *Store) Exists(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1::uuid)`, id).Scan(&ok)
	return ok, err
}

func (s *Store) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// CountActive returns the number of jobs that are queued, probing or running.
func (s *Store) CountActive(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status IN ('queued','probing','running')`).Scan(&n)
	return n, err
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) UpsertWorker(ctx context.Context, id string, encoders []string) error {
	if encoders == nil {
		encoders = []string{}
	}
	return s.exec(ctx, `INSERT INTO workers (id, encoders, seen_at) VALUES ($1, $2, now())
ON CONFLICT (id) DO UPDATE SET encoders = EXCLUDED.encoders, seen_at = now()`, id, encoders)
}

func (s *Store) LiveEncoders(ctx context.Context, within time.Duration) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT unnest(encoders) FROM workers WHERE seen_at > now() - make_interval(secs => $1)`, within.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out[e] = true
	}
	return out, rows.Err()
}

func (s *Store) LiveWorkers(ctx context.Context, within time.Duration) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM workers WHERE seen_at > now() - make_interval(secs => $1)`, within.Seconds()).Scan(&n)
	return n, err
}
