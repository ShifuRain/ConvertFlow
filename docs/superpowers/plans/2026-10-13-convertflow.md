# ConvertFlow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build ConvertFlow, an air-gapped, no-auth, dark-themed web app and REST API that transcodes video/audio with FFmpeg, shipped as hardened containers.

**Architecture:** One Go module, two binaries (`convertflow-api`, `convertflow-worker`). The API serves REST, SSE and an embedded TypeScript UI, stores uploads on a shared volume and jobs in Postgres. Workers claim jobs with `FOR UPDATE SKIP LOCKED`, probe with ffprobe, build FFmpeg arguments from a fixed allow-list and report progress back to Postgres.

**Tech Stack:** Go 1.23 (stdlib `net/http`, `log/slog`), `github.com/jackc/pgx/v5` (MIT), `github.com/google/uuid` (BSD-3), Postgres 16, FFmpeg 7.x built from source (LGPL or GPL flavor), TypeScript + esbuild (build-time only), Docker/Podman Compose.

**Spec:** `docs/superpowers/specs/2026-10-13-convertflow-design.md`

## Global Constraints

- Module path: `github.com/ShifuRain/convertflow`; license Apache-2.0.
- FOSS only; Go dependencies must be MIT/BSD/Apache/ISC. No AGPL/GPL Go modules.
- No authentication in the app (accepted risk; documented, spec §1).
- Air-gapped: no CDN, web fonts, telemetry or runtime outbound calls. UI assets embedded via `go:embed`.
- Default FFmpeg flavor is `lgpl` (royalty-free codecs only); `full` (x264/x265/AAC) is opt-in via `FFMPEG_FLAVOR=full` and prints a licensing/patent notice at build time.
- User input is never passed to a shell; FFmpeg arguments come only from the allow-list in `internal/ffmpeg/args.go`.
- Errors use RFC 9457 `application/problem+json`.
- Containers: non-root, read-only rootfs, `cap_drop: ALL`, `no-new-privileges`, CPU/memory/pids limits, pinned image digests; worker network is `internal: true`.
- Default retention 24 h; all limits configurable via `CONVERTFLOW_*` env vars.
- Every commit message ends with the trailer `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>` (shown as `-m "<trailer>"` below as a second `-m`; always include it).
- DB-backed tests read `TEST_DATABASE_URL` and skip when unset; FFmpeg-backed tests skip when `ffmpeg`/`ffprobe` are missing. Run the full suite with `scripts/test-db.sh`.
- Hardware AV1 encoders (`av1_nvenc`, `av1_qsv`, `av1_vaapi`) are supported at the argument-builder and detection level only. The default image does not compile them in, so they stay inert until an operator supplies a hardware-enabled FFmpeg build (documented in Task 14).

## Review Focus

Failure modes the spec implies but a straightforward implementation would miss. Each has a pinned test in the task named.

1. A text/garbage file named `x.mp4`, or a zero-byte file, must produce a `failed` job with a short message that contains no filesystem path (Task 5 `TestProbeRejectsGarbage`, Task 6 `TestWorkerFailsOnGarbage`, Task 7 `TestUploadRejectsEmptyFile`).
2. A playlist/concat file (`ffconcat`, `.m3u8`) pointing at local files or URLs must be rejected, not followed (Task 5 `TestProbeRejectsConcat`, Task 13).
3. Hostile or odd filenames (`../../etc/passwd`, newlines, quotes, unicode, 5 KB long, empty) must never escape the data dir and must produce a safe `Content-Disposition` (Task 7 `TestSanitizeFilename`, Task 8 `TestOutputDisposition`).
4. An upload that exceeds the limit or is aborted mid-stream must return 413/400 and leave no partial file behind (Task 7 `TestUploadTooLarge`, `TestUploadAborted`).
5. Audio-only input with a video preset (and an MP3 with cover art sent to an audio preset) must fail/succeed predictably: `422`-style job failure "input has no video stream" and a successful audio conversion that ignores the cover image (Task 4 `TestBuildArgsNeedsVideo`, `TestBuildArgsIgnoresCoverArt`, Task 6 `TestWorkerFailsOnAudioOnlyForVideoPreset`).

---

## File Structure

```
go.mod / go.sum / Makefile / .gitignore / LICENSE
cmd/convertflow-api/main.go            # wiring, healthcheck subcommand
cmd/convertflow-worker/main.go         # wiring
internal/config/config.go              # env parsing
internal/audit/audit.go                # structured audit log
internal/db/db.go, migrate.go, migrations/001_init.sql
internal/db/dbtest/dbtest.go           # per-test schema helper
internal/jobs/model.go, store.go, options.go
internal/presets/presets.go            # presets, fallback chains, flavor policy
internal/ffmpeg/args.go                # allow-list argument builder
internal/ffmpeg/probe.go               # ffprobe + validation
internal/ffmpeg/run.go                 # runner, progress, sanitize
internal/ffmpeg/caps.go                # encoder detection
internal/testutil/media.go             # generated test clips
internal/worker/worker.go
internal/api/{server,problem,middleware,upload,read,openapi}.go + openapi.json
internal/janitor/janitor.go
internal/ui/embed.go, internal/ui/dist/ (built)
ui/{package.json,tsconfig.json,index.html,docs.html} ui/src/{main.ts,docs.ts,style.css}
internal/e2e/e2e_test.go
Dockerfile.api, Dockerfile.worker, compose.yaml
docker/ffmpeg.env
scripts/{test-db.sh,gen-secrets.sh,pin-digests.sh,smoke.sh}
tests/container/hardening.sh
.github/workflows/{ci.yml,release.yml}
docs/{operations.md,grundschutz-mapping.md,risk-acceptance-template.md,licensing.md}
```

---

### Task 1: Scaffold, config and audit log

**Files:**
- Create: `go.mod`, `Makefile`, `.gitignore`, `internal/config/config.go`, `internal/config/config_test.go`, `internal/audit/audit.go`, `internal/audit/audit_test.go`, `scripts/test-db.sh`

**Interfaces:**
- Produces:
  - `config.Config` struct (fields below) and `config.Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error)`
  - `audit.New(w io.Writer) *audit.Logger`, `(*Logger).Event(r *http.Request, event, jobID string, attrs ...any)`, `(*Logger).System(event, jobID string, attrs ...any)`

- [ ] **Step 1: Init module and ignore rules**

```bash
cd /home/rain/Projects/ConvertFlow
go mod init github.com/ShifuRain/convertflow
cat > .gitignore <<'EOF'
/secrets/
/bin/
ui/node_modules/
internal/ui/dist/*
!internal/ui/dist/.gitkeep
EOF
mkdir -p internal/ui/dist && touch internal/ui/dist/.gitkeep
```

- [ ] **Step 2: Write the failing config test** — `internal/config/config_test.go`

```go
package config_test

import (
	"errors"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
func noFile(string) ([]byte, error)               { return nil, errors.New("no file") }

func TestDefaults(t *testing.T) {
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}), noFile)
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != "127.0.0.1:8080" || c.DataDir != "/data" || c.Flavor != "lgpl" {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.Retention != 24*time.Hour || c.MaxAttempts != 3 || c.MaxStreams != 16 {
		t.Fatalf("bad defaults: %+v", c)
	}
	if c.MaxUploadBytes != 8<<30 || c.MaxPixels != 8192*4320 || c.MaxDurationSec != 6*3600 {
		t.Fatalf("bad limits: %+v", c)
	}
}

func TestDatabaseURLFile(t *testing.T) {
	read := func(p string) ([]byte, error) {
		if p != "/run/secrets/u" {
			return nil, errors.New("wrong path")
		}
		return []byte("postgres://from-file\n"), nil
	}
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL_FILE": "/run/secrets/u"}), read)
	if err != nil || c.DatabaseURL != "postgres://from-file" {
		t.Fatalf("got %q, %v", c.DatabaseURL, err)
	}
}

func TestInvalidValues(t *testing.T) {
	base := map[string]string{"CONVERTFLOW_DATABASE_URL": "postgres://x"}
	cases := map[string]map[string]string{
		"missing db":  {},
		"bad flavor":  {"CONVERTFLOW_FLAVOR": "gpl"},
		"bad size":    {"CONVERTFLOW_MAX_UPLOAD_BYTES": "abc"},
		"neg size":    {"CONVERTFLOW_MAX_UPLOAD_BYTES": "-5"},
		"bad retain":  {"CONVERTFLOW_RETENTION": "tomorrow"},
	}
	for name, extra := range cases {
		m := map[string]string{}
		if name != "missing db" {
			for k, v := range base {
				m[k] = v
			}
		}
		for k, v := range extra {
			m[k] = v
		}
		if _, err := config.Load(env(m), noFile); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestRetentionOverride(t *testing.T) {
	c, err := config.Load(env(map[string]string{"CONVERTFLOW_DATABASE_URL": "x", "CONVERTFLOW_RETENTION": "36h"}), noFile)
	if err != nil || c.Retention != 36*time.Hour {
		t.Fatalf("got %v, %v", c.Retention, err)
	}
}
```

- [ ] **Step 3: Run it to see it fail**

Run: `go test ./internal/config/ -v`
Expected: FAIL (package does not exist / `config.Load` undefined)

- [ ] **Step 4: Implement** — `internal/config/config.go`

```go
// Package config parses ConvertFlow's environment-based configuration.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr     string
	DatabaseURL    string
	DataDir        string
	Flavor         string // "lgpl" or "full"
	TLSCertFile    string
	TLSKeyFile     string
	VAAPIDevice    string
	MaxUploadBytes int64
	MaxPixels      int64
	MaxDurationSec float64
	MaxStreams     int
	MaxAttempts    int
	Retention      time.Duration
	StaleAfter     time.Duration
	WorkerPoll     time.Duration
}

func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	var firstErr error
	fail := func(format string, a ...any) {
		if firstErr == nil {
			firstErr = fmt.Errorf(format, a...)
		}
	}
	str := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	num := func(key string, def int64) int64 {
		v := getenv(key)
		if v == "" {
			return def
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			fail("%s must be a positive integer, got %q", key, v)
			return def
		}
		return n
	}
	dur := func(key string, def time.Duration) time.Duration {
		v := getenv(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fail("%s must be a positive duration, got %q", key, v)
			return def
		}
		return d
	}

	c := Config{
		ListenAddr:     str("CONVERTFLOW_LISTEN", "127.0.0.1:8080"),
		DataDir:        str("CONVERTFLOW_DATA_DIR", "/data"),
		Flavor:         str("CONVERTFLOW_FLAVOR", "lgpl"),
		TLSCertFile:    getenv("CONVERTFLOW_TLS_CERT_FILE"),
		TLSKeyFile:     getenv("CONVERTFLOW_TLS_KEY_FILE"),
		VAAPIDevice:    str("CONVERTFLOW_VAAPI_DEVICE", "/dev/dri/renderD128"),
		MaxUploadBytes: num("CONVERTFLOW_MAX_UPLOAD_BYTES", 8<<30),
		MaxPixels:      num("CONVERTFLOW_MAX_PIXELS", 8192*4320),
		MaxDurationSec: float64(num("CONVERTFLOW_MAX_DURATION_SEC", 6*3600)),
		MaxStreams:     int(num("CONVERTFLOW_MAX_STREAMS", 16)),
		MaxAttempts:    int(num("CONVERTFLOW_MAX_ATTEMPTS", 3)),
		Retention:      dur("CONVERTFLOW_RETENTION", 24*time.Hour),
		StaleAfter:     dur("CONVERTFLOW_STALE_AFTER", 60*time.Second),
		WorkerPoll:     dur("CONVERTFLOW_WORKER_POLL", 2*time.Second),
	}
	if c.Flavor != "lgpl" && c.Flavor != "full" {
		fail("CONVERTFLOW_FLAVOR must be \"lgpl\" or \"full\", got %q", c.Flavor)
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		fail("CONVERTFLOW_TLS_CERT_FILE and CONVERTFLOW_TLS_KEY_FILE must be set together")
	}
	c.DatabaseURL = getenv("CONVERTFLOW_DATABASE_URL")
	if f := getenv("CONVERTFLOW_DATABASE_URL_FILE"); f != "" {
		b, err := readFile(f)
		if err != nil {
			fail("reading CONVERTFLOW_DATABASE_URL_FILE: %v", err)
		} else {
			c.DatabaseURL = strings.TrimSpace(string(b))
		}
	}
	if c.DatabaseURL == "" {
		fail("CONVERTFLOW_DATABASE_URL or CONVERTFLOW_DATABASE_URL_FILE is required")
	}
	return c, firstErr
}
```

- [ ] **Step 5: Run config tests**

Run: `go test ./internal/config/ -v`
Expected: PASS

- [ ] **Step 6: Write the failing audit test** — `internal/audit/audit_test.go`

```go
package audit_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/ShifuRain/convertflow/internal/audit"
)

func TestEventHasIPAndNoBody(t *testing.T) {
	var buf bytes.Buffer
	l := audit.New(&buf)
	r := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewBufferString("SECRET-CONTENT"))
	r.RemoteAddr = "192.0.2.1:5555"
	l.Event(r, "job_created", "abc", "preset", "audio-wav")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["event"] != "job_created" || m["job_id"] != "abc" || m["remote_ip"] != "192.0.2.1" || m["audit"] != true {
		t.Fatalf("bad record: %v", m)
	}
	if bytes.Contains(buf.Bytes(), []byte("SECRET-CONTENT")) {
		t.Fatal("request body leaked into audit log")
	}
}

func TestSystemEvent(t *testing.T) {
	var buf bytes.Buffer
	audit.New(&buf).System("job_started", "id1", "worker", "w1")
	var m map[string]any
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m["event"] != "job_started" || m["worker"] != "w1" {
		t.Fatalf("bad record: %v", m)
	}
}
```

- [ ] **Step 7: Run to fail, then implement** — `internal/audit/audit.go`

Run: `go test ./internal/audit/` → FAIL (undefined)

```go
// Package audit writes structured audit records (JSON, one per line).
package audit

import (
	"io"
	"log/slog"
	"net"
	"net/http"
)

type Logger struct{ l *slog.Logger }

func New(w io.Writer) *Logger { return &Logger{slog.New(slog.NewJSONHandler(w, nil))} }

// Event records an API-originated event with the client IP. Never pass file contents.
func (a *Logger) Event(r *http.Request, event, jobID string, attrs ...any) {
	args := append([]any{"audit", true, "event", event, "job_id", jobID, "remote_ip", remoteIP(r)}, attrs...)
	a.l.Info("audit", args...)
}

// System records an event with no request (worker, janitor).
func (a *Logger) System(event, jobID string, attrs ...any) {
	args := append([]any{"audit", true, "event", event, "job_id", jobID}, attrs...)
	a.l.Info("audit", args...)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
```

Run: `go test ./internal/audit/` → PASS

- [ ] **Step 8: Makefile and DB test script**

`Makefile`:
```make
.PHONY: ui test test-db build
ui:
	cd ui && npm ci && npm run build
test:
	go vet ./...
	go test ./...
test-db:
	scripts/test-db.sh
build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...
```

`scripts/test-db.sh` (then `chmod +x scripts/test-db.sh`):
```bash
#!/usr/bin/env bash
# Runs the full test suite against a throwaway Postgres container.
set -euo pipefail
RT=${CONTAINER_RUNTIME:-docker}
NAME=cf-test-pg-$$
$RT run --rm -d --name "$NAME" -e POSTGRES_PASSWORD=test -p 127.0.0.1::5432 postgres:16-bookworm >/dev/null
trap '$RT stop "$NAME" >/dev/null' EXIT
for _ in $(seq 1 60); do
  $RT exec "$NAME" pg_isready -U postgres >/dev/null 2>&1 && break
  sleep 1
done
PORT=$($RT port "$NAME" 5432/tcp | head -n1 | sed 's/.*://')
export TEST_DATABASE_URL="postgres://postgres:test@127.0.0.1:${PORT}/postgres?sslmode=disable"
go vet ./...
go test ./... "$@"
```

- [ ] **Step 9: Commit**

```bash
git add -A && git commit -m "feat: scaffold module, config and audit log" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Database, migrations and job store

**Files:**
- Create: `internal/db/db.go`, `internal/db/migrate.go`, `internal/db/migrations/001_init.sql`, `internal/db/dbtest/dbtest.go`, `internal/jobs/model.go`, `internal/jobs/store.go`, `internal/jobs/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `db.Connect(ctx, url string) (*pgxpool.Pool, error)`; `db.Migrate(ctx, pool) error`; `dbtest.New(t *testing.T) *pgxpool.Pool`
  - `jobs.Status` consts `StatusQueued/Probing/Running/Done/Failed/Canceled`, `(Status).Terminal() bool`
  - `jobs.Options{Preset string; Speed, CRF, MaxHeight, AudioBitrateK *int}` (JSON tags `preset,speed,crf,max_height,audio_bitrate_k`, all `omitempty` except preset)
  - `jobs.Job{ID, Filename, InputPath, OutputPath, EncoderUsed, Error, WorkerID string; Status Status; Options Options; Progress float64; Probe []byte; Attempts int; CancelRequested bool; CreatedAt, UpdatedAt time.Time; FinishedAt *time.Time}`
  - `jobs.NewStore(*pgxpool.Pool) *Store` with methods:
    `Create(ctx, *Job) error`, `Get(ctx, id) (*Job, error)`, `List(ctx, limit int) ([]Job, error)`,
    `Claim(ctx, workerID string) (*Job, error)` (nil,nil when queue empty),
    `SetProbe(ctx, id string, probe []byte) error`, `SetRunning(ctx, id, encoder string) error`,
    `UpdateProgress(ctx, id string, pct float64) error`, `Heartbeat(ctx, id string) (cancelRequested bool, err error)`,
    `Complete(ctx, id, outputPath string) error`, `Fail(ctx, id, msg string) error`, `MarkCanceled(ctx, id string) error`,
    `RequestCancel(ctx, id string) error`, `Delete(ctx, id string) (*Job, error)`,
    `RequeueStale(ctx, staleAfter time.Duration, maxAttempts int) (requeued, failed int, err error)`,
    `Expired(ctx, retention time.Duration) ([]Job, error)`, `Exists(ctx, id string) (bool, error)`,
    `Counts(ctx) (map[string]int, error)`, `Ping(ctx) error`,
    `UpsertWorker(ctx, id string, encoders []string) error`, `LiveEncoders(ctx, within time.Duration) (map[string]bool, error)`, `LiveWorkers(ctx, within time.Duration) (int, error)`
  - Errors `jobs.ErrNotFound`, `jobs.ErrBusy`

- [ ] **Step 1: Dependencies and SQL**

```bash
go get github.com/jackc/pgx/v5 github.com/google/uuid
```

`internal/db/migrations/001_init.sql`:
```sql
CREATE TABLE jobs (
  id uuid PRIMARY KEY,
  status text NOT NULL CHECK (status IN ('queued','probing','running','done','failed','canceled')),
  filename text NOT NULL,
  input_path text NOT NULL,
  output_path text NOT NULL DEFAULT '',
  options jsonb NOT NULL,
  progress double precision NOT NULL DEFAULT 0,
  encoder_used text NOT NULL DEFAULT '',
  probe jsonb,
  error text NOT NULL DEFAULT '',
  attempts int NOT NULL DEFAULT 0,
  worker_id text NOT NULL DEFAULT '',
  cancel_requested boolean NOT NULL DEFAULT false,
  heartbeat_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  finished_at timestamptz
);
CREATE INDEX jobs_queue_idx ON jobs (created_at) WHERE status = 'queued';
CREATE INDEX jobs_finished_idx ON jobs (finished_at) WHERE finished_at IS NOT NULL;

CREATE TABLE workers (
  id text PRIMARY KEY,
  encoders text[] NOT NULL,
  seen_at timestamptz NOT NULL DEFAULT now()
);
```

- [ ] **Step 2: DB package and test helper**

`internal/db/db.go`:
```go
// Package db opens the Postgres pool and applies embedded migrations.
package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
```

`internal/db/migrate.go`:
```go
package db

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationLock = 727274

// Migrate applies unapplied migrations in filename order. Safe to call concurrently.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	for i, e := range entries {
		version := i + 1
		sqlText, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version int PRIMARY KEY)`); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&done); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if !done {
			if _, err := tx.Exec(ctx, string(sqlText)); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migration %s: %w", e.Name(), err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
				_ = tx.Rollback(ctx)
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
```

`internal/db/dbtest/dbtest.go`:
```go
// Package dbtest gives each test an isolated, migrated Postgres schema.
package dbtest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})
	return pool
}
```

- [ ] **Step 3: Model** — `internal/jobs/model.go`

```go
// Package jobs defines the job model and its Postgres-backed store.
package jobs

import "time"

type Status string

const (
	StatusQueued   Status = "queued"
	StatusProbing  Status = "probing"
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCanceled
}

// Options are the only user-controllable transcode parameters (see Validate in options.go).
type Options struct {
	Preset        string `json:"preset"`
	Speed         *int   `json:"speed,omitempty"`
	CRF           *int   `json:"crf,omitempty"`
	MaxHeight     *int   `json:"max_height,omitempty"`
	AudioBitrateK *int   `json:"audio_bitrate_k,omitempty"`
}

type Job struct {
	ID              string
	Status          Status
	Filename        string
	InputPath       string
	OutputPath      string
	Options         Options
	Progress        float64
	EncoderUsed     string
	Probe           []byte
	Error           string
	Attempts        int
	WorkerID        string
	CancelRequested bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
	FinishedAt      *time.Time
}
```

- [ ] **Step 4: Write the failing store tests** — `internal/jobs/store_test.go`

```go
package jobs_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
)

func newJob() *jobs.Job {
	id := uuid.NewString()
	return &jobs.Job{ID: id, Filename: "a.mp4", InputPath: "/data/in/" + id, Options: jobs.Options{Preset: "audio-wav"}}
}

