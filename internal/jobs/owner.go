package jobs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Scope restricts which jobs a caller may see and change. The zero Scope matches nothing, so a
// caller that forgot to resolve its owner fails closed.
type Scope struct {
	all   bool
	owner []byte
}

// AllJobs is the scope with sessions off: every job.
func AllJobs() Scope { return Scope{all: true} }

// OwnedBy matches the jobs whose owner key equals key. A nil key matches nothing.
func OwnedBy(key []byte) Scope { return Scope{owner: key} }

// scopeSQL is the predicate the scoped queries append; $2 is sc.all, $3 sc.owner (NULL never
// equals anything).
const scopeSQL = ` AND ($2::boolean OR owner = $3::bytea)`

// GetFor is Get restricted to a scope: a job outside it is ErrNotFound, exactly like a missing one.
func (s *Store) GetFor(ctx context.Context, id string, sc Scope) (*Job, error) {
	return scanJob(s.pool.QueryRow(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = $1::uuid`+scopeSQL, id, sc.all, sc.owner))
}

// ListFor lists the newest jobs in scope.
func (s *Store) ListFor(ctx context.Context, sc Scope, limit int) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+` FROM jobs WHERE true`+scopeSQL+` ORDER BY created_at DESC LIMIT $1`,
		limit, sc.all, sc.owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// DeleteFor is Delete restricted to a scope (ErrNotFound for a job outside it, ErrBusy only for
// an active job inside it).
func (s *Store) DeleteFor(ctx context.Context, id string, sc Scope) (*Job, error) {
	j, err := scanJob(s.pool.QueryRow(ctx,
		`DELETE FROM jobs WHERE id = $1::uuid AND status IN ('uploading','queued','done','failed','canceled')`+scopeSQL+` RETURNING `+jobCols,
		id, sc.all, sc.owner))
	if errors.Is(err, ErrNotFound) {
		if _, gerr := s.GetFor(ctx, id, sc); gerr == nil {
			return nil, ErrBusy
		}
		return nil, ErrNotFound
	}
	return j, err
}

// RequestCancelFor is RequestCancel restricted to a scope.
func (s *Store) RequestCancelFor(ctx context.Context, id string, sc Scope) error {
	return s.exec(ctx, `
UPDATE jobs SET cancel_requested = true, updated_at = now(),
  status = CASE WHEN status = 'queued' THEN 'canceled' ELSE status END,
  finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END
WHERE id = $1::uuid AND status IN ('queued','probing','running')`+scopeSQL, id, sc.all, sc.owner)
}

// Limits are the quotas applied when a job is created. Zero means unlimited.
type Limits struct {
	ActiveJobs      int   // per owner
	QueuedBytes     int64 // per owner
	JobsPerHour     int   // per owner
	ActiveJobsPerIP int   // per client key
}

// Quota kinds (QuotaError.Kind).
const (
	QuotaActiveJobs      = "active_jobs"
	QuotaQueuedBytes     = "queued_bytes"
	QuotaInputTooLarge   = "input_larger_than_quota" // the one input exceeds the queued-bytes quota
	QuotaJobsPerHour     = "jobs_per_hour"
	QuotaActiveJobsPerIP = "active_jobs_per_ip"
	QuotaSessionsPerHour = "sessions_per_hour"
)

// QuotaError reports an exceeded quota. RetryAfter is when retrying may succeed (for the hourly
// limits: when the oldest counted event leaves the window).
type QuotaError struct {
	Kind       string
	RetryAfter time.Duration
}

func (e *QuotaError) Error() string { return "quota exceeded: " + e.Kind }

// Usage is the current consumption counted by the quotas.
type Usage struct {
	ActiveJobs   int
	QueuedBytes  int64
	JobsLastHour int
	// HourResetIn is how long until the oldest job of the trailing hour leaves the window (0
	// when none).
	HourResetIn     time.Duration
	ActiveJobsPerIP int
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const activeStatuses = `('uploading','queued','probing','running')`

// QuotaUsage counts an owner's and a client key's current usage (nil keys count as zero).
func (s *Store) QuotaUsage(ctx context.Context, owner, clientKey []byte) (Usage, error) {
	return usage(ctx, s.pool, owner, clientKey)
}

func usage(ctx context.Context, q querier, owner, clientKey []byte) (Usage, error) {
	var u Usage
	var reset float64
	err := q.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM jobs WHERE $1::bytea IS NOT NULL AND owner = $1 AND status IN `+activeStatuses+`),
  (SELECT coalesce(sum(coalesce(input_size, declared_size, 0)), 0)::bigint FROM jobs
     WHERE $1::bytea IS NOT NULL AND owner = $1 AND status IN `+activeStatuses+`),
  (SELECT count(*) FROM rate_events WHERE $1::bytea IS NOT NULL AND key = $1 AND at > now() - interval '1 hour'),
  (SELECT coalesce(extract(epoch FROM min(at) + interval '1 hour' - now()), 0)::float8 FROM rate_events
     WHERE $1::bytea IS NOT NULL AND key = $1 AND at > now() - interval '1 hour'),
  (SELECT count(*) FROM jobs WHERE $2::bytea IS NOT NULL AND client_key = $2 AND status IN `+activeStatuses+`)`,
		owner, clientKey).Scan(&u.ActiveJobs, &u.QueuedBytes, &u.JobsLastHour, &reset, &u.ActiveJobsPerIP)
	if reset > 0 {
		u.HourResetIn = time.Duration(math.Ceil(reset)) * time.Second
	}
	return u, err
}

// Check returns the first quota a new job of addBytes input bytes would exceed, or nil.
// The owner limits apply only when hasOwner, the per-IP limit only when hasIP.
func (l Limits) Check(u Usage, addBytes int64, hasOwner, hasIP bool) *QuotaError {
	if hasOwner {
		if l.QueuedBytes > 0 && addBytes > l.QueuedBytes {
			return &QuotaError{Kind: QuotaInputTooLarge}
		}
		if l.JobsPerHour > 0 && u.JobsLastHour >= l.JobsPerHour {
			return &QuotaError{Kind: QuotaJobsPerHour, RetryAfter: max(u.HourResetIn, time.Second)}
		}
		if l.ActiveJobs > 0 && u.ActiveJobs >= l.ActiveJobs {
			return &QuotaError{Kind: QuotaActiveJobs, RetryAfter: activeRetry}
		}
		if l.QueuedBytes > 0 && addBytes > 0 && u.QueuedBytes+addBytes > l.QueuedBytes {
			return &QuotaError{Kind: QuotaQueuedBytes, RetryAfter: activeRetry}
		}
	}
	if hasIP && l.ActiveJobsPerIP > 0 && u.ActiveJobsPerIP >= l.ActiveJobsPerIP {
		return &QuotaError{Kind: QuotaActiveJobsPerIP, RetryAfter: activeRetry}
	}
	return nil
}

// activeRetry is the Retry-After for limits on active jobs: they free up when a job finishes,
// which the server cannot predict.
const activeRetry = 60 * time.Second

func (l Limits) any() bool {
	return l.ActiveJobs > 0 || l.QueuedBytes > 0 || l.JobsPerHour > 0 || l.ActiveJobsPerIP > 0
}

// quotaLockClass is the first key of the two-int advisory locks that serialize quota checks (the
// migration lock uses the one-bigint key space, which is separate).
const quotaLockClass = 727275

// lockKeys takes a transaction-scoped advisory lock per key in a fixed order (no deadlock between
// two creators sharing an owner and an IP). Unrelated keys may share a lock; that only serializes
// them.
func lockKeys(ctx context.Context, tx pgx.Tx, keys ...[]byte) error {
	var ids []int32
	for _, k := range keys {
		if len(k) >= 4 {
			ids = append(ids, int32(binary.BigEndian.Uint32(k[:4]))) // #nosec G115 -- reinterpreting hash bits
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, quotaLockClass, id); err != nil {
			return err
		}
	}
	return nil
}

// CreateWithQuota inserts a job like Create after checking the quotas inside one transaction that
// holds an advisory lock on the job's owner and client keys: concurrent creates for the same owner
// or IP (from any API replica) are serialized, so they cannot exceed a limit. On success with an
// hourly limit set, the creation is recorded in rate_events. A *QuotaError means nothing was
// written.
func (s *Store) CreateWithQuota(ctx context.Context, j *Job, l Limits) error {
	hasOwner, hasIP := j.Owner != nil, j.ClientKey != nil
	if !l.any() || (!hasOwner && !hasIP) {
		return s.Create(ctx, j)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockKeys(ctx, tx, j.Owner, j.ClientKey); err != nil {
		return err
	}
	u, err := usage(ctx, tx, j.Owner, j.ClientKey)
	if err != nil {
		return err
	}
	add := j.InputSize
	if add <= 0 {
		add = j.DeclaredSize
	}
	if qe := l.Check(u, add, hasOwner, hasIP); qe != nil {
		return qe
	}
	if err := insertJob(ctx, tx, j); err != nil {
		return err
	}
	if hasOwner && l.JobsPerHour > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO rate_events (key) VALUES ($1)`, j.Owner); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ErrNoSession means the session key is unknown or idle for longer than its lifetime.
var ErrNoSession = errors.New("no such session")

// CreateSession stores a new session key. With perHour > 0 at most perHour sessions may be created
// per mintKey (the client IP's rate key) in the trailing hour; beyond that a *QuotaError is
// returned and nothing is written. The check is serialized per mintKey like CreateWithQuota.
func (s *Store) CreateSession(ctx context.Context, key, mintKey []byte, perHour int) error {
	if len(key) != 32 {
		return fmt.Errorf("session key must be 32 bytes")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if perHour > 0 {
		if err := lockKeys(ctx, tx, mintKey); err != nil {
			return err
		}
		var n int
		var reset float64
		if err := tx.QueryRow(ctx, `SELECT count(*), coalesce(extract(epoch FROM min(at) + interval '1 hour' - now()), 0)::float8
FROM rate_events WHERE key = $1 AND at > now() - interval '1 hour'`, mintKey).Scan(&n, &reset); err != nil {
			return err
		}
		if n >= perHour {
			return &QuotaError{Kind: QuotaSessionsPerHour, RetryAfter: max(time.Duration(math.Ceil(reset))*time.Second, time.Second)}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO rate_events (key) VALUES ($1)`, mintKey); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sessions (key) VALUES ($1)`, key); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// sessionRefresh is how stale last_seen may get before a request refreshes it (one write per
// session per interval instead of one per request).
const sessionRefresh = time.Hour

// TouchSession checks that a session exists and was used within ttl. It refreshes last_seen at
// most once per sessionRefresh and reports whether it did (the caller then renews the cookie's
// lifetime). ErrNoSession for an unknown or expired key.
func (s *Store) TouchSession(ctx context.Context, key []byte, ttl time.Duration) (refreshed bool, err error) {
	var stale bool
	err = s.pool.QueryRow(ctx, `SELECT last_seen < now() - make_interval(secs => $3)
FROM sessions WHERE key = $1 AND last_seen > now() - make_interval(secs => $2)`, key, ttl.Seconds(), sessionRefresh.Seconds()).Scan(&stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNoSession
	}
	if err != nil || !stale {
		return false, err
	}
	_, err = s.pool.Exec(ctx, `UPDATE sessions SET last_seen = now() WHERE key = $1`, key)
	return err == nil, err
}

// DeleteIdleSessions removes sessions not used for longer than ttl and returns how many.
func (s *Store) DeleteIdleSessions(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE last_seen < now() - make_interval(secs => $1)`, ttl.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PruneRateEvents removes rate-limit records that left the trailing hour.
func (s *Store) PruneRateEvents(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM rate_events WHERE at < now() - interval '1 hour'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
