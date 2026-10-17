package janitor_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/janitor"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type env struct {
	pool  *pgxpool.Pool
	store *jobs.Store
	dir   string
	j     *janitor.Janitor
	log   *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	e := &env{pool: pool, store: jobs.NewStore(pool), dir: t.TempDir(), log: &bytes.Buffer{}}
	for _, d := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(e.dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.j = &janitor.Janitor{Store: e.store, Audit: audit.New(e.log), DataDir: e.dir,
		Retention: 24 * time.Hour, StaleAfter: time.Minute, MaxAttempts: 3, Interval: time.Hour,
		OrphanGrace: 6 * time.Hour, JobTimeout: 4 * time.Hour}
	return e
}

func (e *env) file(t *testing.T, sub, name string, age time.Duration) string {
	t.Helper()
	p := filepath.Join(e.dir, sub, name)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

// job creates a job in the given state with its input file recorded.
func (e *env) job(t *testing.T, state string) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if err := e.store.Create(ctx, &jobs.Job{ID: id, Filename: "a", InputPath: filepath.Join(e.dir, "in", id), Options: jobs.Options{Preset: "audio-wav"}}); err != nil {
		t.Fatal(err)
	}
	switch state {
	case "queued":
	case "probing":
		mustClaim(t, e.store)
	case "running":
		mustClaim(t, e.store)
		if err := e.store.SetRunning(ctx, id, "x"); err != nil {
			t.Fatal(err)
		}
	case "failed":
		mustClaim(t, e.store)
		if err := e.store.Fail(ctx, id, "boom"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("bad state")
	}
	return id
}

func mustClaim(t *testing.T, s *jobs.Store) {
	t.Helper()
	if j, err := s.Claim(context.Background(), "w"); err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
}

func (e *env) done(t *testing.T, outName func(string) string) string {
	t.Helper()
	ctx := context.Background()
	id := e.job(t, "running")
	if err := e.store.Complete(ctx, id, filepath.Join(e.dir, "out", outName(id))); err != nil {
		t.Fatal(err)
	}
	return id
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestSweepRemovesExpiredJobsAndOrphans(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := uuid.NewString()
	out := filepath.Join(e.dir, "out", id+".wav")
	_ = os.WriteFile(out, []byte("x"), 0o600)
	_ = e.store.Create(ctx, &jobs.Job{ID: id, Filename: "a", InputPath: filepath.Join(e.dir, "in", id), Options: jobs.Options{Preset: "audio-wav"}})
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET status='running'`)
	_ = e.store.Complete(ctx, id, out)
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET finished_at = now() - interval '2 days'`)

	oldOrphan := e.file(t, "in", uuid.NewString(), 48*time.Hour)
	freshOrphan := e.file(t, "in", uuid.NewString(), 0)
	other := e.file(t, "in", "README", 48*time.Hour)

	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if exists(out) {
		t.Error("expired output should be removed")
	}
	if ok, _ := e.store.Exists(ctx, id); ok {
		t.Error("expired job row should be removed")
	}
	if exists(oldOrphan) {
		t.Error("old orphan should be removed")
	}
	if !exists(freshOrphan) {
		t.Error("fresh orphan must be kept (upload may be in progress)")
	}
	if !exists(other) {
		t.Error("files that are not job ids must never be touched")
	}
}

func TestSweepRequeuesStaleJobs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.job(t, "probing")
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET heartbeat_at = now() - interval '10 minutes'`)
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.Get(ctx, id)
	if got.Status != jobs.StatusQueued {
		t.Fatalf("want queued, got %s", got.Status)
	}
}

func TestSweepStaleCancelRequestedBecomesCanceled(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	id := e.job(t, "running")
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET cancel_requested = true, heartbeat_at = now() - interval '10 minutes'`)
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := e.store.Get(ctx, id)
	if got.Status != jobs.StatusCanceled {
		t.Fatalf("want canceled, got %s", got.Status)
	}
}

func TestOutputsNamedWithAttemptAreSwept(t *testing.T) {
	e := newEnv(t)
	old := e.file(t, "out", uuid.NewString()+".2.wav", 48*time.Hour)
	fresh := e.file(t, "out", uuid.NewString()+".1.mp4", time.Minute)
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists(old) {
		t.Error("old attempt-named orphan output must be removed")
	}
	if !exists(fresh) {
		t.Error("fresh orphan output (worker may be writing) must be kept")
	}
}

func TestSupersededOutputsOfLiveJobs(t *testing.T) {
	e := newEnv(t)
	// done job: recorded output is attempt 2; attempt 1 leftovers are superseded.
	id := e.done(t, func(id string) string { return id + ".2.wav" })
	rec := e.file(t, "out", id+".2.wav", 48*time.Hour)
	stale := e.file(t, "out", id+".1.wav", 48*time.Hour)
	freshStale := e.file(t, "out", id+".3.wav", time.Minute)
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(rec) {
		t.Error("recorded output of a live job must be kept")
	}
	if exists(stale) {
		t.Error("superseded old attempt output must be removed")
	}
	if !exists(freshStale) {
		t.Error("recent unrecorded output may belong to a writing worker; keep within grace")
	}
}