func TestCreateGetRoundTrip(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	j := newJob()
	if err := s.Create(ctx, j); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, j.ID)
	if err != nil || got.Status != jobs.StatusQueued || got.Options.Preset != "audio-wav" || got.Filename != "a.mp4" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := s.Get(ctx, uuid.NewString()); err != jobs.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestClaimIsExclusive(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.Create(ctx, newJob()); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j, err := s.Claim(ctx, fmt.Sprintf("w%d", i))
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if j != nil {
				mu.Lock()
				claimed[j.ID]++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(claimed) != 3 {
		t.Fatalf("want 3 distinct claims, got %v", claimed)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}

func TestClaimEmptyQueue(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	j, err := s.Claim(context.Background(), "w")
	if j != nil || err != nil {
		t.Fatalf("want nil,nil got %v,%v", j, err)
	}
}

func TestLifecycleAndCancel(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	j := newJob()
	_ = s.Create(ctx, j)
	c, _ := s.Claim(ctx, "w1")
	if c.Status != jobs.StatusProbing || c.Attempts != 1 || c.WorkerID != "w1" {
		t.Fatalf("bad claim %+v", c)
	}
	_ = s.SetRunning(ctx, j.ID, "libsvtav1")
	_ = s.UpdateProgress(ctx, j.ID, 42.5)
	cancel, err := s.Heartbeat(ctx, j.ID)
	if err != nil || cancel {
		t.Fatalf("heartbeat %v %v", cancel, err)
	}
	_ = s.RequestCancel(ctx, j.ID)
	if cancel, _ = s.Heartbeat(ctx, j.ID); !cancel {
		t.Fatal("cancel flag not visible to heartbeat")
	}
	_ = s.MarkCanceled(ctx, j.ID)
	got, _ := s.Get(ctx, j.ID)
	if got.Status != jobs.StatusCanceled || got.FinishedAt == nil || got.EncoderUsed != "libsvtav1" {
		t.Fatalf("bad final %+v", got)
	}
}

func TestRequestCancelQueuedCancelsImmediately(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	j := newJob()
	_ = s.Create(ctx, j)
	_ = s.RequestCancel(ctx, j.ID)
	got, _ := s.Get(ctx, j.ID)
	if got.Status != jobs.StatusCanceled {
		t.Fatalf("want canceled, got %s", got.Status)
	}
}

func TestCompleteAndFail(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	a, b := newJob(), newJob()
	_ = s.Create(ctx, a)
	_ = s.Create(ctx, b)
	_ = s.Complete(ctx, a.ID, "/data/out/x.wav")
	_ = s.Fail(ctx, b.ID, "boom")
	ga, _ := s.Get(ctx, a.ID)
	gb, _ := s.Get(ctx, b.ID)
	if ga.Status != jobs.StatusDone || ga.Progress != 100 || ga.OutputPath != "/data/out/x.wav" {
		t.Fatalf("bad done %+v", ga)
	}
	if gb.Status != jobs.StatusFailed || gb.Error != "boom" {
		t.Fatalf("bad failed %+v", gb)
	}
}

func TestRequeueStale(t *testing.T) {
	pool := dbtest.New(t)
	s := jobs.NewStore(pool)
	ctx := context.Background()
	a, b := newJob(), newJob()
	_ = s.Create(ctx, a)
	_ = s.Create(ctx, b)
	_, _ = s.Claim(ctx, "w") // a: attempts=1
	_, _ = s.Claim(ctx, "w") // b: attempts=1
	// b has exhausted attempts
	_, _ = pool.Exec(ctx, `UPDATE jobs SET attempts = 3 WHERE id = $1::uuid`, b.ID)
	_, _ = pool.Exec(ctx, `UPDATE jobs SET heartbeat_at = now() - interval '10 minutes'`)
	requeued, failed, err := s.RequeueStale(ctx, time.Minute, 3)
	if err != nil || requeued != 1 || failed != 1 {
		t.Fatalf("got %d,%d,%v", requeued, failed, err)
	}
	ga, _ := s.Get(ctx, a.ID)
	gb, _ := s.Get(ctx, b.ID)
	if ga.Status != jobs.StatusQueued || gb.Status != jobs.StatusFailed {
		t.Fatalf("a=%s b=%s", ga.Status, gb.Status)
	}
}

func TestDeleteSemantics(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	idle, busy := newJob(), newJob()
	_ = s.Create(ctx, idle)
	_ = s.Create(ctx, busy)
	_, _ = s.Claim(ctx, "w") // claims `idle` (older)
	if _, err := s.Delete(ctx, idle.ID); err != jobs.ErrBusy {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	d, err := s.Delete(ctx, busy.ID) // still queued
	if err != nil || d.ID != busy.ID {
		t.Fatalf("got %v %v", d, err)
	}
	if _, err := s.Delete(ctx, uuid.NewString()); err != jobs.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestExpiredAndExists(t *testing.T) {
	pool := dbtest.New(t)
	s := jobs.NewStore(pool)
	ctx := context.Background()
	old, fresh := newJob(), newJob()
	_ = s.Create(ctx, old)
	_ = s.Create(ctx, fresh)
	_ = s.Fail(ctx, old.ID, "x")
	_ = s.Fail(ctx, fresh.ID, "x")
	_, _ = pool.Exec(ctx, `UPDATE jobs SET finished_at = now() - interval '2 days' WHERE id = $1::uuid`, old.ID)
	exp, err := s.Expired(ctx, 24*time.Hour)
	if err != nil || len(exp) != 1 || exp[0].ID != old.ID {
		t.Fatalf("got %v %v", exp, err)
	}
	if ok, _ := s.Exists(ctx, old.ID); !ok {
		t.Fatal("should exist")
	}
	if ok, _ := s.Exists(ctx, uuid.NewString()); ok {
		t.Fatal("should not exist")
	}
}

func TestWorkersCapabilities(t *testing.T) {
	pool := dbtest.New(t)
	s := jobs.NewStore(pool)
	ctx := context.Background()
	_ = s.UpsertWorker(ctx, "w1", []string{"libopus", "pcm_s16le"})
	_ = s.UpsertWorker(ctx, "w2", []string{"libsvtav1"})
	_, _ = pool.Exec(ctx, `INSERT INTO workers (id, encoders, seen_at) VALUES ('dead', '{libx264}', now() - interval '1 hour')`)
	enc, err := s.LiveEncoders(ctx, time.Minute)
	if err != nil || !enc["libopus"] || !enc["libsvtav1"] || enc["libx264"] {
		t.Fatalf("got %v %v", enc, err)
	}
	if n, _ := s.LiveWorkers(ctx, time.Minute); n != 2 {
		t.Fatalf("want 2 live workers, got %d", n)
	}
}

func TestCounts(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	_ = s.Create(ctx, newJob())
	c, err := s.Counts(ctx)
	if err != nil || c["queued"] != 1 {
		t.Fatalf("got %v %v", c, err)
	}
}
```

- [ ] **Step 5: Run to see failure**

Run: `scripts/test-db.sh ./internal/jobs/ -run . -v`
Expected: FAIL (`jobs.NewStore` undefined)

- [ ] **Step 6: Implement the store** — `internal/jobs/store.go`

```go
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

func (s *Store) SetProbe(ctx context.Context, id string, probe []byte) error {
	return s.exec(ctx, `UPDATE jobs SET probe = $2, updated_at = now() WHERE id = $1::uuid`, id, probe)
}

func (s *Store) SetRunning(ctx context.Context, id, encoder string) error {
	return s.exec(ctx, `UPDATE jobs SET status = 'running', encoder_used = $2, updated_at = now() WHERE id = $1::uuid`, id, encoder)
}

func (s *Store) UpdateProgress(ctx context.Context, id string, pct float64) error {
	return s.exec(ctx, `UPDATE jobs SET progress = $2, updated_at = now() WHERE id = $1::uuid AND status = 'running'`, id, pct)
}

// Heartbeat refreshes the liveness timestamp and reports whether cancellation was requested.
func (s *Store) Heartbeat(ctx context.Context, id string) (bool, error) {
	var c bool
	err := s.pool.QueryRow(ctx, `UPDATE jobs SET heartbeat_at = now() WHERE id = $1::uuid RETURNING cancel_requested`, id).Scan(&c)
	return c, err
}

func (s *Store) Complete(ctx context.Context, id, outputPath string) error {
	return s.exec(ctx, `UPDATE jobs SET status = 'done', progress = 100, output_path = $2, finished_at = now(), updated_at = now() WHERE id = $1::uuid`, id, outputPath)
}

func (s *Store) Fail(ctx context.Context, id, msg string) error {
	return s.exec(ctx, `UPDATE jobs SET status = 'failed', error = $2, finished_at = now(), updated_at = now() WHERE id = $1::uuid`, id, msg)
}

func (s *Store) MarkCanceled(ctx context.Context, id string) error {
	return s.exec(ctx, `UPDATE jobs SET status = 'canceled', finished_at = now(), updated_at = now() WHERE id = $1::uuid`, id)
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

func (s *Store) RequeueStale(ctx context.Context, staleAfter time.Duration, maxAttempts int) (int, int, error) {
	var requeued, failed int
	err := s.pool.QueryRow(ctx, `
WITH s AS (
  UPDATE jobs SET
    status = CASE WHEN attempts < $2 THEN 'queued' ELSE 'failed' END,
    error = CASE WHEN attempts < $2 THEN error ELSE 'worker lost; attempts exhausted' END,
    finished_at = CASE WHEN attempts < $2 THEN NULL ELSE now() END,
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
```

- [ ] **Step 7: Run tests to pass**

Run: `scripts/test-db.sh ./internal/jobs/ -v`
Expected: PASS (all 10 tests)

- [ ] **Step 8: Commit**

```bash
git add -A && git commit -m "feat: postgres schema, migrations and job store" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Presets, flavor policy and option validation

**Files:**
- Create: `internal/presets/presets.go`, `internal/presets/presets_test.go`, `internal/jobs/options.go`, `internal/jobs/options_test.go`

**Interfaces:**
- Consumes: `jobs.Options` (Task 2)
- Produces:
  - `presets.Kind` (`KindVideo`, `KindAudio`), `presets.Candidate{Video, Audio string}`, `presets.Preset{ID, Label string; Kind Kind; Container, Ext string; Candidates []Candidate}`
  - `presets.All() []Preset`, `presets.Get(id string) (Preset, bool)`, `(Preset).Resolve(enc map[string]bool) (Candidate, bool)`, `presets.Available(enc map[string]bool) []Preset`, `presets.AllEncoders() []string`, `presets.FlavorAllows(flavor, encoder string) bool`
  - `(jobs.Options).Validate() error`

- [ ] **Step 1: Write the failing presets test** — `internal/presets/presets_test.go`

```go
package presets_test

import (
	"testing"

	"github.com/ShifuRain/convertflow/internal/presets"
)

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestResolvePrefersFirstAvailable(t *testing.T) {
	p, _ := presets.Get("video-av1")
	c, ok := p.Resolve(set("libsvtav1", "libvpx-vp9", "libopus"))
	if !ok || c.Video != "libsvtav1" {
		t.Fatalf("got %+v %v", c, ok)
	}
	c, ok = p.Resolve(set("av1_nvenc", "libsvtav1", "libopus"))
	if !ok || c.Video != "av1_nvenc" {
		t.Fatalf("hardware should win: %+v", c)
	}
}

func TestResolveFallsBackToVP9(t *testing.T) {
	p, _ := presets.Get("video-av1")
	c, ok := p.Resolve(set("libvpx-vp9", "libopus"))
	if !ok || c.Video != "libvpx-vp9" {
		t.Fatalf("got %+v %v", c, ok)
	}
}

func TestResolveNeedsAudioEncoder(t *testing.T) {
	p, _ := presets.Get("video-vp9")
	if _, ok := p.Resolve(set("libvpx-vp9")); ok {
		t.Fatal("must not resolve without audio encoder")
	}
}

func TestAvailableFilters(t *testing.T) {
	av := presets.Available(set("pcm_s16le"))
	if len(av) != 1 || av[0].ID != "audio-wav" {
		t.Fatalf("got %v", av)
	}
}

func TestFlavorPolicy(t *testing.T) {
	for _, e := range []string{"libx264", "libx265", "aac"} {
		if presets.FlavorAllows("lgpl", e) || !presets.FlavorAllows("full", e) {
			t.Errorf("%s flavor policy wrong", e)
		}
	}
	if !presets.FlavorAllows("lgpl", "libsvtav1") {
		t.Error("royalty-free encoders must be allowed in lgpl")
	}
}

func TestUniqueIDsAndEncoders(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range presets.All() {
		if seen[p.ID] {
			t.Fatalf("duplicate id %s", p.ID)
		}
		seen[p.ID] = true
		if p.Ext == "" || p.Container == "" || len(p.Candidates) == 0 {
			t.Fatalf("incomplete preset %+v", p)
		}
	}
	all := set(presets.AllEncoders()...)
	if !all["libsvtav1"] || !all["pcm_s16le"] || !all["aac"] {
		t.Fatalf("AllEncoders incomplete: %v", all)
	}
}
```

- [ ] **Step 2: Run to fail** — `go test ./internal/presets/` → FAIL (undefined)

- [ ] **Step 3: Implement** — `internal/presets/presets.go`

```go
// Package presets defines the transcode presets, their encoder fallback chains and flavor policy.
package presets

type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
)

// Candidate is one encoder combination; Video is empty for audio presets.
type Candidate struct{ Video, Audio string }

type Preset struct {
	ID         string
	Label      string
	Kind       Kind
	Container  string // ffmpeg muxer name
	Ext        string // output file extension
	Candidates []Candidate
}

var all = []Preset{
	{ID: "video-av1", Label: "Video – AV1 + Opus (WebM)", Kind: KindVideo, Container: "webm", Ext: "webm", Candidates: []Candidate{
		{"av1_nvenc", "libopus"}, {"av1_qsv", "libopus"}, {"av1_vaapi", "libopus"},
		{"libsvtav1", "libopus"}, {"libvpx-vp9", "libopus"}}},
	{ID: "video-vp9", Label: "Video – VP9 + Opus (WebM)", Kind: KindVideo, Container: "webm", Ext: "webm", Candidates: []Candidate{
		{"libvpx-vp9", "libopus"}}},
	{ID: "video-h264", Label: "Video – H.264 + AAC (MP4)", Kind: KindVideo, Container: "mp4", Ext: "mp4", Candidates: []Candidate{
		{"libx264", "aac"}}},
	{ID: "video-h265", Label: "Video – H.265 + AAC (MP4)", Kind: KindVideo, Container: "mp4", Ext: "mp4", Candidates: []Candidate{
		{"libx265", "aac"}}},
	{ID: "audio-opus", Label: "Audio – Opus", Kind: KindAudio, Container: "ogg", Ext: "opus", Candidates: []Candidate{{"", "libopus"}}},
	{ID: "audio-vorbis", Label: "Audio – Vorbis (Ogg)", Kind: KindAudio, Container: "ogg", Ext: "ogg", Candidates: []Candidate{{"", "libvorbis"}}},
	{ID: "audio-flac", Label: "Audio – FLAC", Kind: KindAudio, Container: "flac", Ext: "flac", Candidates: []Candidate{{"", "flac"}}},
	{ID: "audio-mp3", Label: "Audio – MP3", Kind: KindAudio, Container: "mp3", Ext: "mp3", Candidates: []Candidate{{"", "libmp3lame"}}},
	{ID: "audio-wav", Label: "Audio – WAV (PCM)", Kind: KindAudio, Container: "wav", Ext: "wav", Candidates: []Candidate{{"", "pcm_s16le"}}},
	{ID: "audio-aac", Label: "Audio – AAC (M4A)", Kind: KindAudio, Container: "ipod", Ext: "m4a", Candidates: []Candidate{{"", "aac"}}},
}

// fullOnly encoders are withheld in the royalty-free (lgpl) flavor for patent reasons.
var fullOnly = map[string]bool{"libx264": true, "libx265": true, "aac": true}

func All() []Preset { return append([]Preset(nil), all...) }

func Get(id string) (Preset, bool) {
	for _, p := range all {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Resolve returns the first candidate whose encoders are all in enc.
func (p Preset) Resolve(enc map[string]bool) (Candidate, bool) {
	for _, c := range p.Candidates {
		if (c.Video == "" || enc[c.Video]) && enc[c.Audio] {
			return c, true
		}
	}
	return Candidate{}, false
}

func Available(enc map[string]bool) []Preset {
	out := []Preset{}
	for _, p := range all {
		if _, ok := p.Resolve(enc); ok {
			out = append(out, p)
		}
	}
	return out
}

func AllEncoders() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range all {
		for _, c := range p.Candidates {
			for _, e := range []string{c.Video, c.Audio} {
				if e != "" && !seen[e] {
					seen[e] = true
					out = append(out, e)
				}
			}
		}
	}
	return out
}

func FlavorAllows(flavor, encoder string) bool {
	if flavor == "full" {
		return true
	}
	return !fullOnly[encoder]
}
```

- [ ] **Step 4: Run to pass** — `go test ./internal/presets/` → PASS

- [ ] **Step 5: Write failing options test** — `internal/jobs/options_test.go`

```go
package jobs_test

import (
	"testing"

	"github.com/ShifuRain/convertflow/internal/jobs"
)

func ip(i int) *int { return &i }

func TestOptionsValidate(t *testing.T) {
	ok := []jobs.Options{
		{Preset: "audio-wav"},
		{Preset: "video-vp9", Speed: ip(0), CRF: ip(18), MaxHeight: ip(1080), AudioBitrateK: ip(128)},
		{Preset: "video-av1", Speed: ip(10), CRF: ip(45), MaxHeight: ip(240), AudioBitrateK: ip(320)},
	}
	for _, o := range ok {
		if err := o.Validate(); err != nil {
			t.Errorf("%+v should be valid: %v", o, err)
		}
	}
	bad := []jobs.Options{
		{},
		{Preset: "nope"},
		{Preset: "audio-wav", Speed: ip(-1)},
		{Preset: "audio-wav", Speed: ip(11)},
		{Preset: "video-vp9", CRF: ip(17)},
		{Preset: "video-vp9", CRF: ip(46)},
		{Preset: "video-vp9", MaxHeight: ip(1081)},
		{Preset: "audio-mp3", AudioBitrateK: ip(100)},
	}
	for _, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("%+v should be invalid", o)
		}
	}
}
```

Run: `go test ./internal/jobs/ -run TestOptionsValidate` → FAIL (`Validate` undefined)

- [ ] **Step 6: Implement** — `internal/jobs/options.go`

```go
package jobs

import (
	"fmt"
	"slices"

	"github.com/ShifuRain/convertflow/internal/presets"
)

var (
	allowedHeights  = []int{240, 360, 480, 720, 1080, 1440, 2160}
	allowedBitrates = []int{64, 96, 128, 192, 256, 320}
)

// Validate enforces the allow-list; BuildArgs relies on it and must never see unvalidated options.
func (o Options) Validate() error {
	if _, ok := presets.Get(o.Preset); !ok {
		return fmt.Errorf("unknown preset %q", o.Preset)
	}
	if o.Speed != nil && (*o.Speed < 0 || *o.Speed > 10) {
		return fmt.Errorf("speed must be between 0 (best quality) and 10 (fastest)")
	}
	if o.CRF != nil && (*o.CRF < 18 || *o.CRF > 45) {
		return fmt.Errorf("crf must be between 18 and 45")
	}
	if o.MaxHeight != nil && !slices.Contains(allowedHeights, *o.MaxHeight) {
		return fmt.Errorf("max_height must be one of %v", allowedHeights)
	}
	if o.AudioBitrateK != nil && !slices.Contains(allowedBitrates, *o.AudioBitrateK) {
		return fmt.Errorf("audio_bitrate_k must be one of %v", allowedBitrates)
	}
	return nil
}
```

- [ ] **Step 7: Run to pass and commit**

Run: `go test ./internal/presets/ ./internal/jobs/ -run 'TestOptions|TestResolve|TestAvailable|TestFlavor|TestUnique'` → PASS

```bash
git add -A && git commit -m "feat: presets with fallback chains, flavor policy, option validation" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 4: FFmpeg argument builder (allow-list)

**Files:**
- Create: `internal/ffmpeg/args.go`, `internal/ffmpeg/args_test.go`

**Interfaces:**
- Consumes: `jobs.Options`, `presets.Preset`, `presets.Candidate`
- Produces: `ffmpeg.Spec{Input, Output string; Preset presets.Preset; Cand presets.Candidate; Opts jobs.Options; HasVideo, HasAudio bool; VAAPIDevice string}`; `ffmpeg.BuildArgs(Spec) ([]string, error)`; `ffmpeg.ErrNoVideo`, `ffmpeg.ErrNoAudio`

- [ ] **Step 1: Write the failing tests** — `internal/ffmpeg/args_test.go`

```go
package ffmpeg_test

import (
	"slices"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
)

func ip(i int) *int { return &i }

func spec(t *testing.T, presetID string, c presets.Candidate, o jobs.Options, hasV, hasA bool) ffmpeg.Spec {
	t.Helper()
	p, ok := presets.Get(presetID)
	if !ok {
		t.Fatalf("no preset %s", presetID)
	}
	o.Preset = presetID
	return ffmpeg.Spec{Input: "/data/in/x", Output: "/data/out/x.webm", Preset: p, Cand: c, Opts: o,
		HasVideo: hasV, HasAudio: hasA, VAAPIDevice: "/dev/dri/renderD128"}
}

func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func build(t *testing.T, s ffmpeg.Spec) []string {
	t.Helper()
	a, err := ffmpeg.BuildArgs(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestBuildArgsSVTAV1Defaults(t *testing.T) {
	a := build(t, spec(t, "video-av1", presets.Candidate{Video: "libsvtav1", Audio: "libopus"}, jobs.Options{}, true, true))
	for _, seq := range [][]string{
		{"-c:v", "libsvtav1"}, {"-preset", "8"}, {"-crf", "35"}, {"-pix_fmt", "yuv420p"},
		{"-c:a", "libopus"}, {"-b:a", "128k"}, {"-map", "0:v:0"}, {"-map", "0:a:0"},
		{"-f", "webm"}, {"-protocol_whitelist", "file"}, {"-map_metadata", "-1"},
	} {
		if !hasSeq(a, seq...) {
			t.Errorf("missing %v in %v", seq, a)
		}
	}
	if a[len(a)-1] != "/data/out/x.webm" {
		t.Errorf("output must be last, got %v", a)
	}
	if slices.Index(a, "-protocol_whitelist") > slices.Index(a, "-i") {
		t.Error("-protocol_whitelist must precede -i")
	}
}

func TestBuildArgsOptions(t *testing.T) {
	o := jobs.Options{Speed: ip(0), CRF: ip(20), MaxHeight: ip(720), AudioBitrateK: ip(192)}
	a := build(t, spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, o, true, true))
	for _, seq := range [][]string{
		{"-cpu-used", "0"}, {"-crf", "20"}, {"-b:v", "0"}, {"-b:a", "192k"}, {"-vf", `scale=-2:min(ih\,720)`},
	} {
		if !hasSeq(a, seq...) {
			t.Errorf("missing %v in %v", seq, a)
		}
	}
}

func TestBuildArgsX264Preset(t *testing.T) {
	a := build(t, spec(t, "video-h264", presets.Candidate{Video: "libx264", Audio: "aac"}, jobs.Options{Speed: ip(10)}, true, true))
	if !hasSeq(a, "-preset", "ultrafast") || !hasSeq(a, "-crf", "23") {
		t.Errorf("bad x264 args %v", a)
	}
}

func TestBuildArgsVAAPI(t *testing.T) {
	a := build(t, spec(t, "video-av1", presets.Candidate{Video: "av1_vaapi", Audio: "libopus"}, jobs.Options{MaxHeight: ip(1080)}, true, true))
	if !hasSeq(a, "-vaapi_device", "/dev/dri/renderD128") {
		t.Errorf("missing vaapi device: %v", a)
	}
	if !hasSeq(a, "-vf", `scale=-2:min(ih\,1080),format=nv12,hwupload`) {
		t.Errorf("bad vaapi filter: %v", a)
	}
}

func TestBuildArgsAudioPreset(t *testing.T) {
	a := build(t, spec(t, "audio-mp3", presets.Candidate{Audio: "libmp3lame"}, jobs.Options{}, true, true))
	if !slices.Contains(a, "-vn") || slices.Contains(a, "0:v:0") || !hasSeq(a, "-ac", "2") {
		t.Errorf("audio preset must drop video and downmix: %v", a)
	}
	a = build(t, spec(t, "audio-flac", presets.Candidate{Audio: "flac"}, jobs.Options{}, false, true))
	if slices.Contains(a, "-b:a") {
		t.Errorf("flac must not get a bitrate: %v", a)
	}
}

func TestBuildArgsNeedsVideo(t *testing.T) {
	_, err := ffmpeg.BuildArgs(spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, jobs.Options{}, false, true))
	if err != ffmpeg.ErrNoVideo {
		t.Fatalf("want ErrNoVideo, got %v", err)
	}
	_, err = ffmpeg.BuildArgs(spec(t, "audio-wav", presets.Candidate{Audio: "pcm_s16le"}, jobs.Options{}, true, false))
	if err != ffmpeg.ErrNoAudio {
		t.Fatalf("want ErrNoAudio, got %v", err)
	}
}

// An MP3 with cover art has a "video" stream that the probe marks attached_pic; HasVideo=false
// for it, and an audio preset must still map only audio.
func TestBuildArgsIgnoresCoverArt(t *testing.T) {
	a := build(t, spec(t, "audio-opus", presets.Candidate{Audio: "libopus"}, jobs.Options{}, false, true))
	if slices.Contains(a, "0:v:0") || !slices.Contains(a, "-vn") {
		t.Errorf("cover art must not be mapped: %v", a)
	}
}

func TestBuildArgsVideoOnlyInputDropsAudio(t *testing.T) {
	a := build(t, spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, jobs.Options{}, true, false))
	if !slices.Contains(a, "-an") || hasSeq(a, "-map", "0:a:0") {
		t.Errorf("expected -an: %v", a)
	}
}

func TestBuildArgsRejectsUnsafePaths(t *testing.T) {
	s := spec(t, "audio-wav", presets.Candidate{Audio: "pcm_s16le"}, jobs.Options{}, false, true)
	s.Input = "-i evil"
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("relative/dash-leading input must be rejected")
	}
	s = spec(t, "audio-wav", presets.Candidate{Audio: "pcm_s16le"}, jobs.Options{}, false, true)
	s.Output = "rel.wav"
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("relative output must be rejected")
	}
}

func TestBuildArgsRejectsInvalidOptions(t *testing.T) {
	s := spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, jobs.Options{CRF: ip(999)}, true, true)
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("invalid options must be rejected")
	}
}

func TestBuildArgsRejectsEncoderOutsideAllowList(t *testing.T) {
	s := spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9; rm -rf /", Audio: "libopus"}, jobs.Options{}, true, true)
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("unknown encoder must be rejected")
	}
}
```

- [ ] **Step 2: Run to fail** — `go test ./internal/ffmpeg/` → FAIL (undefined)

- [ ] **Step 3: Implement** — `internal/ffmpeg/args.go`

```go
// Package ffmpeg builds, runs and supervises FFmpeg/ffprobe processes.
package ffmpeg

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
)

var (
	ErrNoVideo = errors.New("input has no video stream")
	ErrNoAudio = errors.New("input has no audio stream")
)

type Spec struct {
	Input, Output string
	Preset        presets.Preset
	Cand          presets.Candidate
	Opts          jobs.Options
	HasVideo      bool
	HasAudio      bool
	VAAPIDevice   string
}

var x26xPresets = []string{"veryslow", "slower", "slow", "medium", "medium", "fast", "fast", "faster", "veryfast", "superfast", "ultrafast"}

var defaultCRF = map[string]int{
	"libsvtav1": 35, "libvpx-vp9": 33, "libx264": 23, "libx265": 28,
	"av1_nvenc": 32, "av1_qsv": 32, "av1_vaapi": 32,
}

var lossyAudio = map[string]bool{"libopus": true, "libvorbis": true, "libmp3lame": true, "aac": true}
var losslessAudio = map[string]bool{"flac": true, "pcm_s16le": true}

// BuildArgs is the only place FFmpeg command lines are assembled. Every value comes from a
// fixed set or a validated integer; no user string reaches the argument list except the two
// absolute server-generated paths.
func BuildArgs(s Spec) ([]string, error) {
	if !filepath.IsAbs(s.Input) || !filepath.IsAbs(s.Output) {
		return nil, errors.New("input and output must be absolute paths")
	}
	if err := s.Opts.Validate(); err != nil {
		return nil, err
	}
	if _, ok := defaultCRF[s.Cand.Video]; s.Cand.Video != "" && !ok {
		return nil, fmt.Errorf("unsupported video encoder %q", s.Cand.Video)
	}
	if !lossyAudio[s.Cand.Audio] && !losslessAudio[s.Cand.Audio] {
		return nil, fmt.Errorf("unsupported audio encoder %q", s.Cand.Audio)
	}
	video := s.Preset.Kind == presets.KindVideo
	if video && !s.HasVideo {
		return nil, ErrNoVideo
	}
	if !video && !s.HasAudio {
		return nil, ErrNoAudio
	}

	a := []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-protocol_whitelist", "file"}
	if s.Cand.Video == "av1_vaapi" {
		a = append(a, "-vaapi_device", s.VAAPIDevice)
	}
	a = append(a, "-i", s.Input, "-map_metadata", "-1", "-map_chapters", "-1", "-sn", "-dn")
	if video {
		a = append(a, "-map", "0:v:0")
	} else {
		a = append(a, "-vn")
	}
	if s.HasAudio {
		a = append(a, "-map", "0:a:0")
	} else {
		a = append(a, "-an")
	}
	if video {
		a = append(a, videoArgs(s.Cand.Video, s.Opts)...)
	}
	if s.HasAudio {
		a = append(a, audioArgs(s.Cand.Audio, s.Opts)...)
	}
	a = append(a, "-f", s.Preset.Container, "-progress", "pipe:1", "-nostats", s.Output)
	return a, nil
}

func videoArgs(enc string, o jobs.Options) []string {
	speed := 6
	if o.Speed != nil {
		speed = *o.Speed
	}
	crf := defaultCRF[enc]
	if o.CRF != nil {
		crf = *o.CRF
	}
	c := strconv.Itoa(crf)

	var filters []string
	if o.MaxHeight != nil {
		filters = append(filters, fmt.Sprintf(`scale=-2:min(ih\,%d)`, *o.MaxHeight))
	}
	var enc_ []string
	switch enc {
	case "libsvtav1":
		enc_ = []string{"-c:v", enc, "-preset", strconv.Itoa(4 + speed*8/10), "-crf", c, "-pix_fmt", "yuv420p"}
	case "libvpx-vp9":
		enc_ = []string{"-c:v", enc, "-b:v", "0", "-crf", c, "-cpu-used", strconv.Itoa(speed * 8 / 10), "-row-mt", "1", "-pix_fmt", "yuv420p"}
	case "libx264":
		enc_ = []string{"-c:v", enc, "-preset", x26xPresets[speed], "-crf", c, "-pix_fmt", "yuv420p"}
	case "libx265":
		enc_ = []string{"-c:v", enc, "-preset", x26xPresets[speed], "-crf", c, "-pix_fmt", "yuv420p", "-tag:v", "hvc1"}
	case "av1_nvenc":
		enc_ = []string{"-c:v", enc, "-cq", c}
	case "av1_qsv":
		enc_ = []string{"-c:v", enc, "-global_quality", c}
	case "av1_vaapi":
		filters = append(filters, "format=nv12", "hwupload")
		enc_ = []string{"-c:v", enc, "-global_quality", c}
	}
	out := []string{}
	if len(filters) > 0 {
		out = append(out, "-vf", strings.Join(filters, ","))
	}
	return append(out, enc_...)
}

func audioArgs(enc string, o jobs.Options) []string {
	out := []string{"-c:a", enc}
	if lossyAudio[enc] {
		br := 128
		if o.AudioBitrateK != nil {
			br = *o.AudioBitrateK
		}
		out = append(out, "-b:a", strconv.Itoa(br)+"k")
	}
	if enc == "libmp3lame" {
		out = append(out, "-ac", "2")
	}
	return out
}

```


- [ ] **Step 4: Run to pass** — `go test ./internal/ffmpeg/ -v` → PASS

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat: allow-list FFmpeg argument builder" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 5: ffprobe validation, runner and encoder detection

**Files:**
- Create: `internal/testutil/media.go`, `internal/ffmpeg/probe.go`, `internal/ffmpeg/probe_test.go`, `internal/ffmpeg/run.go`, `internal/ffmpeg/run_test.go`, `internal/ffmpeg/caps.go`, `internal/ffmpeg/caps_test.go`

**Interfaces:**
- Consumes: `presets.AllEncoders`, `presets.FlavorAllows`
- Produces:
  - `testutil.RequireTools(t)`, `testutil.MakeClip(t *testing.T, dir, name, kind string) string` (kind: `"av"`, `"audio"`, `"video"`; returns path)
  - `ffmpeg.Limits{MaxDurationSec float64; MaxPixels int64; MaxStreams int}`
  - `ffmpeg.ProbeResult` with `HasVideo() bool`, `HasAudio() bool`, `DurationSeconds() float64`, `Validate(Limits) error`
  - `ffmpeg.ParseProbe([]byte) (*ProbeResult, error)`, `ffmpeg.Probe(ctx, bin, path, dir string) (*ProbeResult, []byte, error)`
  - `ffmpeg.Run(ctx, bin string, args []string, dir string, durationSec float64, onProgress func(pct float64)) (stderrTail string, err error)`
  - `ffmpeg.Sanitize(msg string, paths ...string) string`
  - `ffmpeg.DetectEncoders(ctx, ffmpegBin, flavor, vaapiDevice string) ([]string, error)`, `ffmpeg.ParseEncoders(output string) []string`

- [ ] **Step 1: Test media helper** — `internal/testutil/media.go`

```go
// Package testutil generates tiny media files for tests using FFmpeg's native encoders.
package testutil

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func RequireTools(t *testing.T) {
	t.Helper()
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
}

// MakeClip writes a 1-second clip. kind: "av" (mkv, video+audio), "audio" (wav), "video" (mkv, no audio).
func MakeClip(t *testing.T, dir, name, kind string) string {
	t.Helper()
	RequireTools(t)
	out := filepath.Join(dir, name)
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	video := []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10"}
	audio := []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=1"}
	switch kind {
	case "av":
		args = append(append(append(args, video...), audio...), "-c:v", "mpeg4", "-c:a", "pcm_s16le", "-f", "matroska")
	case "audio":
		args = append(append(args, audio...), "-c:a", "pcm_s16le", "-f", "wav")
	case "video":
		args = append(append(args, video...), "-c:v", "mpeg4", "-f", "matroska")
	default:
		t.Fatalf("unknown clip kind %q", kind)
	}
	args = append(args, out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("making clip: %v\n%s", err, b)
	}
	return out
}
```

- [ ] **Step 2: Write failing probe tests** — `internal/ffmpeg/probe_test.go`

```go
package ffmpeg_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/testutil"
)

const goodJSON = `{"format":{"format_name":"matroska,webm","duration":"12.5"},
 "streams":[{"codec_type":"video","codec_name":"vp9","width":1920,"height":1080,"disposition":{"attached_pic":0}},
            {"codec_type":"audio","codec_name":"opus"}]}`

var lim = ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 8192 * 4320, MaxStreams: 16}

func TestParseProbeAndFlags(t *testing.T) {
	p, err := ffmpeg.ParseProbe([]byte(goodJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasVideo() || !p.HasAudio() || p.DurationSeconds() != 12.5 {
		t.Fatalf("bad parse %+v", p)
	}
	if err := p.Validate(lim); err != nil {
		t.Fatal(err)
	}
}

func TestCoverArtIsNotVideo(t *testing.T) {
	p, _ := ffmpeg.ParseProbe([]byte(`{"format":{"format_name":"mp3","duration":"3"},
	 "streams":[{"codec_type":"audio","codec_name":"mp3"},
	            {"codec_type":"video","codec_name":"mjpeg","width":500,"height":500,"disposition":{"attached_pic":1}}]}`))
	if p.HasVideo() {
		t.Fatal("attached picture must not count as video")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"concat":    `{"format":{"format_name":"concat","duration":"1"},"streams":[{"codec_type":"audio"}]}`,
		"hls":       `{"format":{"format_name":"hls,applehttp","duration":"1"},"streams":[{"codec_type":"audio"}]}`,
		"image2":    `{"format":{"format_name":"image2","duration":"1"},"streams":[{"codec_type":"video","width":1,"height":1}]}`,
		"no streams": `{"format":{"format_name":"wav","duration":"1"},"streams":[]}`,
		"too long":  `{"format":{"format_name":"wav","duration":"999999"},"streams":[{"codec_type":"audio"}]}`,
		"huge":      `{"format":{"format_name":"mov,mp4","duration":"1"},"streams":[{"codec_type":"video","width":60000,"height":60000}]}`,
	}
	for name, js := range cases {
		p, err := ffmpeg.ParseProbe([]byte(js))
		if err != nil {
			t.Errorf("%s: parse: %v", name, err)
			continue
		}
		if err := p.Validate(lim); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	many := `{"format":{"format_name":"wav","duration":"1"},"streams":[` + strings.Repeat(`{"codec_type":"audio"},`, 20) + `{"codec_type":"audio"}]}`
	p, _ := ffmpeg.ParseProbe([]byte(many))
	if p.Validate(lim) == nil {
		t.Error("too many streams must be rejected")
	}
}

func TestProbeRealClip(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	clip := testutil.MakeClip(t, dir, "a.mkv", "av")
	p, _, err := ffmpeg.Probe(context.Background(), "ffprobe", clip, dir)
	if err != nil || !p.HasVideo() || !p.HasAudio() {
		t.Fatalf("probe: %+v %v", p, err)
	}
	if err := p.Validate(lim); err != nil {
		t.Fatal(err)
	}
}

func TestProbeRejectsGarbage(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	for name, body := range map[string]string{"x.mp4": "this is not media at all", "empty.mp4": ""} {
		path := filepath.Join(dir, name)
		_ = os.WriteFile(path, []byte(body), 0o600)
		_, _, err := ffmpeg.Probe(context.Background(), "ffprobe", path, dir)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if strings.Contains(err.Error(), dir) {
			t.Fatalf("%s: error leaks path: %v", name, err)
		}
	}
}

func TestProbeRejectsConcat(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "evil.mp4")
	_ = os.WriteFile(path, []byte("ffconcat version 1.0\nfile '/etc/passwd'\n"), 0o600)
	p, _, err := ffmpeg.Probe(context.Background(), "ffprobe", path, dir)
	if err == nil && p.Validate(lim) == nil {
		t.Fatal("ffconcat playlist must be rejected by probe or validation")
	}
}
```

- [ ] **Step 3: Run to fail** — `go test ./internal/ffmpeg/ -run 'Probe|Validate|CoverArt'` → FAIL (undefined)

- [ ] **Step 4: Implement** — `internal/ffmpeg/probe.go`

```go
package ffmpeg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Limits struct {
	MaxDurationSec float64
	MaxPixels      int64
	MaxStreams     int
}

type Stream struct {
	CodecType   string `json:"codec_type"`
	CodecName   string `json:"codec_name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

type ProbeResult struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Streams []Stream `json:"streams"`
}

// allowedFormats are plain media containers. Playlist/network/pattern demuxers
// (concat, hls, sdp, rtsp, image2, tee, lavfi, ...) are intentionally absent.
var allowedFormats = map[string]bool{
	"matroska": true, "webm": true, "mov": true, "mp4": true, "m4a": true, "3gp": true, "3g2": true, "mj2": true,
	"avi": true, "flv": true, "mpegts": true, "mpeg": true, "ogg": true, "wav": true, "flac": true, "mp3": true,
	"aac": true, "asf": true, "ac3": true, "eac3": true, "aiff": true, "caf": true, "ape": true, "wv": true,
	"mxf": true, "amr": true, "au": true, "w64": true, "ivf": true, "nut": true, "dts": true, "tta": true,
}

func ParseProbe(b []byte) (*ProbeResult, error) {
	var p ProbeResult
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, errors.New("unreadable probe output")
	}
	return &p, nil
}

func (p *ProbeResult) HasVideo() bool {
	for _, s := range p.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 {
			return true
		}
	}
	return false
}

func (p *ProbeResult) HasAudio() bool {
	for _, s := range p.Streams {
		if s.CodecType == "audio" {
			return true
		}
	}
	return false
}

func (p *ProbeResult) DurationSeconds() float64 {
	d, err := strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

func (p *ProbeResult) Validate(l Limits) error {
	ok := false
	for _, n := range strings.Split(p.Format.FormatName, ",") {
		if allowedFormats[strings.TrimSpace(n)] {
			ok = true
		}
	}
	if !ok {
		return errors.New("unsupported container format")
	}
	if len(p.Streams) == 0 {
		return errors.New("file contains no media streams")
	}
	if len(p.Streams) > l.MaxStreams {
		return fmt.Errorf("too many streams (limit %d)", l.MaxStreams)
	}
	if d := p.DurationSeconds(); l.MaxDurationSec > 0 && d > l.MaxDurationSec {
		return fmt.Errorf("duration exceeds limit of %.0f seconds", l.MaxDurationSec)
	}
	for _, s := range p.Streams {
		if int64(s.Width)*int64(s.Height) > l.MaxPixels {
			return errors.New("video dimensions exceed limit")
		}
	}
	return nil
}

func minimalEnv() []string { return []string{"PATH=" + os.Getenv("PATH")} }

// Probe runs ffprobe with a local-files-only protocol whitelist. dir is the working directory
// (use an empty scratch dir so relative references inside hostile files resolve to nothing).
// Errors never include ffprobe's stderr or file paths.
func Probe(ctx context.Context, bin, path, dir string) (*ProbeResult, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-v", "error", "-protocol_whitelist", "file",
		"-print_format", "json",
		"-show_entries", "format=format_name,duration:stream=codec_type,codec_name,width,height:stream_disposition=attached_pic",
		path)
	cmd.Dir = dir
	cmd.Env = minimalEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, errors.New("file is not a readable media file")
	}
	p, err := ParseProbe(out)
	if err != nil {
		return nil, nil, errors.New("file is not a readable media file")
	}
	return p, out, nil
}
```

- [ ] **Step 5: Run to pass** — `go test ./internal/ffmpeg/ -run 'Probe|Validate|CoverArt' -v` → PASS

- [ ] **Step 6: Write failing runner tests** — `internal/ffmpeg/run_test.go`

```go
package ffmpeg_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/testutil"
)

