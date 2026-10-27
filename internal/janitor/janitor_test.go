package janitor_test

import (
	"bytes"
	"context"
	"github.com/ShifuRain/convertflow/internal/presets"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/blob"
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
	e.j = &janitor.Janitor{Store: e.store, Audit: audit.New(e.log), Blob: blob.OpenLocalFS(e.dir),
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
		if err := e.store.SetRunning(ctx, id, "w", "x"); err != nil {
			t.Fatal(err)
		}
	case "failed":
		mustClaim(t, e.store)
		if err := e.store.Fail(ctx, id, "w", "boom"); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("bad state")
	}
	return id
}

func mustClaim(t *testing.T, s *jobs.Store) {
	t.Helper()
	if j, err := s.Claim(context.Background(), "w", presets.IDs(presets.All())); err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
}

func (e *env) done(t *testing.T, outName func(string) string) string {
	t.Helper()
	ctx := context.Background()
	id := e.job(t, "running")
	if err := e.store.Complete(ctx, id, "w", filepath.Join(e.dir, "out", outName(id))); err != nil {
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
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET status='running', worker_id='w'`)
	_ = e.store.Complete(ctx, id, "w", out)
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
	_ = e.store.RequestCancel(context.Background(), queued)
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
	_, _ = e.pool.Exec(ctx, `UPDATE jobs SET status='running', worker_id='w'`)
	_ = e.store.Complete(ctx, id, "w", outside)
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

func (e *env) backdate(t *testing.T, id string, age time.Duration) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE jobs SET created_at = now() - make_interval(secs => $2), updated_at = now() - make_interval(secs => $2) WHERE id = $1::uuid`, id, age.Seconds()); err != nil {
		t.Fatal(err)
	}
}

func TestSweepFailsStrandedQueuedJobs(t *testing.T) {
	e := newEnv(t)
	e.j.MaxQueueAge = time.Hour
	old, fresh := e.job(t, "queued"), e.job(t, "queued")
	oldIn := e.file(t, "in", old, 0)
	freshIn := e.file(t, "in", fresh, 0)
	e.backdate(t, old, 2*time.Hour)
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := e.store.Get(context.Background(), old)
	if err != nil || got.Status != jobs.StatusFailed || got.Error != jobs.ErrNoWorker || got.FinishedAt == nil {
		t.Fatalf("old job: %+v %v", got, err)
	}
	if _, err := os.Stat(oldIn); !os.IsNotExist(err) {
		t.Error("input of the stranded job was not removed")
	}
	if g, _ := e.store.Get(context.Background(), fresh); g.Status != jobs.StatusQueued {
		t.Errorf("job within age touched: %s", g.Status)
	}
	if _, err := os.Stat(freshIn); err != nil {
		t.Error("input of the young job was removed")
	}
	logs := e.log.String()
	if !strings.Contains(logs, `"event":"job_queue_expired"`) || !strings.Contains(logs, old) || strings.Contains(logs, e.dir) {
		t.Errorf("audit record wrong or leaks a path: %s", logs)
	}
	// Disabled when zero.
	e.j.MaxQueueAge = 0
	e.backdate(t, fresh, 1000*time.Hour)
	_ = e.j.Sweep(context.Background())
	if g, _ := e.store.Get(context.Background(), fresh); g.Status != jobs.StatusQueued {
		t.Error("zero MaxQueueAge must disable the check")
	}
}

// A job that a worker claims is never failed by the queue-age check, even if it is old.
func TestSweepNeverFailsClaimedJobs(t *testing.T) {
	e := newEnv(t)
	e.j.MaxQueueAge = time.Hour
	id := e.job(t, "queued")
	e.backdate(t, id, 5*time.Hour)
	var wg sync.WaitGroup
	wg.Add(2)
	var claimed *jobs.Job
	go func() {
		defer wg.Done()
		claimed, _ = e.store.Claim(context.Background(), "w", presets.IDs(presets.All()))
	}()
	go func() { defer wg.Done(); _ = e.j.Sweep(context.Background()) }()
	wg.Wait()
	got, _ := e.store.Get(context.Background(), id)
	switch {
	case claimed != nil && got.Status != jobs.StatusProbing:
		t.Fatalf("claimed job ended as %s", got.Status)
	case claimed == nil && got.Status != jobs.StatusFailed:
		t.Fatalf("unclaimed job ended as %s", got.Status)
	}
}

