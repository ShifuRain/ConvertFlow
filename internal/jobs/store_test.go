package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/ShifuRain/convertflow/internal/presets"
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
	if _, err := s.Get(ctx, uuid.NewString()); !errors.Is(err, jobs.ErrNotFound) {
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
			j, err := s.Claim(ctx, fmt.Sprintf("w%d", i), presets.IDs(presets.All()))
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
	j, err := s.Claim(context.Background(), "w", presets.IDs(presets.All()))
	if j != nil || err != nil {
		t.Fatalf("want nil,nil got %v,%v", j, err)
	}
}

func TestLifecycleAndCancel(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	j := newJob()
	_ = s.Create(ctx, j)
	c, _ := s.Claim(ctx, "w1", presets.IDs(presets.All()))
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
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All())) // a
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All())) // b
	_ = s.SetRunning(ctx, a.ID, "pcm_s16le")
	if err := s.Complete(ctx, a.ID, "/data/out/x.wav"); err != nil {
		t.Fatal(err)
	}
	if err := s.Fail(ctx, b.ID, "boom"); err != nil {
		t.Fatal(err)
	}
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
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All())) // a: attempts=1
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All())) // b: attempts=1
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
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All())) // claims `idle` (older)
	if _, err := s.Delete(ctx, idle.ID); !errors.Is(err, jobs.ErrBusy) {
		t.Fatalf("want ErrBusy, got %v", err)
	}
	d, err := s.Delete(ctx, busy.ID) // still queued
	if err != nil || d.ID != busy.ID {
		t.Fatalf("got %v %v", d, err)
	}
	if _, err := s.Delete(ctx, uuid.NewString()); !errors.Is(err, jobs.ErrNotFound) {
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
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All()))
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All()))
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

func TestZombieWritesAfterRequeue(t *testing.T) {
	pool := dbtest.New(t)
	s := jobs.NewStore(pool)
	ctx := context.Background()
	j := newJob()
	_ = s.Create(ctx, j)
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All()))
	_, _ = pool.Exec(ctx, `UPDATE jobs SET heartbeat_at = now() - interval '10 minutes'`)
	if r, _, err := s.RequeueStale(ctx, time.Minute, 3); err != nil || r != 1 {
		t.Fatalf("requeue %d %v", r, err)
	}
	checks := map[string]error{
		"SetProbe":       s.SetProbe(ctx, j.ID, []byte(`{}`)),
		"SetRunning":     s.SetRunning(ctx, j.ID, "x"),
		"UpdateProgress": s.UpdateProgress(ctx, j.ID, 5),
		"Complete":       s.Complete(ctx, j.ID, "/o"),
		"Fail":           s.Fail(ctx, j.ID, "x"),
	}
	_, hbErr := s.Heartbeat(ctx, j.ID)
	checks["Heartbeat"] = hbErr
	for name, err := range checks {
		if !errors.Is(err, jobs.ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", name, err)
		}
	}
}

func TestZombieLeavesJobQueued(t *testing.T) {
	pool := dbtest.New(t)
	s := jobs.NewStore(pool)
	ctx := context.Background()
	j := newJob()
	_ = s.Create(ctx, j)
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All()))
	_, _ = pool.Exec(ctx, `UPDATE jobs SET heartbeat_at = now() - interval '10 minutes'`)
	_, _, _ = s.RequeueStale(ctx, time.Minute, 3)
	_ = s.SetRunning(ctx, j.ID, "x")
	_ = s.Complete(ctx, j.ID, "/o")
	_ = s.Fail(ctx, j.ID, "x")
	got, _ := s.Get(ctx, j.ID)
	if got.Status != jobs.StatusQueued || got.OutputPath != "" {
		t.Fatalf("job mutated by zombie: %+v", got)
	}
}

func TestMarkCanceledOnDoneIsNotFound(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	j := newJob()
	_ = s.Create(ctx, j)
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All()))
	_ = s.SetRunning(ctx, j.ID, "x")
	_ = s.Complete(ctx, j.ID, "/o")
	if err := s.MarkCanceled(ctx, j.ID); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	got, _ := s.Get(ctx, j.ID)
	if got.Status != jobs.StatusDone {
		t.Fatalf("status %s", got.Status)
	}
}

func TestHeartbeatMissingRow(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	if _, err := s.Heartbeat(context.Background(), uuid.NewString()); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRequeueStaleCancelRequestedGoesCanceled(t *testing.T) {
	pool := dbtest.New(t)
	s := jobs.NewStore(pool)
	ctx := context.Background()
	j := newJob()
	_ = s.Create(ctx, j)
	_, _ = s.Claim(ctx, "w", presets.IDs(presets.All()))
	_ = s.RequestCancel(ctx, j.ID)
	_, _ = pool.Exec(ctx, `UPDATE jobs SET heartbeat_at = now() - interval '10 minutes'`)
	requeued, failed, err := s.RequeueStale(ctx, time.Minute, 3)
	if err != nil || requeued != 0 || failed != 0 {
		t.Fatalf("got %d,%d,%v", requeued, failed, err)
	}
	got, _ := s.Get(ctx, j.ID)
	if got.Status != jobs.StatusCanceled || got.FinishedAt == nil {
		t.Fatalf("bad %+v", got)
	}
}

func TestCountActive(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	if n, err := s.CountActive(ctx); err != nil || n != 0 {
		t.Fatalf("empty: %d %v", n, err)
	}
	var ids []string
	for i := 0; i < 4; i++ {
		j := newJob()
		if err := s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, j.ID)
	}
	if n, _ := s.CountActive(ctx); n != 4 {
		t.Fatalf("queued jobs count: %d", n)
	}
	claimed, err := s.Claim(ctx, "w1", presets.IDs(presets.All())) // probing still counts
	if err != nil || claimed == nil {
		t.Fatal(claimed, err)
	}
	if n, _ := s.CountActive(ctx); n != 4 {
		t.Fatalf("probing must count: %d", n)
	}
	for _, id := range ids {
		if id != claimed.ID {
			if err := s.RequestCancel(ctx, id); err != nil { // queued -> canceled immediately
				t.Fatal(err)
			}
			break
		}
	}
	if err := s.Fail(ctx, claimed.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountActive(ctx); n != 2 {
		t.Fatalf("terminal jobs must not count: %d", n)
	}
}

func TestClaimFiltersByPreset(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	mk := func(preset string) *jobs.Job {
		j := newJob()
		j.Options.Preset = preset
		if err := s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
		return j
	}
	a1, b, a2 := mk("video-vp9"), mk("audio-wav"), mk("video-vp9")
	if j, err := s.Claim(ctx, "w", nil); j != nil || err != nil {
		t.Fatalf("empty list must claim nothing: %v %v", j, err)
	}
	if j, err := s.Claim(ctx, "w", []string{"audio-flac"}); j != nil || err != nil {
		t.Fatalf("unrelated preset claimed: %v %v", j, err)
	}
	got, err := s.Claim(ctx, "wb", []string{"audio-wav"})
	if err != nil || got == nil || got.ID != b.ID {
		t.Fatalf("worker offering audio-wav: %v %v", got, err)
	}
	for _, want := range []string{a1.ID, a2.ID} { // oldest first among eligible
		got, err := s.Claim(ctx, "wa", []string{"video-vp9", "audio-wav"})
		if err != nil || got == nil || got.ID != want {
			t.Fatalf("want %s, got %v %v", want, got, err)
		}
	}
	if j, _ := s.Claim(ctx, "wa", presets.IDs(presets.All())); j != nil {
		t.Fatalf("nothing left, got %v", j)
	}
}