func TestRunConverts(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	in := testutil.MakeClip(t, dir, "a.wav", "audio")
	out := filepath.Join(dir, "o.flac")
	var last float64
	tail, err := ffmpeg.Run(context.Background(), "ffmpeg",
		[]string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-i", in, "-c:a", "flac", "-f", "flac", "-progress", "pipe:1", "-nostats", out},
		dir, 1.0, func(p float64) { last = p })
	if err != nil {
		t.Fatalf("%v\n%s", err, tail)
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatal("no output")
	}
	if last < 0 || last > 99.9 {
		t.Fatalf("progress out of range: %v", last)
	}
}

func TestRunFailureTailIsSanitizable(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.wav")
	tail, err := ffmpeg.Run(context.Background(), "ffmpeg",
		[]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-i", missing, "-f", "null", "-"}, dir, 0, nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	clean := ffmpeg.Sanitize(tail, dir)
	if strings.Contains(clean, dir) || clean == "" {
		t.Fatalf("sanitize failed: %q", clean)
	}
}

func TestRunCancel(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ffmpeg.Run(ctx, "ffmpeg",
			[]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "sine=duration=600", "-f", "null", "-"}, dir, 600, nil)
		done <- err
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after cancel")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSanitizeCollapsesAndTruncates(t *testing.T) {
	got := ffmpeg.Sanitize("a\n\nb   /data/in/x  c", "/data")
	if got != "a b <data>/in/x c" {
		t.Fatalf("got %q", got)
	}
	if n := len(ffmpeg.Sanitize(strings.Repeat("x", 5000))); n > 500 {
		t.Fatalf("not truncated: %d", n)
	}
}
```

- [ ] **Step 7: Run to fail; implement** — `internal/ffmpeg/run.go`

Run: `go test ./internal/ffmpeg/ -run 'Run|Sanitize'` → FAIL (undefined)

```go
package ffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type tailBuffer struct {
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > t.max {
		b := t.buf.Bytes()
		t.buf = *bytes.NewBuffer(append([]byte(nil), b[len(b)-t.max:]...))
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return t.buf.String() }

// Run executes bin with args (no shell), reports progress parsed from `-progress pipe:1`,
// and returns the tail of stderr. Cancelling ctx kills the process.
func Run(ctx context.Context, bin string, args []string, dir string, durationSec float64, onProgress func(pct float64)) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = minimalEnv()
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	tail := &tailBuffer{max: 4096}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "out_time_us=")
		if !ok || onProgress == nil || durationSec <= 0 {
			continue
		}
		us, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || us < 0 {
			continue
		}
		pct := float64(us) / 1e6 / durationSec * 100
		if pct > 99.9 {
			pct = 99.9
		}
		onProgress(pct)
	}
	err = cmd.Wait()
	return tail.String(), err
}