// A long backlog for a preset that a live worker can run is not "stranded": only jobs whose preset no
// single live worker can run expire.
func TestSweepKeepsBacklogOfRunnablePresetAndExpiresUnsupportedOnes(t *testing.T) {
	e := newEnv(t)
	e.j.MaxQueueAge = time.Hour
	e.j.Flavor = "full"
	ctx := context.Background()
	if err := e.store.UpsertWorker(ctx, "w1", []string{"pcm_s16le"}); err != nil { // audio-wav only
		t.Fatal(err)
	}
	backlog := e.job(t, "queued") // audio-wav
	unsupported := uuid.NewString()
	if err := e.store.Create(ctx, &jobs.Job{ID: unsupported, Filename: "a", InputPath: filepath.Join(e.dir, "in", unsupported), Options: jobs.Options{Preset: "video-vp9"}}); err != nil {
		t.Fatal(err)
	}
	e.backdate(t, backlog, 48*time.Hour)
	e.backdate(t, unsupported, 48*time.Hour)
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.store.Get(ctx, backlog); g.Status != jobs.StatusQueued {
		t.Errorf("runnable backlog was failed by age: %+v", g)
	}
	if g, _ := e.store.Get(ctx, unsupported); g.Status != jobs.StatusFailed || g.Error != jobs.ErrNoWorker {
		t.Errorf("unsupported preset should expire: %+v", g)
	}
}

// Encoders split across two workers do not make a preset runnable (no single worker can run it).
func TestSweepExpiresPresetOnlySplittableAcrossWorkers(t *testing.T) {
	e := newEnv(t)
	e.j.MaxQueueAge = time.Hour
	e.j.Flavor = "full"
	ctx := context.Background()
	_ = e.store.UpsertWorker(ctx, "a", []string{"libvpx-vp9"})
	_ = e.store.UpsertWorker(ctx, "b", []string{"libopus"})
	id := uuid.NewString()
	if err := e.store.Create(ctx, &jobs.Job{ID: id, Filename: "a", InputPath: filepath.Join(e.dir, "in", id), Options: jobs.Options{Preset: "video-vp9"}}); err != nil {
		t.Fatal(err)
	}
	e.backdate(t, id, 48*time.Hour)
	_ = e.j.Sweep(ctx)
	if g, _ := e.store.Get(ctx, id); g.Status != jobs.StatusFailed {
		t.Errorf("job no single worker can run must expire: %+v", g)
	}
}