func TestActiveJobOutputProtectedUntilTimeoutPlusMargin(t *testing.T) {
	e := newEnv(t)
	id := e.job(t, "running")
	// older than grace (6h) but within JobTimeout (4h) is impossible with defaults; tighten grace.
	e.j.OrphanGrace = time.Hour
	e.j.JobTimeout = 4 * time.Hour
	mid := e.file(t, "out", id+".1.wav", 3*time.Hour)   // > grace, < timeout
	dead := e.file(t, "out", id+".1.mkv", 48*time.Hour) // far beyond timeout+margin
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(mid) {
		t.Error("output of a running job within JobTimeout must be kept")
	}
	if exists(dead) {
		t.Error("output of a running job older than JobTimeout+margin must be removed")
	}
}

func TestInputRules(t *testing.T) {
	e := newEnv(t)
	age := 48 * time.Hour
	queued := e.job(t, "queued")
	// each job claims the oldest queued job, so create states in order
	qIn := e.file(t, "in", queued, age)
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(qIn) {
		t.Fatal("input of a queued job must be kept")
	}
	_ = e.store.MarkCanceled(context.Background(), queued)
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists(qIn) {
		t.Error("input of a terminal job must be removed (lease-loss leftovers)")
	}

	e2 := newEnv(t)
	run := e2.job(t, "running")
	rIn := e2.file(t, "in", run, age)
	partial := e2.file(t, "in", uuid.NewString(), age)
	if err := e2.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(rIn) {
		t.Error("input of a running job must be kept")
	}
	if exists(partial) {
		t.Error("partial upload without a row must be removed after the grace period")
	}

	e3 := newEnv(t)
	f := e3.job(t, "failed")
	fIn := e3.file(t, "in", f, time.Minute)
	if err := e3.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(fIn) {
		t.Error("recent file kept within grace even for terminal job")
	}
}

func TestWeirdEntriesAreIgnored(t *testing.T) {
	e := newEnv(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "precious")
	_ = os.WriteFile(target, []byte("keep"), 0o600)
	oldT := time.Now().Add(-72 * time.Hour)

	// symlink with a canonical job-id name pointing outside the data dir
	link := filepath.Join(e.dir, "in", uuid.NewString())
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// directory with a job-id name, containing an old file
	d := filepath.Join(e.dir, "in", uuid.NewString())
	_ = os.Mkdir(d, 0o700)
	inner := filepath.Join(d, "f")
	_ = os.WriteFile(inner, []byte("x"), 0o600)
	_ = os.Chtimes(d, oldT, oldT)
	_ = os.Chtimes(inner, oldT, oldT)
	// symlinked directory in out/
	dl := filepath.Join(e.dir, "out", uuid.NewString()+".1.wav")
	_ = os.Symlink(outside, dl)
	upper := e.file(t, "in", strings.ToUpper(uuid.NewString()), 72*time.Hour)
	odd := e.file(t, "out", "not-a-uuid.1.wav", 72*time.Hour)
	hidden := e.file(t, "out", ".hidden", 72*time.Hour)
	// dangling symlink
	dang := filepath.Join(e.dir, "in", uuid.NewString())
	_ = os.Symlink(filepath.Join(outside, "nope"), dang)

	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "keep" {
		t.Error("symlink target outside the data dir must be untouched")
	}
	for _, p := range []string{link, d, inner, dl, upper, odd, hidden, dang} {
		if !exists(p) {
			t.Errorf("%s must be ignored", p)
		}
	}
}

func TestMissingAndUnreadableDirsDoNotFail(t *testing.T) {
	e := newEnv(t)
	_ = os.RemoveAll(filepath.Join(e.dir, "out"))
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatalf("missing dir must not fail the sweep: %v", err)
	}
	if os.Geteuid() != 0 {
		_ = os.MkdirAll(filepath.Join(e.dir, "out"), 0o700)
		_ = os.Chmod(filepath.Join(e.dir, "out"), 0)
		defer os.Chmod(filepath.Join(e.dir, "out"), 0o700)
		if err := e.j.Sweep(context.Background()); err != nil {
			t.Fatalf("unreadable dir must not fail the sweep: %v", err)
		}
	}
}

func TestRetentionRefusesPathsOutsideDataDir(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "precious")
	_ = os.WriteFile(outside, []byte("keep"), 0o600)
	id := uuid.NewString()
	_ = e.store.Create(ctx, &jobs.Job{ID: id, Filename: "a", InputPath: outside, Options: jobs.Options{Preset: "audio-wav"}})
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET status='running'`)
	_ = e.store.Complete(ctx, id, outside)
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET finished_at = now() - interval '2 days'`)
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.store.Exists(ctx, id); ok {
		t.Error("row should be deleted")
	}
	if !exists(outside) {
		t.Error("file outside DataDir must never be removed")
	}
}

func TestAuditLogsJobIDsOnly(t *testing.T) {
	e := newEnv(t)
	id := uuid.NewString()
	e.file(t, "in", id, 48*time.Hour)
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := e.log.String()
	if !strings.Contains(s, `"event":"orphan_removed"`) || !strings.Contains(s, id) {
		t.Errorf("want orphan_removed audit with job id, got %q", s)
	}
	if strings.Contains(s, e.dir) {
		t.Error("audit must not leak filesystem paths")
	}
	_ = io.Discard
}

func TestRunStopsOnContextCancel(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.j.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}