// Sanitize makes FFmpeg output safe to show users: strips the given paths, collapses
// whitespace and truncates to 500 bytes.
func Sanitize(msg string, paths ...string) string {
	for _, p := range paths {
		if p != "" {
			msg = strings.ReplaceAll(msg, p, "<data>")
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return strings.ToValidUTF8(msg, "")
}
```

Run: `go test ./internal/ffmpeg/ -run 'Run|Sanitize' -v` → PASS

- [ ] **Step 8: Write failing capability tests** — `internal/ffmpeg/caps_test.go`

```go
package ffmpeg_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/testutil"
)

const encodersFixture = ` Encoders:
 V..... = Video
 ------
 V....D libsvtav1            SVT-AV1(Scalable Video Technology for AV1) encoder (codec av1)
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V....D av1_nvenc            NVIDIA NVENC av1 encoder (codec av1)
 V....D libvpx-vp9           libvpx VP9 (codec vp9)
 A....D aac                  AAC (Advanced Audio Coding)
 A....D libopus              libopus Opus (codec opus)
 A....D somethingelse        not ours
`

func TestParseEncoders(t *testing.T) {
	got := ffmpeg.ParseEncoders(encodersFixture)
	for _, want := range []string{"libsvtav1", "libx264", "aac", "libopus", "libvpx-vp9", "av1_nvenc", "somethingelse"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if slices.Contains(got, "Encoders:") || slices.Contains(got, "=") {
		t.Errorf("header leaked: %v", got)
	}
}

func TestDetectEncodersRespectsFlavor(t *testing.T) {
	testutil.RequireTools(t)
	lgpl, err := ffmpeg.DetectEncoders(context.Background(), "ffmpeg", "lgpl", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"libx264", "libx265", "aac"} {
		if slices.Contains(lgpl, banned) {
			t.Errorf("lgpl flavor must not advertise %s", banned)
		}
	}
	if !slices.Contains(lgpl, "pcm_s16le") {
		t.Errorf("pcm_s16le should always be available: %v", lgpl)
	}
	full, _ := ffmpeg.DetectEncoders(context.Background(), "ffmpeg", "full", "")
	if !slices.Contains(full, "aac") {
		t.Errorf("full flavor should advertise native aac: %v", full)
	}
}
```

- [ ] **Step 9: Run to fail; implement** — `internal/ffmpeg/caps.go`

```go
package ffmpeg

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/presets"
)

var hardwareEncoders = map[string]bool{"av1_nvenc": true, "av1_qsv": true, "av1_vaapi": true}

// ParseEncoders extracts encoder names from `ffmpeg -encoders` output.
func ParseEncoders(output string) []string {
	var out []string
	started := false
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "------") {
			started = true
			continue
		}
		if !started {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 && len(f[0]) == 6 {
			out = append(out, f[1])
		}
	}
	return out
}

// DetectEncoders lists the preset-relevant encoders this FFmpeg can really use under the
// given flavor. Hardware encoders must also pass a short trial encode.
func DetectEncoders(ctx context.Context, ffmpegBin, flavor, vaapiDevice string) ([]string, error) {
	cmd := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-encoders")
	cmd.Env = minimalEnv()
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, e := range presets.AllEncoders() {
		wanted[e] = true
	}
	var out []string
	for _, e := range ParseEncoders(string(b)) {
		if !wanted[e] || !presets.FlavorAllows(flavor, e) {
			continue
		}
		if hardwareEncoders[e] && !hwTrial(ctx, ffmpegBin, e, vaapiDevice) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func hwTrial(ctx context.Context, bin, enc, vaapiDevice string) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if enc == "av1_vaapi" {
		args = append(args, "-vaapi_device", vaapiDevice)
	}
	args = append(args, "-f", "lavfi", "-i", "testsrc=duration=0.2:size=256x256:rate=5")
	if enc == "av1_vaapi" {
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	args = append(args, "-c:v", enc, "-f", "null", "-")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = minimalEnv()
	return cmd.Run() == nil
}
```

- [ ] **Step 10: Run all ffmpeg tests and commit**

Run: `go test ./internal/ffmpeg/ -v` → PASS

```bash
git add -A && git commit -m "feat: ffprobe validation, process runner, encoder detection" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Worker

**Files:**
- Create: `internal/worker/worker.go`, `internal/worker/worker_test.go`, `cmd/convertflow-worker/main.go`

**Interfaces:**
- Consumes: `jobs.Store` (Task 2), `presets` (Task 3), `ffmpeg.{Probe,Run,BuildArgs,DetectEncoders,Sanitize,Limits}` (Tasks 4-5), `audit.Logger` (Task 1)
- Produces:
  - `worker.Worker{ID string; Store *jobs.Store; Audit *audit.Logger; FFmpegBin, FFprobeBin, Flavor, VAAPIDevice, OutDir, ScratchDir string; Limits ffmpeg.Limits; Poll, HeartbeatEvery time.Duration}`
  - `(*Worker).RegisterCapabilities(ctx) error`, `(*Worker).ProcessOne(ctx) (bool, error)`, `(*Worker).Run(ctx) error`, `(*Worker).Watch(ctx, id string, cancel context.CancelFunc, canceled *atomic.Bool)`

- [ ] **Step 1: Write failing worker tests** — `internal/worker/worker_test.go`

```go
package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/testutil"
	"github.com/ShifuRain/convertflow/internal/worker"
	"github.com/google/uuid"
)

type env struct {
	w     *worker.Worker
	store *jobs.Store
	dir   string
}

func setup(t *testing.T, lim ffmpeg.Limits) env {
	t.Helper()
	testutil.RequireTools(t)
	pool := dbtest.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, "out"), 0o700)
	store := jobs.NewStore(pool)
	w := &worker.Worker{
		ID: "w1", Store: store, Audit: audit.New(&strings.Builder{}),
		FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", Flavor: "full",
		OutDir: filepath.Join(dir, "out"), ScratchDir: t.TempDir(), Limits: lim,
		Poll: 50 * time.Millisecond, HeartbeatEvery: 50 * time.Millisecond,
	}
	if err := w.RegisterCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	return env{w, store, dir}
}

var defaultLimits = ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 8192 * 4320, MaxStreams: 16}

func (e env) enqueue(t *testing.T, src, preset string) string {
	t.Helper()
	id := uuid.NewString()
	in := filepath.Join(e.dir, "in", id)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(in, b, 0o600)
	if err := e.store.Create(context.Background(), &jobs.Job{ID: id, Filename: "x", InputPath: in, Options: jobs.Options{Preset: preset}}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWorkerConvertsAudio(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-flac")
	ok, err := e.w.ProcessOne(context.Background())
	if !ok || err != nil {
		t.Fatalf("ProcessOne: %v %v", ok, err)
	}
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusDone || j.EncoderUsed != "flac" || j.Progress != 100 {
		t.Fatalf("bad job %+v", j)
	}
	if st, err := os.Stat(j.OutputPath); err != nil || st.Size() == 0 {
		t.Fatalf("output missing: %v", err)
	}
	if _, err := os.Stat(j.InputPath); !os.IsNotExist(err) {
		t.Fatal("input should be removed after completion")
	}
}

func TestWorkerFailsOnGarbage(t *testing.T) {
	e := setup(t, defaultLimits)
	src := filepath.Join(t.TempDir(), "x.mp4")
	_ = os.WriteFile(src, []byte("not media"), 0o600)
	id := e.enqueue(t, src, "audio-wav")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || j.Error == "" || strings.Contains(j.Error, e.dir) {
		t.Fatalf("bad failure %+v", j)
	}
}

func TestWorkerFailsOnAudioOnlyForVideoPreset(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "video-vp9")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || !strings.Contains(j.Error, "no video stream") {
		t.Fatalf("bad failure %+v", j)
	}
}

func TestWorkerEnforcesLimits(t *testing.T) {
	e := setup(t, ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 100, MaxStreams: 16})
	src := testutil.MakeClip(t, t.TempDir(), "v.mkv", "video")
	id := e.enqueue(t, src, "video-vp9")
	_, _ = e.w.ProcessOne(context.Background())
	j, _ := e.store.Get(context.Background(), id)
	if j.Status != jobs.StatusFailed || !strings.Contains(j.Error, "dimensions") {
		t.Fatalf("bad failure %+v", j)
	}
}

func TestWorkerNoJob(t *testing.T) {
	e := setup(t, defaultLimits)
	ok, err := e.w.ProcessOne(context.Background())
	if ok || err != nil {
		t.Fatalf("got %v %v", ok, err)
	}
}

func TestWatchCancelsOnRequest(t *testing.T) {
	e := setup(t, defaultLimits)
	src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
	id := e.enqueue(t, src, "audio-wav")
	ctx := context.Background()
	if _, err := e.store.Claim(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	_ = e.store.RequestCancel(ctx, id)
	cctx, cancel := context.WithCancel(ctx)
	var canceled atomic.Bool
	done := make(chan struct{})
	go func() { e.w.Watch(cctx, id, cancel, &canceled); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not react to cancel request")
	}
	if !canceled.Load() || cctx.Err() == nil {
		t.Fatal("expected canceled flag and cancelled context")
	}
}
```

- [ ] **Step 2: Run to fail** — `scripts/test-db.sh ./internal/worker/` → FAIL (undefined)

- [ ] **Step 3: Implement** — `internal/worker/worker.go`

```go
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
	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
)

type Worker struct {
	ID             string
	Store          *jobs.Store
	Audit          *audit.Logger
	FFmpegBin      string
	FFprobeBin     string
	Flavor         string
	VAAPIDevice    string
	OutDir         string
	ScratchDir     string
	Limits         ffmpeg.Limits
	Poll           time.Duration
	HeartbeatEvery time.Duration

	mu       sync.RWMutex
	encoders map[string]bool
}

func (w *Worker) RegisterCapabilities(ctx context.Context) error {
	encs, err := ffmpeg.DetectEncoders(ctx, w.FFmpegBin, w.Flavor, w.VAAPIDevice)
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(encs))
	for _, e := range encs {
		set[e] = true
	}
	w.mu.Lock()
	w.encoders = set
	w.mu.Unlock()
	return w.Store.UpsertWorker(ctx, w.ID, encs)
}

func (w *Worker) snapshot() map[string]bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.encoders
}

// Run registers capabilities, refreshes them periodically and processes jobs until ctx ends.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.RegisterCapabilities(ctx); err != nil {
		return fmt.Errorf("detecting encoders: %w", err)
	}
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := w.RegisterCapabilities(ctx); err != nil {
					slog.Warn("capability refresh failed", "err", err)
				}
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
	j, err := w.Store.Claim(ctx, w.ID)
	if err != nil {
		return false, err
	}
	if j == nil {
		return false, nil
	}
	w.process(ctx, j)
	return true, nil
}

func (w *Worker) process(ctx context.Context, j *jobs.Job) {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var canceled atomic.Bool
	watchDone := make(chan struct{})
	go func() { w.Watch(jobCtx, j.ID, cancel, &canceled); close(watchDone) }()

	w.Audit.System("job_started", j.ID, "worker", w.ID, "attempt", j.Attempts)
	outPath, err := w.run(jobCtx, j)
	cancel()
	<-watchDone

	if ctx.Err() != nil { // worker shutting down: leave the job for requeue
		return
	}
	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer bgCancel()
	_ = os.Remove(j.InputPath)
	switch {
	case canceled.Load():
		_ = os.Remove(outPath)
		_ = w.Store.MarkCanceled(bg, j.ID)
		w.Audit.System("job_canceled", j.ID, "worker", w.ID)
	case err != nil:
		_ = os.Remove(outPath)
		_ = w.Store.Fail(bg, j.ID, err.Error())
		w.Audit.System("job_failed", j.ID, "worker", w.ID, "error", err.Error())
	default:
		_ = w.Store.Complete(bg, j.ID, outPath)
		w.Audit.System("job_finished", j.ID, "worker", w.ID, "encoder", j.EncoderUsed)
	}
}

// Watch heartbeats the job and triggers cancel when the API requested cancellation.
func (w *Worker) Watch(ctx context.Context, id string, cancel context.CancelFunc, canceled *atomic.Bool) {
	t := time.NewTicker(w.HeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if req, err := w.Store.Heartbeat(ctx, id); err == nil && req {
				canceled.Store(true)
				cancel()
				return
			}
		}
	}
}

// run performs probe → validate → encode. Returned errors are safe to show to users.
func (w *Worker) run(ctx context.Context, j *jobs.Job) (string, error) {
	pr, raw, err := ffmpeg.Probe(ctx, w.FFprobeBin, j.InputPath, w.ScratchDir)
	if err != nil {
		return "", err
	}
	if err := pr.Validate(w.Limits); err != nil {
		return "", err
	}
	_ = w.Store.SetProbe(ctx, j.ID, raw)

	preset, ok := presets.Get(j.Options.Preset)
	if !ok {
		return "", fmt.Errorf("unknown preset %q", j.Options.Preset)
	}
	if preset.Kind == presets.KindVideo && !pr.HasVideo() {
		return "", ffmpeg.ErrNoVideo
	}
	if preset.Kind == presets.KindAudio && !pr.HasAudio() {
		return "", ffmpeg.ErrNoAudio
	}
	cand, ok := preset.Resolve(w.snapshot())
	if !ok {
		return "", fmt.Errorf("no encoder available on this worker for preset %q", preset.ID)
	}
	encoder := cand.Audio
	if cand.Video != "" {
		encoder = cand.Video
	}
	if err := w.Store.SetRunning(ctx, j.ID, encoder); err != nil {
		return "", errors.New("internal error")
	}
	j.EncoderUsed = encoder

	outPath := filepath.Join(w.OutDir, j.ID+"."+preset.Ext)
	args, err := ffmpeg.BuildArgs(ffmpeg.Spec{
		Input: j.InputPath, Output: outPath, Preset: preset, Cand: cand, Opts: j.Options,
		HasVideo: pr.HasVideo(), HasAudio: pr.HasAudio(), VAAPIDevice: w.VAAPIDevice,
	})
	if err != nil {
		return "", err
	}
	var last time.Time
	tail, err := ffmpeg.Run(ctx, w.FFmpegBin, args, w.ScratchDir, pr.DurationSeconds(), func(p float64) {
		if time.Since(last) >= time.Second {
			last = time.Now()
			_ = w.Store.UpdateProgress(ctx, j.ID, p)
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			return outPath, ctx.Err()
		}
		return outPath, fmt.Errorf("transcode failed: %s", ffmpeg.Sanitize(tail, filepath.Dir(j.InputPath), w.OutDir, w.ScratchDir))
	}
	return outPath, nil
}
```

- [ ] **Step 4: Run to pass** — `scripts/test-db.sh ./internal/worker/ -v` → PASS

- [ ] **Step 5: Worker main** — `cmd/convertflow-worker/main.go`

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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
	scratch, err := os.MkdirTemp("", "cf-scratch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	host, _ := os.Hostname()
	w := &worker.Worker{
		ID: fmt.Sprintf("%s-%s", host, uuid.NewString()[:8]), Store: jobs.NewStore(pool), Audit: audit.New(os.Stdout),
		FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", Flavor: cfg.Flavor, VAAPIDevice: cfg.VAAPIDevice,
		OutDir: filepath.Join(cfg.DataDir, "out"), ScratchDir: scratch,
		Limits: ffmpeg.Limits{MaxDurationSec: cfg.MaxDurationSec, MaxPixels: cfg.MaxPixels, MaxStreams: cfg.MaxStreams},
		Poll:   cfg.WorkerPoll, HeartbeatEvery: 2e9,
	}
	slog.Info("worker starting", "id", w.ID, "flavor", cfg.Flavor)
	return w.Run(ctx)
}
```

- [ ] **Step 6: Build and commit**

Run: `go build ./... && go vet ./...` → no output

```bash
git add -A && git commit -m "feat: worker with probe, encode, cancel and heartbeat" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 7: API core — problem+json, middleware, upload

**Files:**
- Create: `internal/api/problem.go`, `internal/api/middleware.go`, `internal/api/upload.go`, `internal/api/server.go`, `internal/api/upload_test.go`, `internal/ui/embed.go` (placeholder handler, replaced in Task 10)

**Interfaces:**
- Consumes: `jobs.Store`, `config.Config`, `audit.Logger`, `presets`
- Produces:
  - `api.Deps{Store *jobs.Store; Cfg config.Config; Audit *audit.Logger; EventInterval time.Duration}`, `api.NewHandler(Deps) http.Handler`
  - `api.WriteProblem(w http.ResponseWriter, status int, title, detail string)`
  - `api.SanitizeFilename(string) string`
  - `ui.Handler() http.Handler`
  - Routes registered here: `POST /api/v1/jobs`. Task 8 adds the rest to the same `Server` struct (method `(*Server).routes(mux)` is extended there).
  - `jobView` struct and `toView(jobs.Job) jobView` defined in Task 8; Task 7's `createJob` returns the view via `s.writeJob(w, status, *jobs.Job)` — defined in this task as a minimal version and extended in Task 8 (see Step 5).

- [ ] **Step 1: Write failing tests** — `internal/api/upload_test.go`

```go
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
)

type fixture struct {
	h     http.Handler
	store *jobs.Store
	dir   string
}

func newFixture(t *testing.T, maxUpload int64) fixture {
	t.Helper()
	pool := dbtest.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, "out"), 0o700)
	store := jobs.NewStore(pool)
	_ = store.UpsertWorker(context.Background(), "w1", []string{"pcm_s16le", "libopus", "flac"})
	cfg := config.Config{DataDir: dir, MaxUploadBytes: maxUpload, Retention: time.Hour}
	h := api.NewHandler(api.Deps{Store: store, Cfg: cfg, Audit: audit.New(io.Discard), EventInterval: 20 * time.Millisecond})
	return fixture{h, store, dir}
}

type field struct{ name, value string }

func multipartBody(t *testing.T, fields []field, fileName string, file []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		_ = mw.WriteField(f.name, f.value)
	}
	if fileName != "" || file != nil {
		fw, _ := mw.CreateFormFile("file", fileName)
		_, _ = fw.Write(file)
	}
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func post(f fixture, body io.Reader, ctype string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/v1/jobs", body)
	r.Header.Set("Content-Type", ctype)
	r.RemoteAddr = "192.0.2.7:1234"
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func inFiles(t *testing.T, f fixture) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(f.dir, "in"))
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func problemOf(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("content-type %q", ct)
	}
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	return m
}

func TestUploadCreatesJob(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "song.mp3", []byte("data"))
	w := post(f, body, ct)
	if w.Code != 202 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	id, _ := v["id"].(string)
	if id == "" || v["status"] != "queued" || w.Header().Get("Location") != "/api/v1/jobs/"+id {
		t.Fatalf("bad response %v / %v", v, w.Header())
	}
	if names := inFiles(t, f); len(names) != 1 || names[0] != id {
		t.Fatalf("input files %v", names)
	}
	j, err := f.store.Get(context.Background(), id)
	if err != nil || j.Filename != "song.mp3" || j.Options.Preset != "audio-wav" {
		t.Fatalf("bad stored job %+v %v", j, err)
	}
}

func TestUploadOptionsJSON(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"options", `{"preset":"audio-flac"}`}}, "a.wav", []byte("data"))
	if w := post(f, body, ct); w.Code != 202 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	body, ct = multipartBody(t, []field{{"options", `{"preset":"audio-flac","evil":"1"}`}}, "a.wav", []byte("data"))
	if w := post(f, body, ct); w.Code != 422 {
		t.Fatalf("unknown option field must be 422, got %d", w.Code)
	}
}

func TestUploadRejectsFileBeforePreset(t *testing.T) {
	f := newFixture(t, 1<<20)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write([]byte("data"))
	_ = mw.WriteField("preset", "audio-wav")
	_ = mw.Close()
	w := post(f, &buf, mw.FormDataContentType())
	if w.Code != 400 {
		t.Fatalf("want 400 got %d", w.Code)
	}
	problemOf(t, w)
	if len(inFiles(t, f)) != 0 {
		t.Fatal("no file may be stored")
	}
}

func TestUploadRejectsMissingFile(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "", nil)
	if w := post(f, body, ct); w.Code != 400 {
		t.Fatalf("want 400 got %d", w.Code)
	}
}

func TestUploadRejectsEmptyFile(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "empty.mp4", []byte{})
	w := post(f, body, ct)
	if w.Code != 400 || len(inFiles(t, f)) != 0 {
		t.Fatalf("want 400 and no files, got %d %v", w.Code, inFiles(t, f))
	}
}

func TestUploadRejectsUnavailablePreset(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "video-av1"}}, "a.mp4", []byte("data"))
	w := post(f, body, ct)
	if w.Code != 422 {
		t.Fatalf("want 422 got %d", w.Code)
	}
	problemOf(t, w)
	body, ct = multipartBody(t, []field{{"preset", "nonexistent"}}, "a.mp4", []byte("data"))
	if w := post(f, body, ct); w.Code != 422 {
		t.Fatalf("unknown preset: want 422 got %d", w.Code)
	}
	if len(inFiles(t, f)) != 0 {
		t.Fatal("rejected uploads must not leave files")
	}
}

func TestUploadTooLarge(t *testing.T) {
	f := newFixture(t, 1000)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "big.wav", bytes.Repeat([]byte("x"), 200_000))
	w := post(f, body, ct)
	if w.Code != 413 {
		t.Fatalf("want 413 got %d", w.Code)
	}
	problemOf(t, w)
	if len(inFiles(t, f)) != 0 {
		t.Fatalf("partial file left behind: %v", inFiles(t, f))
	}
}

type failingReader struct{ r io.Reader }

func (f failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, errors.New("connection reset")
	}
	return n, err
}

func TestUploadAborted(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", bytes.Repeat([]byte("x"), 5000))
	truncated := failingReader{io.LimitReader(body, int64(body.Len()-200))}
	w := post(f, truncated, ct)
	if w.Code != 400 {
		t.Fatalf("want 400 got %d", w.Code)
	}
	if len(inFiles(t, f)) != 0 {
		t.Fatalf("partial file left behind: %v", inFiles(t, f))
	}
	jobsList, _ := f.store.List(context.Background(), 10)
	if len(jobsList) != 0 {
		t.Fatal("no job may be created for an aborted upload")
	}
}

func TestUploadHostileFilenameStaysInsideDataDir(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "../../evil\n\"x.mp4", []byte("data"))
	w := post(f, body, ct)
	if w.Code != 202 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	name, _ := v["filename"].(string)
	if strings.ContainsAny(name, "/\\\n\r\"") || name == "" {
		t.Fatalf("unsafe stored filename %q", name)
	}
	parent := filepath.Dir(f.dir)
	if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
		t.Fatal("path traversal succeeded")
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"song.mp3":          "song.mp3",
		"../../etc/passwd":  "passwd",
		`C:\Users\x\a.mp4`:  "a.mp4",
		"a\nb\r.mp4":        "ab.mp4",
		`we"ird.mp4`:        "weird.mp4",
		"":                  "upload",
		"..":                "upload",
		"   ":               "upload",
		"Müller – Ünï.mp4":  "Müller – Ünï.mp4",
	}
	for in, want := range cases {
		if got := api.SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	long := api.SanitizeFilename(strings.Repeat("ü", 400) + ".mp4")
	if len(long) > 200 || !strings.HasSuffix(long, ".mp4") && len(long) == 0 {
		t.Errorf("long name not truncated sensibly: %d bytes", len(long))
	}
}

func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t, 1<<20)
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	h := w.Header()
	if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") ||
		h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" ||
		h.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing security headers: %v", h)
	}
}