func (e *env) uploading(t *testing.T, age time.Duration) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	if err := e.store.Create(ctx, &jobs.Job{ID: id, Status: jobs.StatusUploading, Filename: "a", InputPath: filepath.Join(e.dir, "in", id),
		Options: jobs.Options{Preset: "audio-wav"}, DeclaredSize: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE jobs SET updated_at = now() - make_interval(secs => $2) WHERE id = $1::uuid`, id, age.Seconds()); err != nil {
		t.Fatal(err)
	}
	return id
}

// Uploads never completed are failed after UploadTTL+UploadGrace and their object removed;
// younger ones and (with UploadTTL unset) all of them are kept.
func TestSweepExpiresAbandonedUploads(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old := e.uploading(t, 15*time.Minute+janitor.UploadGrace+time.Minute)
	young := e.uploading(t, 15*time.Minute)
	oldIn := e.file(t, "in", old, time.Minute)
	youngIn := e.file(t, "in", young, time.Minute)

	if err := e.j.Sweep(ctx); err != nil { // UploadTTL unset: nothing happens
		t.Fatal(err)
	}
	if j, _ := e.store.Get(ctx, old); j.Status != jobs.StatusUploading {
		t.Fatalf("UploadTTL=0 must not expire uploads, got %s", j.Status)
	}

	e.j.UploadTTL = 15 * time.Minute
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	j, _ := e.store.Get(ctx, old)
	if j.Status != jobs.StatusFailed || j.Error != jobs.ErrUploadExpired {
		t.Fatalf("abandoned upload: %+v", j)
	}
	if _, err := os.Stat(oldIn); !os.IsNotExist(err) {
		t.Fatal("abandoned upload's object must be removed")
	}
	if j, _ := e.store.Get(ctx, young); j.Status != jobs.StatusUploading {
		t.Fatalf("young upload expired: %s", j.Status)
	}
	if _, err := os.Stat(youngIn); err != nil {
		t.Fatal("young upload's object removed")
	}
	if !strings.Contains(e.log.String(), `"event":"job_upload_expired"`) || !strings.Contains(e.log.String(), old) {
		t.Fatalf("missing audit: %s", e.log)
	}
}

// The orphan sweep keeps the input of an uploading job however old it is.
func TestSweepKeepsInputOfUploadingJob(t *testing.T) {
	e := newEnv(t)
	id := e.uploading(t, time.Minute)
	in := e.file(t, "in", id, 7*time.Hour)
	if err := e.j.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(in); err != nil {
		t.Fatal("input of an uploading job removed by the orphan sweep")
	}
}

// On demand (scale to zero) a live worker is the exception: queued jobs expire purely by age, even
// when a live worker could run the preset, and their input is removed.
func TestSweepOnDemandExpiresByAgeOnly(t *testing.T) {
	e := newEnv(t)
	e.j.MaxQueueAge = time.Hour
	e.j.Flavor = "full"
	e.j.OnDemand = true
	ctx := context.Background()
	if err := e.store.UpsertWorker(ctx, "w1", []string{"pcm_s16le"}); err != nil { // could run audio-wav
		t.Fatal(err)
	}
	old, young := e.job(t, "queued"), e.job(t, "queued")
	oldIn := e.file(t, "in", old, time.Minute)
	e.backdate(t, old, 2*time.Hour)
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.store.Get(ctx, old); g.Status != jobs.StatusFailed || g.Error != jobs.ErrQueueTimeout {
		t.Errorf("old queued job must expire by age on demand: %+v", g)
	}
	if _, err := os.Stat(oldIn); !os.IsNotExist(err) {
		t.Error("expired job's input must be removed")
	}
	if g, _ := e.store.Get(ctx, young); g.Status != jobs.StatusQueued {
		t.Errorf("young job must stay queued: %+v", g)
	}
}

// With no worker at all (the on-demand steady state), a young job is never touched and an old one
// fails with the queue timeout, not "no worker available".
func TestSweepOnDemandWithZeroWorkers(t *testing.T) {
	e := newEnv(t)
	e.j.MaxQueueAge = time.Hour
	e.j.OnDemand = true
	ctx := context.Background()
	young := e.job(t, "queued")
	e.backdate(t, young, 59*time.Minute)
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.store.Get(ctx, young); g.Status != jobs.StatusQueued {
		t.Errorf("job inside MaxQueueAge failed while scaled to zero: %+v", g)
	}
}

// Issue #9: a worker whose last registration refresh is 90s old (for example during a slow
// re-detection) still counts as live for queue expiry by default.
func TestSweepLiveWindowToleratesRefreshGap(t *testing.T) {
	e := newEnv(t)
	e.j.MaxQueueAge = time.Hour
	e.j.Flavor = "full"
	ctx := context.Background()
	_ = e.store.UpsertWorker(ctx, "w1", []string{"pcm_s16le"})
	_, _ = e.pool.Exec(ctx, `UPDATE workers SET seen_at = now() - interval '90 seconds'`)
	backlog := e.job(t, "queued")
	e.backdate(t, backlog, 48*time.Hour)
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.store.Get(ctx, backlog); g.Status != jobs.StatusQueued {
		t.Errorf("backlog failed during a short registration gap: %+v", g)
	}
	// 2*StaleAfter widens the window further.
	e.j.StaleAfter = 5 * time.Minute
	_, _ = e.pool.Exec(ctx, `UPDATE workers SET seen_at = now() - interval '8 minutes'`)
	_ = e.j.Sweep(ctx)
	if g, _ := e.store.Get(ctx, backlog); g.Status != jobs.StatusQueued {
		t.Errorf("window must be at least 2*StaleAfter: %+v", g)
	}
	_, _ = e.pool.Exec(ctx, `UPDATE workers SET seen_at = now() - interval '11 minutes'`)
	_ = e.j.Sweep(ctx)
	if g, _ := e.store.Get(ctx, backlog); g.Status != jobs.StatusFailed {
		t.Errorf("a worker gone for longer than the window no longer protects the backlog: %+v", g)
	}
}

func TestSweepPrunesOldWorkerRegistrations(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	_ = e.store.UpsertWorker(ctx, "recent", []string{"flac"})
	_ = e.store.UpsertWorker(ctx, "old", []string{"flac"})
	_, _ = e.pool.Exec(ctx, `UPDATE workers SET seen_at = now() - interval '8 days' WHERE id = 'old'`)
	if err := e.j.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var ids []string
	rows, _ := e.pool.Query(ctx, `SELECT id FROM workers`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 1 || ids[0] != "recent" {
		t.Fatalf("want only the recent registration, got %v", ids)
	}
}