func TestAuditLogOnCreate(t *testing.T) {
	pool := dbtest.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	store := jobs.NewStore(pool)
	_ = store.UpsertWorker(context.Background(), "w1", []string{"pcm_s16le"})
	var logBuf bytes.Buffer
	h := api.NewHandler(api.Deps{Store: store, Cfg: config.Config{DataDir: dir, MaxUploadBytes: 1 << 20}, Audit: audit.New(&logBuf)})
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte("SECRET"))
	r := httptest.NewRequest("POST", "/api/v1/jobs", body)
	r.Header.Set("Content-Type", ct)
	r.RemoteAddr = "192.0.2.9:1"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !strings.Contains(logBuf.String(), `"event":"job_created"`) || !strings.Contains(logBuf.String(), "192.0.2.9") ||
		strings.Contains(logBuf.String(), "SECRET") {
		t.Fatalf("bad audit log: %s", logBuf.String())
	}
}
```

- [ ] **Step 2: Run to fail** — `scripts/test-db.sh ./internal/api/` → FAIL (undefined)

- [ ] **Step 3: Problem, middleware, UI placeholder**

`internal/api/problem.go`:
```go
package api

import (
	"encoding/json"
	"net/http"
)

// Problem is an RFC 9457 problem details object.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func WriteProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Type: "about:blank", Title: title, Status: status, Detail: detail})
}
```

`internal/api/middleware.go`:
```go
package api

import (
	"log/slog"
	"net/http"
)

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func securityHeaders(hsts bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		if hsts {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func noStoreAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic in handler", "panic", v, "path", r.URL.Path)
				WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
```

`internal/ui/embed.go` (replaced with a real embed in Task 10):
```go
// Package ui serves the embedded web UI.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

func Handler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
```

- [ ] **Step 4: Server skeleton** — `internal/api/server.go`

```go
// Package api implements the REST API, SSE stream and static UI hosting.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/ui"
)

type Deps struct {
	Store         *jobs.Store
	Cfg           config.Config
	Audit         *audit.Logger
	EventInterval time.Duration // SSE poll interval; default 1s
}

type Server struct {
	store *jobs.Store
	cfg   config.Config
	audit *audit.Logger
	every time.Duration
}

const liveWindow = 30 * time.Second // a worker is "live" if it was seen this recently

func NewHandler(d Deps) http.Handler {
	s := &Server{store: d.Store, cfg: d.Cfg, audit: d.Audit, every: d.EventInterval}
	if s.every <= 0 {
		s.every = time.Second
	}
	mux := http.NewServeMux()
	s.routes(mux)
	mux.Handle("/", ui.Handler())
	return recoverer(securityHeaders(d.Cfg.TLSCertFile != "", noStoreAPI(mux)))
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/jobs", s.createJob)
}

// jobView is the public representation of a job (never exposes server paths).
type jobView struct {
	ID          string          `json:"id"`
	Status      jobs.Status     `json:"status"`
	Filename    string          `json:"filename"`
	Preset      string          `json:"preset"`
	Progress    float64         `json:"progress"`
	EncoderUsed string          `json:"encoder_used"`
	Probe       json.RawMessage `json:"probe,omitempty"`
	Error       string          `json:"error,omitempty"`
	Attempts    int             `json:"attempts"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	FinishedAt  *time.Time      `json:"finished_at,omitempty"`
	OutputURL   string          `json:"output_url,omitempty"`
}

func toView(j jobs.Job) jobView {
	v := jobView{ID: j.ID, Status: j.Status, Filename: j.Filename, Preset: j.Options.Preset,
		Progress: j.Progress, EncoderUsed: j.EncoderUsed, Probe: j.Probe, Error: j.Error,
		Attempts: j.Attempts, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, FinishedAt: j.FinishedAt}
	if j.Status == jobs.StatusDone {
		v.OutputURL = "/api/v1/jobs/" + j.ID + "/output"
	}
	return v
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 5: Upload handler** — `internal/api/upload.go`

```go
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
	"github.com/google/uuid"
)

const multipartSlack = 64 << 10

var (
	errTooLarge = errors.New("file exceeds the configured upload limit")
	errEmpty    = errors.New("file is empty")
)

// SanitizeFilename reduces a client-supplied filename to a safe display name.
func SanitizeFilename(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if s == "" {
		return "upload"
	}
	for len(s) > 200 {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

func readSmall(r io.Reader, max int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > max {
		return nil, errors.New("field too large")
	}
	return b, nil
}

func (s *Server) uploadError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, errTooLarge) || errors.As(err, &mbe):
		WriteProblem(w, http.StatusRequestEntityTooLarge, "Upload too large", "The file exceeds the configured limit.")
	case errors.Is(err, errEmpty):
		WriteProblem(w, http.StatusBadRequest, "Empty file", "The uploaded file is empty.")
	default:
		WriteProblem(w, http.StatusBadRequest, "Upload failed", "The upload was interrupted or malformed.")
	}
}

// checkOptions validates options and confirms a live worker can run the preset.
func (s *Server) checkOptions(r *http.Request, o jobs.Options) (string, bool) {
	if err := o.Validate(); err != nil {
		return err.Error(), false
	}
	enc, err := s.store.LiveEncoders(r.Context(), liveWindow)
	if err != nil {
		return "could not determine worker capabilities", false
	}
	p, _ := presets.Get(o.Preset)
	if _, ok := p.Resolve(enc); !ok {
		return "preset " + o.Preset + " is not available on any running worker", false
	}
	return "", true
}

// createJob streams a multipart upload to disk. Fields "preset" and/or "options" (JSON) must
// precede the "file" field so invalid requests are rejected before any bytes are stored.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+multipartSlack)
	mr, err := r.MultipartReader()
	if err != nil {
		WriteProblem(w, http.StatusBadRequest, "Bad request", "expected multipart/form-data")
		return
	}
	id := uuid.NewString()
	inPath := filepath.Join(s.cfg.DataDir, "in", id)
	var opts jobs.Options
	var filename string
	stored, created := false, false
	defer func() {
		if stored && !created {
			_ = os.Remove(inPath)
		}
	}()

	for !stored {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadError(w, err)
			return
		}
		switch part.FormName() {
		case "preset":
			b, err := readSmall(part, 128)
			if err != nil {
				WriteProblem(w, http.StatusBadRequest, "Bad request", "invalid preset field")
				return
			}
			opts.Preset = strings.TrimSpace(string(b))
		case "options":
			b, err := readSmall(part, 4096)
			if err != nil {
				WriteProblem(w, http.StatusBadRequest, "Bad request", "invalid options field")
				return
			}
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&opts); err != nil {
				WriteProblem(w, http.StatusUnprocessableEntity, "Invalid options", "options must be a JSON object with known fields")
				return
			}
		case "file":
			if opts.Preset == "" {
				WriteProblem(w, http.StatusBadRequest, "Bad request", "send the preset (or options) field before the file field")
				return
			}
			if msg, ok := s.checkOptions(r, opts); !ok {
				WriteProblem(w, http.StatusUnprocessableEntity, "Invalid options", msg)
				return
			}
			filename = SanitizeFilename(part.FileName())
			f, err := os.OpenFile(inPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
				return
			}
			stored = true
			n, err := io.Copy(f, io.LimitReader(part, s.cfg.MaxUploadBytes+1))
			_ = f.Close()
			switch {
			case err != nil:
				s.uploadError(w, err)
				return
			case n > s.cfg.MaxUploadBytes:
				s.uploadError(w, errTooLarge)
				return
			case n == 0:
				s.uploadError(w, errEmpty)
				return
			}
		}
	}
	if !stored {
		WriteProblem(w, http.StatusBadRequest, "Bad request", "missing file field")
		return
	}
	j := &jobs.Job{ID: id, Filename: filename, InputPath: inPath, Options: opts}
	if err := s.store.Create(r.Context(), j); err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	created = true
	s.audit.Event(r, "job_created", id, "preset", opts.Preset)
	got, err := s.store.Get(r.Context(), id)
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+id)
	writeJSON(w, http.StatusAccepted, toView(*got))
}
```

Note on the ordering of `stored`: the deferred cleanup removes the file for any early return after the file was opened (`stored` true, `created` false), including errors from `io.Copy`, over-limit and empty uploads.

- [ ] **Step 6: Run to pass**

Run: `mkdir -p internal/ui/dist && scripts/test-db.sh ./internal/api/ -v`
Expected: PASS. If `TestUploadTooLarge` returns 400 instead of 413, check that `uploadError` unwraps `*http.MaxBytesError` (it uses `errors.As`) and that the in-handler size count triggers `errTooLarge` first.

- [ ] **Step 7: API main** — `cmd/convertflow-api/main.go` (janitor is wired in Task 9; add it there)

```go
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db"
	"github.com/ShifuRain/convertflow/internal/jobs"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
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
	for _, d := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o700); err != nil {
			return err
		}
	}
	store := jobs.NewStore(pool)
	al := audit.New(os.Stdout)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           api.NewHandler(api.Deps{Store: store, Cfg: cfg, Audit: al}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("api listening", "addr", cfg.ListenAddr, "tls", cfg.TLSCertFile != "")
		if cfg.TLSCertFile != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// healthcheck is used by the container HEALTHCHECK (distroless has no curl).
func healthcheck() int {
	addr := os.Getenv("CONVERTFLOW_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	client := &http.Client{Timeout: 3 * time.Second}
	if os.Getenv("CONVERTFLOW_TLS_CERT_FILE") != "" {
		scheme = "https"
		// #nosec G402 -- loopback health probe against our own listener
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	}
	resp, err := client.Get(fmt.Sprintf("%s://%s/healthz", scheme, net.JoinHostPort(host, port)))
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
```

- [ ] **Step 8: Build and commit**

Run: `go build ./... && go vet ./...` → no output

```bash
git add -A && git commit -m "feat(api): problem+json, security headers, streaming upload, api main" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 8: API read endpoints, SSE, metrics, OpenAPI

**Files:**
- Create: `internal/api/read.go`, `internal/api/read_test.go`, `internal/api/openapi.go`, `internal/api/openapi.json`
- Modify: `internal/api/server.go` (`routes`)

**Interfaces:**
- Consumes: `fixture`, `newFixture`, `post`, `multipartBody` helpers from `upload_test.go` (same package `api_test`); `jobs.Store`; `toView`, `writeJSON`, `WriteProblem`, `liveWindow`
- Produces routes: `GET /api/v1/jobs`, `GET /api/v1/jobs/{id}`, `GET /api/v1/jobs/{id}/events`, `GET /api/v1/jobs/{id}/output`, `DELETE /api/v1/jobs/{id}`, `GET /api/v1/presets`, `GET /healthz`, `GET /metrics`, `GET /api/openapi.json`

- [ ] **Step 1: Write failing tests** — `internal/api/read_test.go`

```go
package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
)

func get(f fixture, path string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func del(f fixture, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest("DELETE", path, nil))
	return w
}

func seedDone(t *testing.T, f fixture, filename, preset, content string) string {
	t.Helper()
	id := uuid.NewString()
	in := filepath.Join(f.dir, "in", id)
	out := filepath.Join(f.dir, "out", id+".wav")
	_ = os.WriteFile(out, []byte(content), 0o600)
	ctx := context.Background()
	if err := f.store.Create(ctx, &jobs.Job{ID: id, Filename: filename, InputPath: in, Options: jobs.Options{Preset: preset}}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Complete(ctx, id, out); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestGetAndListJobs(t *testing.T) {
	f := newFixture(t, 1<<20)
	id := seedDone(t, f, "a.mp3", "audio-wav", "x")
	w := get(f, "/api/v1/jobs/"+id)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if v["status"] != "done" || v["output_url"] != "/api/v1/jobs/"+id+"/output" {
		t.Fatalf("bad view %v", v)
	}
	if strings.Contains(w.Body.String(), f.dir) {
		t.Fatal("server paths must not be exposed")
	}
	var list []map[string]any
	_ = json.Unmarshal(get(f, "/api/v1/jobs").Body.Bytes(), &list)
	if len(list) != 1 {
		t.Fatalf("want 1 job, got %v", list)
	}
}

func TestGetJobNotFoundAndBadID(t *testing.T) {
	f := newFixture(t, 1<<20)
	for _, p := range []string{"/api/v1/jobs/" + uuid.NewString(), "/api/v1/jobs/not-a-uuid"} {
		w := get(f, p)
		if w.Code != 404 {
			t.Errorf("%s: want 404 got %d", p, w.Code)
		}
		problemOf(t, w)
	}
}

func TestOutputDisposition(t *testing.T) {
	f := newFixture(t, 1<<20)
	id := seedDone(t, f, "My Clip – Ünï.mp4", "audio-wav", "RIFFDATA")
	w := get(f, "/api/v1/jobs/"+id+"/output")
	if w.Code != 200 || w.Body.String() != "RIFFDATA" {
		t.Fatalf("status %d body %q", w.Code, w.Body)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment") || strings.ContainsAny(cd, "\n\r") || !strings.Contains(cd, ".wav") {
		t.Fatalf("bad disposition %q", cd)
	}
	if w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("content-type %q", w.Header().Get("Content-Type"))
	}
	w = get(f, "/api/v1/jobs/"+id+"/output", "Range", "bytes=0-3")
	if w.Code != 206 || w.Body.String() != "RIFF" {
		t.Fatalf("range: %d %q", w.Code, w.Body)
	}
}

func TestOutputNotReady(t *testing.T) {
	f := newFixture(t, 1<<20)
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte("x"))
	var v map[string]any
	_ = json.Unmarshal(post(f, body, ct).Body.Bytes(), &v)
	w := get(f, "/api/v1/jobs/"+v["id"].(string)+"/output")
	if w.Code != 409 {
		t.Fatalf("want 409 got %d", w.Code)
	}
	problemOf(t, w)
}

func TestDeleteSemantics(t *testing.T) {
	f := newFixture(t, 1<<20)
	ctx := context.Background()
	// done job: row and files removed
	id := seedDone(t, f, "a.mp3", "audio-wav", "x")
	out := filepath.Join(f.dir, "out", id+".wav")
	if w := del(f, "/api/v1/jobs/"+id); w.Code != 204 {
		t.Fatalf("want 204 got %d", w.Code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("output should be deleted")
	}
	if _, err := f.store.Get(ctx, id); err != jobs.ErrNotFound {
		t.Fatal("row should be deleted")
	}
	// running job: cancel requested, 202
	rid := uuid.NewString()
	_ = f.store.Create(ctx, &jobs.Job{ID: rid, Filename: "r", InputPath: "/x", Options: jobs.Options{Preset: "audio-wav"}})
	_, _ = f.store.Claim(ctx, "w1")
	if w := del(f, "/api/v1/jobs/"+rid); w.Code != 202 {
		t.Fatalf("want 202 got %d", w.Code)
	}
	if cancel, _ := f.store.Heartbeat(ctx, rid); !cancel {
		t.Fatal("cancel flag should be set")
	}
	if w := del(f, "/api/v1/jobs/"+uuid.NewString()); w.Code != 404 {
		t.Fatalf("want 404 got %d", w.Code)
	}
}

func TestPresetsListsOnlyRunnable(t *testing.T) {
	f := newFixture(t, 1<<20)
	var ps []map[string]any
	_ = json.Unmarshal(get(f, "/api/v1/presets").Body.Bytes(), &ps)
	ids := map[string]bool{}
	for _, p := range ps {
		ids[p["id"].(string)] = true
	}
	if !ids["audio-wav"] || !ids["audio-flac"] || !ids["audio-opus"] || ids["video-av1"] {
		t.Fatalf("unexpected presets %v", ids)
	}
}

func TestHealthzAndMetrics(t *testing.T) {
	f := newFixture(t, 1<<20)
	if w := get(f, "/healthz"); w.Code != 200 {
		t.Fatalf("healthz %d", w.Code)
	}
	body, ct := multipartBody(t, []field{{"preset", "audio-wav"}}, "a.wav", []byte("x"))
	post(f, body, ct)
	m := get(f, "/metrics").Body.String()
	if !strings.Contains(m, `convertflow_jobs{status="queued"} 1`) || !strings.Contains(m, "convertflow_workers_live 1") {
		t.Fatalf("bad metrics:\n%s", m)
	}
}

func TestOpenAPIServed(t *testing.T) {
	f := newFixture(t, 1<<20)
	w := get(f, "/api/openapi.json")
	var doc map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &doc) != nil || doc["openapi"] != "3.1.0" {
		t.Fatalf("bad openapi: %d", w.Code)
	}
}

func TestSSEStreamsUntilDone(t *testing.T) {
	f := newFixture(t, 1<<20)
	ctx := context.Background()
	id := uuid.NewString()
	_ = f.store.Create(ctx, &jobs.Job{ID: id, Filename: "a", InputPath: "/x", Options: jobs.Options{Preset: "audio-wav"}})
	srv := httptest.NewServer(f.h)
	defer srv.Close()

	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = f.store.Claim(ctx, "w1")
		_ = f.store.SetRunning(ctx, id, "pcm_s16le")
		_ = f.store.UpdateProgress(ctx, id, 50)
		time.Sleep(100 * time.Millisecond)
		_ = f.store.Complete(ctx, id, "/out")
	}()

	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/jobs/"+id+"/events", nil)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(cctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("content-type %q", resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	sawDone := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"status":"done"`) {
			sawDone = true
			break
		}
	}
	if !sawDone {
		t.Fatal("stream ended without a done event")
	}
}
```

- [ ] **Step 2: Run to fail** — `scripts/test-db.sh ./internal/api/ -run 'GetAnd|Output|Delete|Presets|Health|OpenAPI|SSE|NotFound'` → FAIL (404s)

- [ ] **Step 3: Implement handlers** — `internal/api/read.go`

```go
package api

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
	"github.com/google/uuid"
)

// jobID validates the {id} path value; it writes a 404 and returns false when invalid.
func jobID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
		return "", false
	}
	return id, true
}

func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*jobs.Job, bool) {
	id, ok := jobID(w, r)
	if !ok {
		return nil, false
	}
	j, err := s.store.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
		return nil, false
	}
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return nil, false
	}
	return j, true
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	js, err := s.store.List(r.Context(), 200)
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	out := make([]jobView, 0, len(js))
	for _, j := range js {
		out = append(out, toView(j))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	if j, ok := s.lookup(w, r); ok {
		writeJSON(w, http.StatusOK, toView(*j))
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "streaming unsupported")
		return
	}
	if _, err := s.store.Get(r.Context(), id); err != nil {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	for {
		j, err := s.store.Get(r.Context(), id)
		if err != nil {
			return // deleted or request cancelled
		}
		b, _ := jsonMarshal(toView(*j))
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		if j.Status.Terminal() {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(s.every):
		}
	}
}

func (s *Server) output(w http.ResponseWriter, r *http.Request) {
	j, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if j.Status != jobs.StatusDone {
		WriteProblem(w, http.StatusConflict, "Output not ready", "job status is "+string(j.Status))
		return
	}
	f, err := os.Open(j.OutputPath)
	if err != nil {
		WriteProblem(w, http.StatusGone, "Output removed", "the result is no longer available")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	p, _ := presets.Get(j.Options.Preset)
	base := strings.TrimSuffix(j.Filename, filepath.Ext(j.Filename))
	name := SanitizeFilename(base + "." + p.Ext)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := jobID(w, r)
	if !ok {
		return
	}
	j, err := s.store.Delete(r.Context(), id)
	switch {
	case err == nil:
		_ = os.Remove(j.InputPath)
		_ = os.Remove(j.OutputPath)
		s.audit.Event(r, "job_deleted", id)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, jobs.ErrNotFound):
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
	case errors.Is(err, jobs.ErrBusy):
		if err := s.store.RequestCancel(r.Context(), id); err != nil {
			WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
			return
		}
		s.audit.Event(r, "job_cancel_requested", id)
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": "canceling"})
	default:
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
	}
}

func (s *Server) listPresets(w http.ResponseWriter, r *http.Request) {
	enc, err := s.store.LiveEncoders(r.Context(), liveWindow)
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	type pv struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Kind  string `json:"kind"`
		Ext   string `json:"ext"`
	}
	out := []pv{}
	for _, p := range presets.Available(enc) {
		out = append(out, pv{p.ID, p.Label, string(p.Kind), p.Ext})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		WriteProblem(w, http.StatusServiceUnavailable, "Unhealthy", "database unreachable")
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.Counts(r.Context())
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	live, _ := s.store.LiveWorkers(r.Context(), liveWindow)
	var b strings.Builder
	b.WriteString("# HELP convertflow_jobs Jobs by status.\n# TYPE convertflow_jobs gauge\n")
	statuses := []string{"queued", "probing", "running", "done", "failed", "canceled"}
	sort.Strings(statuses)
	for _, st := range statuses {
		fmt.Fprintf(&b, "convertflow_jobs{status=%q} %d\n", st, counts[st])
	}
	b.WriteString("# HELP convertflow_workers_live Workers seen recently.\n# TYPE convertflow_workers_live gauge\n")
	fmt.Fprintf(&b, "convertflow_workers_live %d\n", live)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}
```

Add the missing imports to `read.go`: `"encoding/json"` and `"time"`, and define `var jsonMarshal = json.Marshal` (or call `json.Marshal` directly and delete `jsonMarshal`).

- [ ] **Step 4: OpenAPI** — `internal/api/openapi.go`

```go
package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openapiJSON []byte

func (s *Server) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openapiJSON)
}
```

`internal/api/openapi.json`:
```json
{
  "openapi": "3.1.0",
  "info": {"title": "ConvertFlow API", "version": "1.0.0", "description": "Transcode video and audio. No authentication; intended for isolated networks."},
  "paths": {
    "/api/v1/jobs": {
      "post": {"summary": "Upload a file and create a job (multipart: preset or options first, then file)", "responses": {"202": {"description": "Job accepted"}, "400": {"description": "Malformed request"}, "413": {"description": "File too large"}, "422": {"description": "Invalid or unavailable preset/options"}}},
      "get": {"summary": "List recent jobs", "responses": {"200": {"description": "Array of jobs"}}}
    },
    "/api/v1/jobs/{id}": {
      "get": {"summary": "Get job status and progress", "responses": {"200": {"description": "Job"}, "404": {"description": "Unknown job"}}},
      "delete": {"summary": "Delete an idle job and its files, or request cancellation of an active job", "responses": {"204": {"description": "Deleted"}, "202": {"description": "Cancellation requested"}, "404": {"description": "Unknown job"}}}
    },
    "/api/v1/jobs/{id}/events": {"get": {"summary": "Server-Sent Events progress stream until the job finishes", "responses": {"200": {"description": "text/event-stream"}}}},
    "/api/v1/jobs/{id}/output": {"get": {"summary": "Download the result (supports Range)", "responses": {"200": {"description": "File"}, "206": {"description": "Partial content"}, "409": {"description": "Job not done"}, "410": {"description": "Output removed"}}}},
    "/api/v1/presets": {"get": {"summary": "Presets runnable on the currently running workers", "responses": {"200": {"description": "Array of presets"}}}},
    "/healthz": {"get": {"summary": "Liveness and database check", "responses": {"200": {"description": "ok"}, "503": {"description": "database unreachable"}}}},
    "/metrics": {"get": {"summary": "Prometheus metrics", "responses": {"200": {"description": "Text exposition format"}}}}
  }
}
```

- [ ] **Step 5: Register routes** — in `internal/api/server.go` replace `routes`:

```go
func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/jobs", s.createJob)
	mux.HandleFunc("GET /api/v1/jobs", s.listJobs)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.getJob)
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.deleteJob)
	mux.HandleFunc("GET /api/v1/jobs/{id}/events", s.events)
	mux.HandleFunc("GET /api/v1/jobs/{id}/output", s.output)
	mux.HandleFunc("GET /api/v1/presets", s.listPresets)
	mux.HandleFunc("GET /api/openapi.json", s.openapi)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metrics)
}
```

- [ ] **Step 6: Run to pass and commit**

Run: `scripts/test-db.sh ./internal/api/ -v` → PASS

```bash
git add -A && git commit -m "feat(api): job read/delete, SSE, output download, presets, metrics, openapi" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Janitor (retention, stale requeue, orphan sweep)

**Files:**
- Create: `internal/janitor/janitor.go`, `internal/janitor/janitor_test.go`
- Modify: `cmd/convertflow-api/main.go` (start the janitor)

**Interfaces:**
- Consumes: `jobs.Store.{Expired,Delete,RequeueStale,Exists}`, `audit.Logger.System`
- Produces: `janitor.Janitor{Store *jobs.Store; Audit *audit.Logger; DataDir string; Retention, StaleAfter, Interval time.Duration; MaxAttempts int}`, `(*Janitor).Sweep(ctx) error`, `(*Janitor).Run(ctx)`

- [ ] **Step 1: Write failing tests** — `internal/janitor/janitor_test.go`

```go
package janitor_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/janitor"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
)

func TestSweepRemovesExpiredJobsAndOrphans(t *testing.T) {
	pool := dbtest.New(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, "out"), 0o700)

	// expired done job with output file
	id := uuid.NewString()
	out := filepath.Join(dir, "out", id+".wav")
	_ = os.WriteFile(out, []byte("x"), 0o600)
	_ = store.Create(ctx, &jobs.Job{ID: id, Filename: "a", InputPath: filepath.Join(dir, "in", id), Options: jobs.Options{Preset: "audio-wav"}})
	_ = store.Complete(ctx, id, out)
	_, _ = pool.Exec(ctx, `UPDATE jobs SET finished_at = now() - interval '2 days'`)

	// orphans: one old, one fresh, one not a uuid
	oldOrphan := filepath.Join(dir, "in", uuid.NewString())
	freshOrphan := filepath.Join(dir, "in", uuid.NewString())
	other := filepath.Join(dir, "in", "README")
	for _, p := range []string{oldOrphan, freshOrphan, other} {
		_ = os.WriteFile(p, []byte("x"), 0o600)
	}
	past := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(oldOrphan, past, past)
	_ = os.Chtimes(other, past, past)

	j := &janitor.Janitor{Store: store, Audit: audit.New(io.Discard), DataDir: dir,
		Retention: 24 * time.Hour, StaleAfter: time.Minute, MaxAttempts: 3, Interval: time.Hour}
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("expired output should be removed")
	}
	if ok, _ := store.Exists(ctx, id); ok {
		t.Error("expired job row should be removed")
	}
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Error("old orphan should be removed")
	}
	if _, err := os.Stat(freshOrphan); err != nil {
		t.Error("fresh orphan must be kept (upload may be in progress)")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("files that are not job ids must never be touched")
	}
}

func TestSweepRequeuesStaleJobs(t *testing.T) {
	pool := dbtest.New(t)
	store := jobs.NewStore(pool)
	ctx := context.Background()
	id := uuid.NewString()
	_ = store.Create(ctx, &jobs.Job{ID: id, Filename: "a", InputPath: "/x", Options: jobs.Options{Preset: "audio-wav"}})
	_, _ = store.Claim(ctx, "dead-worker")
	_, _ = pool.Exec(ctx, `UPDATE jobs SET heartbeat_at = now() - interval '10 minutes'`)
	j := &janitor.Janitor{Store: store, Audit: audit.New(io.Discard), DataDir: t.TempDir(),
		Retention: time.Hour, StaleAfter: time.Minute, MaxAttempts: 3}
	if err := j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(ctx, id)
	if got.Status != jobs.StatusQueued {
		t.Fatalf("want queued, got %s", got.Status)
	}
}
```

- [ ] **Step 2: Run to fail; implement** — `internal/janitor/janitor.go`

Run: `scripts/test-db.sh ./internal/janitor/` → FAIL (undefined)

```go
// Package janitor enforces retention, requeues jobs of dead workers and removes orphaned files.
package janitor

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
)

type Janitor struct {
	Store       *jobs.Store
	Audit       *audit.Logger
	DataDir     string
	Retention   time.Duration
	StaleAfter  time.Duration
	Interval    time.Duration
	MaxAttempts int
}

func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := j.Sweep(ctx); err != nil {
			slog.Error("janitor sweep", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep performs one maintenance pass. Files are removed with a plain unlink; use an encrypted
// volume if remnants on the underlying storage are a concern (see docs/operations.md).
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
		deleted, err := j.Store.Delete(ctx, e.ID)
		if err != nil {
			continue
		}
		_ = os.Remove(deleted.InputPath)
		_ = os.Remove(deleted.OutputPath)
		j.Audit.System("job_expired", e.ID)
	}
	for _, sub := range []string{"in", "out"} {
		j.sweepOrphans(ctx, filepath.Join(j.DataDir, sub))
	}
	return nil
}

func (j *Janitor) sweepOrphans(ctx context.Context, dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		id := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		if _, err := uuid.Parse(id); err != nil {
			continue // not ours
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < j.Retention {
			continue
		}
		if exists, err := j.Store.Exists(ctx, id); err != nil || exists {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
		j.Audit.System("orphan_removed", id)
	}
}
```

- [ ] **Step 3: Run to pass; wire into API main**

Run: `scripts/test-db.sh ./internal/janitor/ -v` → PASS

In `cmd/convertflow-api/main.go`, after `al := audit.New(os.Stdout)` add (and import `internal/janitor`):
```go
	go (&janitor.Janitor{Store: store, Audit: al, DataDir: cfg.DataDir, Retention: cfg.Retention,
		StaleAfter: cfg.StaleAfter, MaxAttempts: cfg.MaxAttempts, Interval: time.Minute}).Run(ctx)
```

- [ ] **Step 4: Build and commit**

Run: `go build ./... && go vet ./...`

```bash
git add -A && git commit -m "feat: janitor for retention, stale requeue and orphan sweep" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Dark UI (TypeScript, embedded)

**Files:**
- Create: `ui/package.json`, `ui/tsconfig.json`, `ui/index.html`, `ui/docs.html`, `ui/src/main.ts`, `ui/src/docs.ts`, `ui/src/style.css`, `internal/ui/ui_test.go`
- Modify: none (`internal/ui/embed.go` from Task 7 already embeds `dist`)

**Interfaces:**
- Consumes: REST API of Tasks 7–8 (`jobView` JSON, `/api/v1/presets`, SSE)
- Produces: built assets in `internal/ui/dist/` (`index.html`, `docs.html`, `main.js`, `docs.js`, `style.css`), `make ui` target.

- [ ] **Step 1: Write the failing UI test** — `internal/ui/ui_test.go`

```go
package ui_test

import (
	"io/fs"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ui"
)

func TestIndexServed(t *testing.T) {
	w := httptest.NewRecorder()
	ui.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "main.js") {
		t.Fatalf("index not served (run `make ui` first): %d", w.Code)
	}
}

func TestNoInlineScriptsOrExternalReferences(t *testing.T) {
	root := os.DirFS("dist")
	ext := regexp.MustCompile(`https?://`)
	allowed := "http://www.w3.org/"
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return err
		}
		b, _ := fs.ReadFile(root, p)
		s := strings.ReplaceAll(string(b), allowed, "")
		if ext.MatchString(s) {
			t.Errorf("%s references an external URL (air-gap violation)", p)
		}
		if strings.HasSuffix(p, ".html") {
			if regexp.MustCompile(`<script(\s[^>]*)?>[^<]`).MatchString(s) || strings.Contains(s, " style=") || strings.Contains(s, "<style") {
				t.Errorf("%s contains inline script/style (violates CSP)", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run to fail** — `go test ./internal/ui/` → FAIL (dist empty)

- [ ] **Step 3: Build config**

`ui/package.json`:
```json
{
  "name": "convertflow-ui",
  "private": true,
  "license": "Apache-2.0",
  "scripts": {
    "build": "tsc --noEmit && esbuild src/main.ts src/docs.ts --bundle --minify --target=es2020 --outdir=../internal/ui/dist && cp index.html docs.html src/style.css ../internal/ui/dist/"
  },
  "devDependencies": {
    "esbuild": "^0.24.0",
    "typescript": "^5.6.0"
  }
}
```

`ui/tsconfig.json`:
```json
{
  "compilerOptions": {
    "target": "ES2020", "module": "ESNext", "moduleResolution": "Bundler",
    "strict": true, "noUnusedLocals": true, "noImplicitReturns": true,
    "lib": ["ES2020", "DOM"], "skipLibCheck": true
  },
  "include": ["src"]
}
```

Run: `cd ui && npm install && cd ..` (creates `package-lock.json`; commit it).

- [ ] **Step 4: HTML shells**

`ui/index.html`:
```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="dark">
  <title>ConvertFlow</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <header class="top">
    <h1>ConvertFlow</h1>
    <nav><a href="docs.html">API</a></nav>
  </header>
  <main>
    <section class="panel" aria-label="Upload">
      <div id="drop" class="drop" tabindex="0" role="button" aria-label="Choose files or drop them here">
        <p class="drop-title">Drop files here</p>
        <p class="muted">or click to choose video / audio files</p>
        <input id="file" type="file" multiple hidden>
      </div>
      <div class="controls">
        <label for="preset">Preset</label>
        <select id="preset"></select>
        <details>
          <summary>Advanced</summary>
          <div class="adv">
            <label for="speed">Speed (0 best – 10 fastest)</label>
            <input id="speed" type="number" min="0" max="10" placeholder="default">
            <label for="crf">Quality CRF (18 best – 45 smallest)</label>
            <input id="crf" type="number" min="18" max="45" placeholder="default">
            <label for="height">Max height</label>
            <select id="height"><option value="">original</option><option>2160</option><option>1440</option><option>1080</option><option>720</option><option>480</option><option>360</option><option>240</option></select>
            <label for="abr">Audio bitrate (kbit/s)</label>
            <select id="abr"><option value="">default</option><option>64</option><option>96</option><option>128</option><option>192</option><option>256</option><option>320</option></select>
          </div>
        </details>
      </div>
      <p id="notice" class="muted" hidden></p>
      <p id="error" class="error" role="alert" hidden></p>
    </section>
    <section class="panel" aria-label="Jobs">
      <table>
        <thead><tr><th>File</th><th>Preset</th><th>Status</th><th>Progress</th><th>Encoder</th><th></th></tr></thead>
        <tbody id="jobs"></tbody>
      </table>
      <p id="empty" class="muted">No jobs yet.</p>
    </section>
  </main>
  <script src="main.js"></script>
</body>
</html>
```

`ui/docs.html`:
```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta name="color-scheme" content="dark">
  <title>ConvertFlow API</title>
  <link rel="stylesheet" href="style.css">
</head>
<body>
  <header class="top"><h1>ConvertFlow API</h1><nav><a href="index.html">Back</a></nav></header>
  <main>
    <section class="panel">
      <p class="muted">Machine-readable spec: <a href="/api/openapi.json">/api/openapi.json</a></p>
      <div id="endpoints"></div>
      <h2>Example</h2>
      <pre>curl -F preset=audio-flac -F file=@song.wav http://HOST:8080/api/v1/jobs
curl http://HOST:8080/api/v1/jobs/&lt;id&gt;
curl -OJ http://HOST:8080/api/v1/jobs/&lt;id&gt;/output</pre>
      <p class="muted">Send the <code>preset</code> (or <code>options</code> JSON) field before the <code>file</code> field.</p>
    </section>
  </main>
  <script src="docs.js"></script>
</body>
</html>
```

- [ ] **Step 5: Styles (dark)** — `ui/src/style.css`

```css
:root {
  color-scheme: dark;
  --bg: #0e1116; --panel: #161b22; --panel2: #1d242d; --border: #2b3440;
  --text: #e6edf3; --muted: #8b98a8; --accent: #4aa3ff; --accent-dim: #1f4f80;
  --ok: #3fb950; --bad: #f85149; --warn: #d29922;
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--text); font: 15px/1.5 system-ui, "Segoe UI", Roboto, sans-serif; }
a { color: var(--accent); }
.top { display: flex; justify-content: space-between; align-items: center; padding: 14px 24px; border-bottom: 1px solid var(--border); background: var(--panel); }
.top h1 { margin: 0; font-size: 18px; letter-spacing: .02em; }
main { max-width: 1100px; margin: 24px auto; padding: 0 16px; display: grid; gap: 20px; }
.panel { background: var(--panel); border: 1px solid var(--border); border-radius: 10px; padding: 20px; overflow-x: auto; }
.drop { border: 2px dashed var(--border); border-radius: 10px; padding: 36px 16px; text-align: center; cursor: pointer; transition: border-color .15s, background .15s; }
.drop:hover, .drop:focus-visible, .drop.over { border-color: var(--accent); background: var(--panel2); outline: none; }
.drop-title { margin: 0; font-size: 18px; }
.muted { color: var(--muted); margin: .25rem 0; }
.error { color: var(--bad); }
.controls { display: grid; gap: 8px; margin-top: 16px; max-width: 420px; }
label { color: var(--muted); font-size: 13px; }
select, input[type=number] { background: var(--panel2); color: var(--text); border: 1px solid var(--border); border-radius: 6px; padding: 8px; }
select:focus-visible, input:focus-visible, button:focus-visible, summary:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
.adv { display: grid; gap: 8px; margin-top: 8px; }
summary { cursor: pointer; color: var(--muted); }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 10px 8px; border-bottom: 1px solid var(--border); vertical-align: middle; }
th { color: var(--muted); font-weight: 500; font-size: 13px; }
progress { width: 160px; height: 10px; accent-color: var(--accent); }
.st-done { color: var(--ok); } .st-failed { color: var(--bad); } .st-canceled { color: var(--warn); }
button, .btn { background: var(--panel2); color: var(--text); border: 1px solid var(--border); border-radius: 6px; padding: 6px 12px; cursor: pointer; text-decoration: none; font: inherit; }
button:hover, .btn:hover { border-color: var(--accent); }
pre, code { background: var(--panel2); border-radius: 6px; padding: 2px 6px; }
pre { padding: 12px; overflow-x: auto; }
.ep { border-bottom: 1px solid var(--border); padding: 8px 0; }
.method { display: inline-block; min-width: 64px; font-weight: 600; color: var(--accent); }
```

- [ ] **Step 6: Main script** — `ui/src/main.ts`

```ts
interface Job {
  id: string; status: string; filename: string; preset: string; progress: number;
  encoder_used: string; error?: string; output_url?: string;
}
interface Preset { id: string; label: string; kind: string; ext: string }

const $ = <T extends HTMLElement>(id: string) => document.getElementById(id) as T;
const rows = new Map<string, HTMLTableRowElement>();
const streams = new Map<string, EventSource>();
const TERMINAL = ["done", "failed", "canceled"];

function el<K extends keyof HTMLElementTagNameMap>(tag: K, text = "", cls = ""): HTMLElementTagNameMap[K] {
  const e = document.createElement(tag);
  if (text) e.textContent = text; // never innerHTML: filenames are user-controlled
  if (cls) e.className = cls;
  return e;
}

function showError(msg: string) {
  const e = $("error");
  e.textContent = msg;
  e.hidden = !msg;
}

async function problem(res: Response): Promise<string> {
  try { const p = await res.json(); return p.detail || p.title || res.statusText; } catch { return res.statusText; }
}

async function loadPresets() {
  const sel = $<HTMLSelectElement>("preset");
  const res = await fetch("/api/v1/presets");
  const presets: Preset[] = await res.json();
  sel.replaceChildren();
  for (const p of presets) {
    const o = el("option", p.label);
    o.value = p.id;
    sel.append(o);
  }
  const n = $("notice");
  if (presets.length === 0) { n.textContent = "No worker is running; uploads are disabled."; n.hidden = false; }
  else if (presets.some(p => p.id === "video-av1")) { n.textContent = "AV1 output may not play in older players; VP9 is the compatible alternative."; n.hidden = false; }
  else n.hidden = true;
}

function options(preset: string): string {
  const o: Record<string, unknown> = { preset };
  const num = (id: string) => { const v = $<HTMLInputElement>(id).value; return v === "" ? undefined : Number(v); };
  const speed = num("speed"), crf = num("crf"), height = num("height"), abr = num("abr");
  if (speed !== undefined) o.speed = speed;
  if (crf !== undefined) o.crf = crf;
  if (height !== undefined) o.max_height = height;
  if (abr !== undefined) o.audio_bitrate_k = abr;
  return JSON.stringify(o);
}

function render(job: Job) {
  let tr = rows.get(job.id);
  if (!tr) {
    tr = el("tr");
    rows.set(job.id, tr);
    $("jobs").prepend(tr);
  }
  const prog = el("progress");
  prog.max = 100;
  prog.value = job.progress;
  const act = el("td");
  if (job.output_url) {
    const a = el("a", "Download", "btn");
    a.href = job.output_url;
    act.append(a, " ");
  }
  const rm = el("button", TERMINAL.includes(job.status) ? "Remove" : "Cancel");
  rm.onclick = () => remove(job.id);
  act.append(rm);
  const status = el("td", job.status + (job.error ? ` – ${job.error}` : ""), `st-${job.status}`);
  tr.replaceChildren(el("td", job.filename), el("td", job.preset), status,
    (() => { const td = el("td"); td.append(prog); return td; })(), el("td", job.encoder_used), act);
  $("empty").hidden = rows.size > 0;
  if (!TERMINAL.includes(job.status)) subscribe(job.id);
}

function subscribe(id: string) {
  if (streams.has(id)) return;
  const es = new EventSource(`/api/v1/jobs/${id}/events`);
  streams.set(id, es);
  es.onmessage = (ev) => {
    const j: Job = JSON.parse(ev.data);
    render(j);
    if (TERMINAL.includes(j.status)) { es.close(); streams.delete(id); }
  };
  es.onerror = () => { es.close(); streams.delete(id); };
}

async function remove(id: string) {
  const res = await fetch(`/api/v1/jobs/${id}`, { method: "DELETE" });
  if (res.status === 204) { rows.get(id)?.remove(); rows.delete(id); $("empty").hidden = rows.size > 0; }
  else if (res.status !== 202) showError(await problem(res));
}

function upload(file: File) {
  showError("");
  const preset = $<HTMLSelectElement>("preset").value;
  if (!preset) { showError("No preset available."); return; }
  const fd = new FormData();
  fd.append("options", options(preset)); // options/preset must precede the file
  fd.append("file", file);
  const xhr = new XMLHttpRequest();
  const tmp = el("tr");
  const prog = el("progress");
  prog.max = 100;
  const td = el("td"); td.append(prog);
  tmp.append(el("td", file.name), el("td", preset), el("td", "uploading"), td, el("td"), el("td"));
  $("jobs").prepend(tmp);
  $("empty").hidden = true;
  xhr.upload.onprogress = (e) => { if (e.lengthComputable) prog.value = (e.loaded / e.total) * 100; };
  xhr.onload = () => {
    tmp.remove();
    if (xhr.status === 202) render(JSON.parse(xhr.responseText));
    else {
      let msg = `Upload failed (${xhr.status})`;
      try { const p = JSON.parse(xhr.responseText); msg = p.detail || p.title || msg; } catch { /* keep default */ }
      showError(`${file.name}: ${msg}`);
      $("empty").hidden = rows.size > 0;
    }
  };
  xhr.onerror = () => { tmp.remove(); showError(`${file.name}: network error`); };
  xhr.open("POST", "/api/v1/jobs");
  xhr.send(fd);
}

function wire() {
  const drop = $("drop"), input = $<HTMLInputElement>("file");
  drop.onclick = () => input.click();
  drop.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); input.click(); } };
  input.onchange = () => { Array.from(input.files ?? []).forEach(upload); input.value = ""; };
  ["dragenter", "dragover"].forEach(n => drop.addEventListener(n, (e) => { e.preventDefault(); drop.classList.add("over"); }));
  ["dragleave", "drop"].forEach(n => drop.addEventListener(n, (e) => { e.preventDefault(); drop.classList.remove("over"); }));
  drop.addEventListener("drop", (e) => Array.from((e as DragEvent).dataTransfer?.files ?? []).forEach(upload));
}

async function init() {
  wire();
  await loadPresets();
  setInterval(loadPresets, 30000);
  const res = await fetch("/api/v1/jobs");
  if (res.ok) (await res.json() as Job[]).reverse().forEach(render);
}
init().catch((e) => showError(String(e)));
```

`ui/src/docs.ts`:
```ts
interface Op { summary?: string }
async function main() {
  const res = await fetch("/api/openapi.json");
  const doc = await res.json() as { paths: Record<string, Record<string, Op>> };
  const root = document.getElementById("endpoints")!;
  for (const [path, ops] of Object.entries(doc.paths)) {
    for (const [method, op] of Object.entries(ops)) {
      const row = document.createElement("div");
      row.className = "ep";
      const m = document.createElement("span");
      m.className = "method";
      m.textContent = method.toUpperCase();
      const code = document.createElement("code");
      code.textContent = path;
      const s = document.createElement("span");
      s.className = "muted";
      s.textContent = " " + (op.summary ?? "");
      row.append(m, code, s);
      root.append(row);
    }
  }
}
main().catch(() => { /* docs are optional */ });
```

- [ ] **Step 7: Build and test**

Run: `make ui && go test ./internal/ui/ -v`
Expected: PASS (`TestIndexServed`, `TestNoInlineScriptsOrExternalReferences`). If the external-URL test fails on a minified bundle, find the string with `grep -o 'https\?://[^"]*' internal/ui/dist/*.js` and remove its source.

- [ ] **Step 8: Manual visual check**

Run: `scripts/test-db.sh` is not needed; start Postgres with `docker run --rm -d --name cf-dev -e POSTGRES_PASSWORD=dev -p 55433:5432 postgres:16-bookworm`, then
`CONVERTFLOW_DATABASE_URL='postgres://postgres:dev@127.0.0.1:55433/postgres?sslmode=disable' CONVERTFLOW_DATA_DIR=/tmp/cf-data CONVERTFLOW_FLAVOR=full go run ./cmd/convertflow-api &` and the same env with `go run ./cmd/convertflow-worker &`. Open http://127.0.0.1:8080, drop a small audio file with preset "Audio – FLAC", and confirm: dark theme, live progress, Download link works, Remove works. Stop both processes and `docker stop cf-dev`.

- [ ] **Step 9: Commit**

```bash
git add -A && git commit -m "feat(ui): dark drag-and-drop UI with live progress and API docs page" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Container images, compose and hardening test

**Files:**
- Create: `Dockerfile.api`, `Dockerfile.worker`, `compose.yaml`, `docker/ffmpeg.env`, `scripts/gen-secrets.sh`, `scripts/pin-digests.sh`, `scripts/smoke.sh`, `tests/container/hardening.sh`

**Interfaces:**
- Consumes: binaries from Tasks 6–7, `make ui` output (built inside the image)
- Produces: images `convertflow-api`, `convertflow-worker`; `compose.yaml` services `db`, `api`, `worker`; networks `frontend`, `backend` (internal).

- [ ] **Step 1: Write the failing hardening test** — `tests/container/hardening.sh` (then `chmod +x`)

```bash
#!/usr/bin/env bash
# Fails if compose.yaml or the built images regress on the hardening rules in spec §3.
# Requires: docker (or CONTAINER_RUNTIME), jq. Images must be built first (`docker compose build`).
set -euo pipefail
cd "$(dirname "$0")/../.."
RT=${CONTAINER_RUNTIME:-docker}
fail=0
check() { if ! eval "$2"; then echo "FAIL: $1"; fail=1; else echo "ok:   $1"; fi; }

CFG=$($RT compose config --format json)

check "every service is read_only" \
  "jq -e '.services | to_entries | all(.value.read_only == true)' <<<\"\$CFG\" >/dev/null"
check "every service drops ALL capabilities" \
  "jq -e '.services | to_entries | all(.value.cap_drop | index(\"ALL\"))' <<<\"\$CFG\" >/dev/null"
check "every service sets no-new-privileges" \
  "jq -e '.services | to_entries | all(.value.security_opt | index(\"no-new-privileges:true\"))' <<<\"\$CFG\" >/dev/null"
check "every service has pids_limit, mem_limit and cpus" \
  "jq -e '.services | to_entries | all(.value.pids_limit and .value.mem_limit and .value.cpus)' <<<\"\$CFG\" >/dev/null"
check "backend network is internal" \
  "jq -e '.networks.backend.internal == true' <<<\"\$CFG\" >/dev/null"
check "worker is only on the backend network" \
  "jq -e '.services.worker.networks | keys == [\"backend\"]' <<<\"\$CFG\" >/dev/null"
check "db is only on the backend network" \
  "jq -e '.services.db.networks | keys == [\"backend\"]' <<<\"\$CFG\" >/dev/null"
check "api port is published on loopback only by default" \
  "jq -e '.services.api.ports | all(.host_ip == \"127.0.0.1\")' <<<\"\$CFG\" >/dev/null"
check "compose images are pinned by digest" \
  "jq -e '.services | to_entries | all(.value.image | test(\"@sha256:\"))' <<<\"\$CFG\" >/dev/null"
check "Dockerfile base images are pinned by digest" \
  "! grep -hE '^FROM ' Dockerfile.api Dockerfile.worker | grep -vE '@sha256:|AS (ffmpeg|gobuild|ui|build)\$' | grep -q ."

for img in convertflow-api convertflow-worker; do
  user=$($RT image inspect "$img" --format '{{.Config.User}}' 2>/dev/null || true)
  check "$img runs as non-root ($user)" "[[ \"$user\" == 65532* ]]"
done

exit $fail
```

Run: `tests/container/hardening.sh` → FAIL (no compose.yaml yet)

- [ ] **Step 2: FFmpeg pin file and verification** — `docker/ffmpeg.env`

First fetch and verify the source (outside the air gap), then record the checksum:
```bash
V=7.1.1
curl -fLO https://ffmpeg.org/releases/ffmpeg-$V.tar.xz
curl -fLO https://ffmpeg.org/releases/ffmpeg-$V.tar.xz.asc
curl -fL https://ffmpeg.org/ffmpeg-devel.asc | gpg --import
gpg --verify ffmpeg-$V.tar.xz.asc ffmpeg-$V.tar.xz   # must report a good signature
sha256sum ffmpeg-$V.tar.xz
```
Write the verified values (replace the hash with the real `sha256sum` output):
```
FFMPEG_VERSION=7.1.1
FFMPEG_SHA256=<64-hex-sha256-from-the-command-above>
```
Do not continue until the signature check passed and the hash is recorded; the Dockerfile refuses to build without a matching hash. Delete the downloaded tarball afterwards.

- [ ] **Step 3: API Dockerfile** — `Dockerfile.api`

```dockerfile
# syntax=docker/dockerfile:1
FROM node:22-bookworm-slim AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
RUN mkdir -p /src/internal/ui/dist && npm run build

FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/convertflow-api ./cmd/convertflow-api \
 && mkdir -p /out/data/in /out/data/out

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/convertflow-api /convertflow-api
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV CONVERTFLOW_DATA_DIR=/data
HEALTHCHECK --interval=15s --timeout=5s --retries=3 CMD ["/convertflow-api", "healthcheck"]
ENTRYPOINT ["/convertflow-api"]
```

- [ ] **Step 4: Worker Dockerfile** — `Dockerfile.worker`

```dockerfile
# syntax=docker/dockerfile:1
FROM debian:bookworm-slim AS ffmpeg
ARG FFMPEG_VERSION
ARG FFMPEG_SHA256
ARG FFMPEG_FLAVOR=lgpl
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential pkg-config nasm yasm curl ca-certificates xz-utils \
      libsvtav1enc-dev libaom-dev libvpx-dev libopus-dev libvorbis-dev libmp3lame-dev \
      libx264-dev libx265-dev \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN test -n "$FFMPEG_VERSION" && test -n "$FFMPEG_SHA256" \
 && curl -fLo ffmpeg.tar.xz "https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz" \
 && echo "${FFMPEG_SHA256}  ffmpeg.tar.xz" | sha256sum -c - \
 && mkdir ffmpeg && tar -xf ffmpeg.tar.xz -C ffmpeg --strip-components=1
WORKDIR /src/ffmpeg
RUN set -eux; \
    flags="--prefix=/opt/ffmpeg --disable-debug --disable-doc --disable-ffplay --disable-network \
      --enable-libsvtav1 --enable-libaom --enable-libvpx --enable-libopus --enable-libvorbis --enable-libmp3lame"; \
    if [ "$FFMPEG_FLAVOR" = "full" ]; then \
      echo "NOTICE: building the GPL flavor (x264/x265). Redistribution requires offering source; H.264/H.265/AAC may require patent licenses. See docs/licensing.md."; \
      flags="$flags --enable-gpl --enable-libx264 --enable-libx265"; \
    fi; \
    ./configure $flags; \
    make -j"$(nproc)"; make install

FROM golang:1.23-bookworm AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/convertflow-worker ./cmd/convertflow-worker

FROM debian:bookworm-slim
ARG FFMPEG_FLAVOR=lgpl
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends libsvtav1enc1 libaom3 libvpx7 libopus0 libvorbisenc2 libmp3lame0; \
    if [ "$FFMPEG_FLAVOR" = "full" ]; then apt-get install -y --no-install-recommends libx264-164 libx265-199; fi; \
    rm -rf /var/lib/apt/lists/*; \
    groupadd -g 65532 convertflow; useradd -u 65532 -g 65532 -M -s /usr/sbin/nologin convertflow; \
    mkdir -p /data/in /data/out; chown -R 65532:65532 /data; \
    find / -xdev -perm /6000 -type f -exec chmod a-s {} + 2>/dev/null || true
COPY --from=ffmpeg /opt/ffmpeg/bin/ffmpeg /opt/ffmpeg/bin/ffprobe /usr/local/bin/
COPY --from=ffmpeg /src/ffmpeg/COPYING.* /usr/share/doc/ffmpeg/
COPY --from=gobuild /out/convertflow-worker /usr/local/bin/convertflow-worker
USER 65532:65532
ENV CONVERTFLOW_DATA_DIR=/data CONVERTFLOW_FLAVOR=${FFMPEG_FLAVOR}
ENTRYPOINT ["/usr/local/bin/convertflow-worker"]
```

Note: `ENV ... ${FFMPEG_FLAVOR}` expands the build ARG declared in the final stage, so the runtime flavor always matches the build flavor.

- [ ] **Step 5: Compose** — `compose.yaml`

```yaml
name: convertflow

x-hardening: &hardening
  read_only: true
  cap_drop: [ALL]
  security_opt: ["no-new-privileges:true"]
  restart: unless-stopped

services:
  db:
    <<: *hardening
    image: postgres:16-bookworm
    user: "999:999"
    environment:
      POSTGRES_USER: convertflow
      POSTGRES_DB: convertflow
      POSTGRES_PASSWORD_FILE: /run/secrets/db_password
    secrets: [db_password]
    volumes: ["pgdata:/var/lib/postgresql/data"]
    tmpfs: ["/tmp", "/run/postgresql"]
    networks: [backend]
    pids_limit: 200
    mem_limit: 1g
    cpus: 1.0
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U convertflow -d convertflow"]
      interval: 10s
      timeout: 5s
      retries: 5

  api:
    <<: *hardening
    image: convertflow-api:${CONVERTFLOW_VERSION:-dev}
    build: {context: ., dockerfile: Dockerfile.api}
    depends_on: {db: {condition: service_healthy}}
    environment:
      CONVERTFLOW_LISTEN: "0.0.0.0:8080"
      CONVERTFLOW_DATABASE_URL_FILE: /run/secrets/database_url
      CONVERTFLOW_RETENTION: ${CONVERTFLOW_RETENTION:-24h}
      CONVERTFLOW_MAX_UPLOAD_BYTES: ${CONVERTFLOW_MAX_UPLOAD_BYTES:-8589934592}
    secrets: [database_url]
    volumes: ["media:/data"]
    ports: ["127.0.0.1:8080:8080"]   # change the host IP deliberately to expose the service
    networks: [frontend, backend]
    pids_limit: 200
    mem_limit: 512m
    cpus: 1.0

  worker:
    <<: *hardening
    image: convertflow-worker:${CONVERTFLOW_VERSION:-dev}
    build:
      context: .
      dockerfile: Dockerfile.worker
      args:
        FFMPEG_FLAVOR: ${FFMPEG_FLAVOR:-lgpl}
        FFMPEG_VERSION: ${FFMPEG_VERSION:?set via docker/ffmpeg.env}
        FFMPEG_SHA256: ${FFMPEG_SHA256:?set via docker/ffmpeg.env}
    depends_on: {db: {condition: service_healthy}}
    environment:
      CONVERTFLOW_DATABASE_URL_FILE: /run/secrets/database_url
      CONVERTFLOW_FLAVOR: ${FFMPEG_FLAVOR:-lgpl}
    secrets: [database_url]
    volumes: ["media:/data"]
    tmpfs: ["/tmp:size=2g,mode=1777"]
    networks: [backend]            # no egress: backend is internal
    pids_limit: 512
    mem_limit: 4g
    cpus: 4.0

networks:
  frontend: {}
  backend: {internal: true}

volumes:
  pgdata: {}
  media: {}

secrets:
  db_password: {file: ./secrets/db_password}
  database_url: {file: ./secrets/database_url}
```

Compose reads build args from the shell, so run it as `set -a; . docker/ffmpeg.env; set +a; docker compose build` (the smoke script does this).

- [ ] **Step 6: Helper scripts** (all `chmod +x`)

`scripts/gen-secrets.sh`:
```bash
#!/usr/bin/env bash
# Creates ./secrets with a random database password and the matching connection URL.
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
mkdir -p secrets
[ -f secrets/db_password ] && { echo "secrets/ already exists; refusing to overwrite"; exit 0; }
pw=$(head -c 24 /dev/urandom | base64 | tr -d '/+=\n')
printf '%s' "$pw" > secrets/db_password
printf 'postgres://convertflow:%s@db:5432/convertflow?sslmode=disable' "$pw" > secrets/database_url
# the postgres container runs as uid 999 and the apps as 65532; world-readable inside the host dir is
# acceptable only because ./secrets is mode 700 on a host you control.
chmod 644 secrets/*
echo "secrets written to ./secrets"
```

`scripts/pin-digests.sh`:
```bash
#!/usr/bin/env bash
# Rewrites every base image reference to <ref>@sha256:<digest>. Run outside the air gap, review the diff, commit.
set -euo pipefail
cd "$(dirname "$0")/.."
RT=${CONTAINER_RUNTIME:-docker}
refs=(node:22-bookworm-slim golang:1.23-bookworm gcr.io/distroless/static-debian12:nonroot debian:bookworm-slim postgres:16-bookworm)
for ref in "${refs[@]}"; do
  digest=$($RT buildx imagetools inspect "$ref" --format '{{.Manifest.Digest}}')
  esc=$(printf '%s' "$ref" | sed 's/[.[\*^$/]/\\&/g')
  sed -i -E "s|${esc}(@sha256:[0-9a-f]+)?([[:space:]]|\$)|${ref}@${digest}\2|g" Dockerfile.api Dockerfile.worker compose.yaml
  echo "pinned $ref -> $digest"
done
```

`scripts/smoke.sh`:
```bash
#!/usr/bin/env bash
# End-to-end smoke test against the real containers: upload a generated WAV, wait for FLAC, check it.
set -euo pipefail
cd "$(dirname "$0")/.."
RT=${CONTAINER_RUNTIME:-docker}
set -a; . docker/ffmpeg.env; set +a
export FFMPEG_FLAVOR=${FFMPEG_FLAVOR:-lgpl}
scripts/gen-secrets.sh
trap '$RT compose down -v >/dev/null 2>&1 || true' EXIT
$RT compose up -d --build
for _ in $(seq 1 60); do curl -fs http://127.0.0.1:8080/healthz >/dev/null && break; sleep 2; done
tmp=$(mktemp -d)
ffmpeg -hide_banner -loglevel error -f lavfi -i sine=duration=2 -c:a pcm_s16le "$tmp/in.wav"
for _ in $(seq 1 30); do
  curl -fs http://127.0.0.1:8080/api/v1/presets | grep -q audio-flac && break; sleep 2
done
id=$(curl -fs -F preset=audio-flac -F "file=@$tmp/in.wav" http://127.0.0.1:8080/api/v1/jobs | sed -E 's/.*"id":"([^"]+)".*/\1/')
for _ in $(seq 1 60); do
  st=$(curl -fs "http://127.0.0.1:8080/api/v1/jobs/$id" | sed -E 's/.*"status":"([^"]+)".*/\1/')
  [ "$st" = done ] && break
  [ "$st" = failed ] && { echo "job failed"; exit 1; }
  sleep 1
done
curl -fs -o "$tmp/out.flac" "http://127.0.0.1:8080/api/v1/jobs/$id/output"
ffprobe -v error -show_entries format=format_name -of csv=p=0 "$tmp/out.flac" | grep -q flac
echo "smoke test passed"
```

- [ ] **Step 7: Pin digests, build, run the hardening test**

```bash
scripts/pin-digests.sh
scripts/gen-secrets.sh
set -a; . docker/ffmpeg.env; set +a
docker compose build
tests/container/hardening.sh
```
Expected: every line `ok:`, exit 0. Fix any `FAIL:` by editing `compose.yaml`/Dockerfiles (not the test).

- [ ] **Step 8: Smoke test the real stack**

Run: `scripts/smoke.sh`
Expected: `smoke test passed`. If the worker exits with a read-only filesystem error, add the failing path as a `tmpfs` entry on the worker service; do not remove `read_only`.

- [ ] **Step 9: Commit**

```bash
git add -A && git commit -m "feat: hardened container images, compose stack and hardening test" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 12: CI and release supply chain

**Files:**
- Create: `.github/workflows/ci.yml`, `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `scripts/test-db.sh` conventions (`TEST_DATABASE_URL`), `make ui`, Dockerfiles, `tests/container/hardening.sh`

- [ ] **Step 1: CI workflow** — `.github/workflows/ci.yml`

```yaml
name: ci
on:
  push: {branches: [main]}
  pull_request:

permissions:
  contents: read

jobs:
  test:
    runs-on: ubuntu-24.04
    services:
      postgres:
        image: postgres:16-bookworm
        env: {POSTGRES_PASSWORD: test}
        ports: ["5432:5432"]
        options: >-
          --health-cmd "pg_isready -U postgres" --health-interval 5s --health-timeout 5s --health-retries 10
    env:
      TEST_DATABASE_URL: postgres://postgres:test@localhost:5432/postgres?sslmode=disable
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: "1.23"}
      - uses: actions/setup-node@v4
        with: {node-version: "22", cache: npm, cache-dependency-path: ui/package-lock.json}
      - run: sudo apt-get update && sudo apt-get install -y ffmpeg
      - run: make ui
      - run: go vet ./...
      - run: go test ./... -race -count=1

  security:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: "1.23"}
      - run: go run golang.org/x/vuln/cmd/govulncheck@latest ./...
      - run: go run github.com/securego/gosec/v2/cmd/gosec@latest -exclude-dir=internal/e2e ./...
      - name: License policy (permissive only)
        run: go run github.com/google/go-licenses@latest check ./... --allowed_licenses=MIT,BSD-2-Clause,BSD-3-Clause,Apache-2.0,ISC

  images:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - name: Load FFmpeg pin
        run: grep -v '^#' docker/ffmpeg.env >> "$GITHUB_ENV"
      - run: ./scripts/gen-secrets.sh
      - run: docker compose build
      - run: tests/container/hardening.sh
      - name: Scan images
        run: |
          for img in convertflow-api convertflow-worker; do
            docker run --rm -v /var/run/docker.sock:/var/run/docker.sock aquasec/trivy:latest image \
              --exit-code 1 --severity HIGH,CRITICAL --ignore-unfixed "$img:dev"
          done
```

- [ ] **Step 2: Release workflow** — `.github/workflows/release.yml`

```yaml
name: release
on:
  push:
    tags: ["v*"]

permissions:
  contents: write
  packages: write
  id-token: write

jobs:
  release:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: {go-version: "1.23"}
      - uses: sigstore/cosign-installer@v3
      - uses: anchore/sbom-action/download-syft@v0
      - name: Third-party licenses
        run: go run github.com/google/go-licenses@latest save ./... --save_path=THIRD-PARTY-LICENSES --force
      - name: Build images
        run: |
          grep -v '^#' docker/ffmpeg.env >> "$GITHUB_ENV"
          set -a; . docker/ffmpeg.env; set +a
          ./scripts/gen-secrets.sh
          export CONVERTFLOW_VERSION=${GITHUB_REF_NAME}
          docker compose build
      - name: Push and sign
        run: |
          echo "${{ secrets.GITHUB_TOKEN }}" | docker login ghcr.io -u "${{ github.actor }}" --password-stdin
          for n in api worker; do
            ref=ghcr.io/${GITHUB_REPOSITORY,,}/convertflow-$n:${GITHUB_REF_NAME}
            docker tag convertflow-$n:${GITHUB_REF_NAME} "$ref"
            docker push "$ref"
            cosign sign --yes "$ref"
          done
      - name: SBOMs and air-gap bundle
        run: |
          mkdir dist
          for n in api worker; do
            syft convertflow-$n:${GITHUB_REF_NAME} -o cyclonedx-json=dist/convertflow-$n.cdx.json
          done
          docker save convertflow-api:${GITHUB_REF_NAME} convertflow-worker:${GITHUB_REF_NAME} \
            $(grep -oE 'postgres:16-bookworm@sha256:[0-9a-f]+' compose.yaml | head -1) | gzip > dist/convertflow-images.tar.gz
          cp compose.yaml dist/; cp -r scripts docs THIRD-PARTY-LICENSES LICENSE dist/
          (cd dist && sha256sum convertflow-images.tar.gz *.cdx.json > SHA256SUMS)
          tar -czf convertflow-${GITHUB_REF_NAME}-airgap.tar.gz -C dist .
      - name: Publish release
        env: {GH_TOKEN: "${{ github.token }}"}
        run: gh release create "${GITHUB_REF_NAME}" convertflow-${GITHUB_REF_NAME}-airgap.tar.gz --generate-notes
```

Note: pin third-party actions by commit SHA in a follow-up hardening pass once the workflows are green; tags are used here for readability.

- [ ] **Step 3: Validate and commit**

Run: `python3 -c "import yaml,sys;[yaml.safe_load(open(f)) for f in ['.github/workflows/ci.yml','.github/workflows/release.yml']]"` → no error

```bash
git add -A && git commit -m "ci: tests, security scans, image scan, signed release with SBOM and air-gap bundle" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```
Then `git push` and check the Actions tab with `gh run watch`; fix failures before proceeding.

---

### Task 13: End-to-end and hostile-input tests

**Files:**
- Create: `internal/e2e/e2e_test.go`

**Interfaces:**
- Consumes: `api.NewHandler`, `worker.Worker`, `testutil`, `dbtest`, helpers from earlier tasks. All components wired in-process.

- [ ] **Step 1: Write the tests** — `internal/e2e/e2e_test.go`

```go
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/api"
	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/config"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/testutil"
	"github.com/ShifuRain/convertflow/internal/worker"
)

type stack struct {
	base string
	dir  string
}

func start(t *testing.T, lim ffmpeg.Limits) stack {
	t.Helper()
	testutil.RequireTools(t)
	pool := dbtest.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "in"), 0o700)
	_ = os.MkdirAll(filepath.Join(dir, "out"), 0o700)
	store := jobs.NewStore(pool)
	w := &worker.Worker{
		ID: "e2e", Store: store, Audit: audit.New(io.Discard), FFmpegBin: "ffmpeg", FFprobeBin: "ffprobe", Flavor: "full",
		OutDir: filepath.Join(dir, "out"), ScratchDir: t.TempDir(), Limits: lim,
		Poll: 50 * time.Millisecond, HeartbeatEvery: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := w.RegisterCapabilities(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run(ctx) }()
	h := api.NewHandler(api.Deps{Store: store, Audit: audit.New(io.Discard), EventInterval: 50 * time.Millisecond,
		Cfg: config.Config{DataDir: dir, MaxUploadBytes: 50 << 20}})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return stack{srv.URL, dir}
}

func (s stack) upload(t *testing.T, preset, name string, content []byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("preset", preset)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = mw.Close()
	resp, err := http.Post(s.base+"/api/v1/jobs", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func (s stack) wait(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(s.base + "/api/v1/jobs/" + id)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		if st, _ := v["status"].(string); st == "done" || st == "failed" || st == "canceled" {
			return v
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("job did not finish in time")
	return nil
}

var limits = ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 8192 * 4320, MaxStreams: 16}

func TestRoundTripAudio(t *testing.T) {
	s := start(t, limits)
	clip, _ := os.ReadFile(testutil.MakeClip(t, t.TempDir(), "a.wav", "audio"))
	code, v := s.upload(t, "audio-flac", "tone.wav", clip)
	if code != 202 {
		t.Fatalf("upload: %d %v", code, v)
	}
	done := s.wait(t, v["id"].(string))
	if done["status"] != "done" {
		t.Fatalf("job: %v", done)
	}
	resp, err := http.Get(s.base + done["output_url"].(string))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("download: %v %v", resp, err)
	}
	out := filepath.Join(t.TempDir(), "o.flac")
	f, _ := os.Create(out)
	_, _ = io.Copy(f, resp.Body)
	f.Close()
	resp.Body.Close()
	b, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=format_name,duration", "-of", "csv=p=0", out).Output()
	if err != nil || !strings.Contains(string(b), "flac") {
		t.Fatalf("ffprobe: %q %v", b, err)
	}
	dur, _ := strconv.ParseFloat(strings.Split(strings.TrimSpace(string(b)), ",")[1], 64)
	if dur < 0.8 || dur > 1.3 {
		t.Fatalf("unexpected duration %v", dur)
	}
}

func TestCoverArtMP3StyleInputToAudioPreset(t *testing.T) {
	s := start(t, limits)
	dir := t.TempDir()
	cover := filepath.Join(dir, "cover.png")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "color=c=red:s=64x64:d=1", "-frames:v", "1", cover).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	mp3 := filepath.Join(dir, "a.mp3")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=duration=1",
		"-i", cover, "-map", "0:a", "-map", "1:v", "-c:a", "libmp3lame", "-c:v", "png", "-disposition:v", "attached_pic", mp3).CombinedOutput(); err != nil {
		t.Skipf("cannot build cover-art mp3 here (needs libmp3lame): %v %s", err, out)
	}
	b, _ := os.ReadFile(mp3)
	_, v := s.upload(t, "audio-flac", "cover.mp3", b)
	if done := s.wait(t, v["id"].(string)); done["status"] != "done" {
		t.Fatalf("cover-art file must convert: %v", done)
	}
}

func TestHostileInputsFailCleanly(t *testing.T) {
	s := start(t, limits)
	cases := map[string][]byte{
		"text.mp4":   []byte("just some text, not media"),
		"concat.mp4": []byte("ffconcat version 1.0\nfile '/etc/passwd'\nfile 'http://127.0.0.1:1/x'\n"),
		"m3u8.mp4":   []byte("#EXTM3U\n#EXTINF:1,\n/etc/passwd\n#EXT-X-ENDLIST\n"),
		"nulls.mp4":  bytes.Repeat([]byte{0}, 4096),
	}
	for name, content := range cases {
		code, v := s.upload(t, "audio-wav", name, content)
		if code != 202 {
			t.Errorf("%s: upload %d %v", name, code, v)
			continue
		}
		done := s.wait(t, v["id"].(string))
		if done["status"] != "failed" {
			t.Errorf("%s: want failed, got %v", name, done)
			continue
		}
		msg, _ := done["error"].(string)
		if msg == "" || strings.Contains(msg, s.dir) || strings.Contains(msg, "/etc/passwd") {
			t.Errorf("%s: unsafe or empty error %q", name, msg)
		}
	}
}

func TestLimitsEnforcedEndToEnd(t *testing.T) {
	s := start(t, ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 100, MaxStreams: 16})
	b, _ := os.ReadFile(testutil.MakeClip(t, t.TempDir(), "v.mkv", "video"))
	_, v := s.upload(t, "video-vp9", "big.mkv", b)
	if v["id"] == nil {
		t.Skip("video-vp9 not available with this local ffmpeg build")
	}
	done := s.wait(t, v["id"].(string))
	if done["status"] != "failed" || !strings.Contains(done["error"].(string), "dimensions") {
		t.Fatalf("want dimension failure, got %v", done)
	}
}

func TestDeleteRemovesFiles(t *testing.T) {
	s := start(t, limits)
	clip, _ := os.ReadFile(testutil.MakeClip(t, t.TempDir(), "a.wav", "audio"))
	_, v := s.upload(t, "audio-wav", "a.wav", clip)
	id := v["id"].(string)
	s.wait(t, id)
	req, _ := http.NewRequest("DELETE", s.base+"/api/v1/jobs/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("delete: %v %v", resp, err)
	}
	ents, _ := os.ReadDir(filepath.Join(s.dir, "out"))
	if len(ents) != 0 {
		t.Fatalf("output not removed: %v", ents)
	}
}
```

- [ ] **Step 2: Run**

Run: `scripts/test-db.sh ./internal/e2e/ -v -count=1`
Expected: PASS. A failure in `TestHostileInputsFailCleanly` for `concat.mp4`/`m3u8.mp4` means a hostile playlist was followed or probed in a leaky way: tighten `allowedFormats`/`Probe` (Task 5), never weaken the test.

- [ ] **Step 3: Full suite and commit**

Run: `scripts/test-db.sh -race -count=1` → PASS

```bash
git add -A && git commit -m "test: end-to-end round trip and hostile-input coverage" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
```

---

### Task 14: Documentation deliverables

**Files:**
- Create: `docs/operations.md`, `docs/grundschutz-mapping.md`, `docs/risk-acceptance-template.md`, `docs/licensing.md`
- Modify: `README.md`

**Interfaces:**
- Consumes: final behavior from all earlier tasks (env vars from Task 1, compose from Task 11).

- [ ] **Step 1: README.md** — replace with:

````markdown
# ConvertFlow

Self-hosted, air-gap-friendly web app and REST API that transcodes video and audio with FFmpeg. Dark UI, drag-and-drop, no authentication (see `docs/risk-acceptance-template.md`).

## Quick start (air-gapped)

On a connected machine, download the release bundle `convertflow-<version>-airgap.tar.gz`, verify `SHA256SUMS`, and carry it to the target host.

```bash
tar -xzf convertflow-<version>-airgap.tar.gz && cd convertflow-*
docker load < convertflow-images.tar.gz     # or: podman load
./scripts/gen-secrets.sh
CONVERTFLOW_VERSION=<version> docker compose up -d
```

Open http://127.0.0.1:8080. The UI is on that page; the API reference is on `/docs.html` and `/api/openapi.json`.

```bash
curl -F preset=audio-flac -F file=@song.wav http://127.0.0.1:8080/api/v1/jobs
curl http://127.0.0.1:8080/api/v1/jobs/<id>
curl -OJ http://127.0.0.1:8080/api/v1/jobs/<id>/output
```

Send the `preset` (or JSON `options`) field **before** the `file` field.

## Build from source

`set -a; . docker/ffmpeg.env; set +a; docker compose build` (royalty-free FFmpeg) or `FFMPEG_FLAVOR=full` for x264/x265/AAC (read `docs/licensing.md` first). Tests: `scripts/test-db.sh`.

## Documentation

- `docs/operations.md` – configuration, TLS, upgrades, backup, monitoring, hardware acceleration
- `docs/grundschutz-mapping.md` – IT-Grundschutz Bausteine mapping
- `docs/risk-acceptance-template.md` – risk acceptance for missing authentication
- `docs/licensing.md` – licenses, FFmpeg flavors, patent notice

License: Apache-2.0.
````

- [ ] **Step 2: docs/operations.md** — write these sections with this content:

  1. **Configuration** – a table of every `CONVERTFLOW_*` variable from `internal/config/config.go` with default and meaning: `LISTEN` (127.0.0.1:8080; the compose file sets 0.0.0.0 inside the container and publishes on 127.0.0.1), `DATABASE_URL` / `DATABASE_URL_FILE`, `DATA_DIR` (/data), `FLAVOR` (lgpl|full), `TLS_CERT_FILE` / `TLS_KEY_FILE` (set together), `VAAPI_DEVICE`, `MAX_UPLOAD_BYTES` (8 GiB), `MAX_PIXELS` (35,389,440), `MAX_DURATION_SEC` (21,600), `MAX_STREAMS` (16), `MAX_ATTEMPTS` (3), `RETENTION` (24h), `STALE_AFTER` (60s), `WORKER_POLL` (2s).
  2. **Exposing the service** – default publishes on loopback; to serve the network change `127.0.0.1:8080:8080` in `compose.yaml` to the intended interface, restrict access with the host firewall, and place a TLS-terminating reverse proxy in front (or set the two TLS variables and mount the certificate read-only). State plainly that there is no authentication: anyone who can reach the port can submit and delete jobs.
  3. **Scaling** – `docker compose up -d --scale worker=N`; each worker needs the `media` volume and the backend network; sizing guidance: SVT-AV1 at default speed uses all cores of one job, so N ≈ cores / 4.
  4. **Upgrades** – load new images, `docker compose up -d`; migrations run automatically and are idempotent; running jobs of stopped workers are requeued after `STALE_AFTER` (up to `MAX_ATTEMPTS`).
  5. **Backup** – only the Postgres database matters (`docker compose exec db pg_dump -U convertflow convertflow > backup.sql`); media files are ephemeral by design and are deleted after `RETENTION`.
  6. **Deletion and storage** – files are removed with a plain unlink after retention; for stronger guarantees place the `media` volume on an encrypted filesystem (LUKS) and destroy the key to sanitize.
  7. **Monitoring** – `/healthz` (liveness + DB), `/metrics` (Prometheus: `convertflow_jobs{status=…}`, `convertflow_workers_live`), audit events as JSON lines on API stdout (`"audit":true`, events `job_created`, `job_deleted`, `job_cancel_requested`) and worker stdout (`job_started`, `job_finished`, `job_failed`, `job_canceled`), janitor events (`job_expired`, `orphan_removed`, `jobs_recovered`). Forward container logs to the central log server (OPS.1.1.5). The client IP is the TCP peer address; behind a reverse proxy it will be the proxy's address.
  8. **Hardware acceleration** – the default image does not include NVENC/QSV/VAAPI. To use them, build a custom FFmpeg with the relevant `--enable-*` flags, pass the device (`devices: ["/dev/dri:/dev/dri"]` or the NVIDIA runtime) to the worker via a compose override file, and set `CONVERTFLOW_VAAPI_DEVICE` if needed. Workers verify each hardware encoder with a trial encode at start and advertise it only if it works; presets fall back to software automatically.
  9. **Known limits** – a worker that lacks an encoder the API accepted (mixed fleets) fails the job rather than rerouting; keep workers homogeneous.

- [ ] **Step 3: docs/grundschutz-mapping.md** — create the document in this form:

```markdown
# IT-Grundschutz mapping

ConvertFlow is software, not an ISMS: it cannot be "Grundschutz-compliant" or certified by itself. This document shows how it supports the Bausteine of the IT-Grundschutz-Kompendium that apply to it, and what the operating organization still has to do (Strukturanalyse, Schutzbedarfsfeststellung, Modellierung, audit).

Status values: **Erfüllt** (implemented in the product), **Teilweise** (product supports, operator completes), **Operator** (organizational), **Nicht umgesetzt** (accepted risk).

## Bausteine

| Baustein | Product measure | Where | Status |
|---|---|---|---|
| CON.10 / APP.3.1 | Input validation, option allow-list, CSP without inline script, security headers, upload limits, no shell | `internal/jobs/options.go`, `internal/ffmpeg/args.go`, `internal/api/middleware.go` | Erfüllt |
| APP.3.2 | TLS 1.2+ optional, reverse-proxy ready, minimal surface | `cmd/convertflow-api/main.go`, `docs/operations.md` | Teilweise |
| CON.8 | SBOM, vulnerability and license scanning, signed images, tests in CI | `.github/workflows/` | Erfüllt |
| OPS.1.1.5 | Structured audit log, metrics | `internal/audit/` | Teilweise |
| CON.6 | Retention janitor, explicit delete, orphan sweep | `internal/janitor/` | Teilweise |
| CON.3 | Only the database needs backup; documented | `docs/operations.md` | Operator |
| SYS.1.6 | Non-root, read-only rootfs, caps dropped, no-new-privileges, limits, internal network, pinned digests | `compose.yaml`, `tests/container/hardening.sh` | Erfüllt |
| OPS.1.1.4 | Untrusted media is probed and processed only in the isolated worker | `internal/worker/` | Teilweise |
| OPS.1.1.3 | Documented upgrade path; image rebuild on dependency updates | `docs/operations.md` | Operator |
| APP.4.3 | PostgreSQL on an internal network, credentials via secrets | `compose.yaml` | Teilweise |
| ORP.4 | No authentication by design | `docs/risk-acceptance-template.md` | Nicht umgesetzt |

## Requirement-level table

(Generated in the next step.)
```

Then replace the "Requirement-level table" placeholder section as follows: download the current IT-Grundschutz-Kompendium (outside the air gap) from the BSI page linked in the spec, and for each Baustein in the table above add one subsection listing every MUSS and SOLLTE requirement exactly as titled in the Kompendium (ID, title), with a status and a one-line product/operator note. Do not invent or paraphrase requirement IDs; copy them from the Kompendium edition you downloaded and state that edition in the document header. Requirements about the operator's organization stay marked **Operator**.

- [ ] **Step 4: docs/risk-acceptance-template.md** — write a document with these parts, filled with the product's facts and blanks only for organization-specific data (marked `[ORGANISATION: …]` for the operator to complete, never left unexplained):
  1. **Subject** – ConvertFlow version, host, network segment `[ORGANISATION: segment]`.
  2. **Deviation** – no identity and access management (ORP.4, relevant requirements of APP.3.1/CON.10): anyone with network access to the service can upload, download, list and delete any job.
  3. **Threat and impact analysis** – exposed capabilities (submit media, read others' results within retention, cancel/delete jobs, consume CPU), what is *not* exposed (no shell, no arbitrary FFmpeg options, no filesystem access outside the data volume), impact on confidentiality (other users' files visible within 24 h retention), integrity (job deletion), availability (CPU exhaustion by queueing).
  4. **Compensating controls** – air-gapped network, loopback-only default publish, host firewall rules `[ORGANISATION: allowed sources]`, hardened containers, upload/duration/pixel limits, retention, audit log with client IPs, internal-only worker/database network.
  5. **Residual risk assessment** and **decision** block: risk owner, ISB, date, review date (suggested: yearly or on any network exposure change), signature lines.
  6. **Triggers for re-assessment** – exposing beyond the isolated segment, processing media with higher protection requirements, adding users with different clearance.

- [ ] **Step 5: docs/licensing.md** — write with this content:
  1. Project license Apache-2.0; Go dependencies permissive only, checked in CI; `THIRD-PARTY-LICENSES` ships with each release.
  2. FFmpeg is executed as a separate process (no linking into ConvertFlow code).
  3. **lgpl flavor (default):** FFmpeg configured without `--enable-gpl`/`--enable-nonfree`; encoders SVT-AV1, libaom, libvpx, libopus, libvorbis (BSD-style), LAME (LGPL), FLAC/PCM (native); the image carries FFmpeg's `COPYING.*` files; LGPL obligations when redistributing: provide the FFmpeg source version used (`docker/ffmpeg.env`) and the build flags (printed in the Dockerfile).
  4. **full flavor (opt-in):** adds x264/x265 (GPL) so the FFmpeg binary is GPL-licensed; permitted for internal use; if the image is ever given to a third party, the corresponding source must be offered; the native AAC encoder is LGPL code but H.264/H.265/AAC are covered by patent pools (e.g. Via LA, Access Advance) — licensing those is the operator's responsibility and may apply to internal use. State that patent expiry varies by jurisdiction and that this is not legal advice; recommend the operator's legal department review before enabling `full`.
  5. Why MP3 is in the default flavor (key patents expired) and why the default video path is AV1/VP9.
  6. User content: ConvertFlow does not check rights to the converted material; the operator is responsible.

- [ ] **Step 6: Verify and commit**

Run: `grep -rn "TBD\|TODO\|Generated in the next step" docs README.md` → no matches (the mapping placeholder must be gone).

```bash
git add -A && git commit -m "docs: README, operations guide, Grundschutz mapping, risk template, licensing" -m "Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>"
git push
```

---

## Self-Review

**Spec coverage**
- §1 scope, no auth, Grundschutz position → Tasks 11 (compensating controls), 14 (risk template, mapping)
- §2 architecture (Go, two images, Postgres queue, embedded UI) → Tasks 2, 6, 7, 10, 11
- §3 deployment/hardening, SBOM, signing → Tasks 11, 12
- §4 FFmpeg flavors, licensing → Tasks 3 (policy), 11 (build), 14 (docs)
- §5 capability detection, fallback chains, 422 on unavailable → Tasks 3, 5, 7
- §6 API incl. OpenAPI, healthz, metrics → Tasks 7, 8
- §7 lifecycle (streaming upload, probe, allow-list, progress, janitor, heartbeat/requeue, sanitized errors) → Tasks 4–9
- §8 UI → Task 10 (deviation: plain TypeScript with no framework, which keeps the dependency tree and SBOM smaller; the spec only asked for "a small bundled framework")
- §9 mapping, §10 testing (unit, integration, hostile input, container, CI security), §11 docs → Tasks 13, 11, 12, 14
- Input is deleted immediately after processing (Task 6), stricter than the spec's retention rule; output still follows retention.

**Placeholder scan:** the only deliberate execution-time inputs are the FFmpeg SHA-256 (Task 11 Step 2, with the exact verification commands), image digests (written by `scripts/pin-digests.sh`) and the Kompendium requirement IDs (Task 14 Step 3, copied from the BSI document rather than invented).

**Type consistency:** `Heartbeat` returns `(bool, error)` everywhere; `Worker.Watch` is exported and used by the test and `process`; `ffmpeg.Limits` is built from `config.Config` in the worker main; `jobView`/`toView` are defined once (Task 7) and reused (Task 8); `liveWindow` is shared.

**Review Focus:** all five items map to named tests (Tasks 4–8, 13).
