package jobs_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
	"github.com/google/uuid"
)

func key(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func ownedJob(owner []byte) *jobs.Job {
	j := newJob()
	j.Owner = owner
	return j
}

func TestScopedAccess(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	a, b := key("a"), key("b")
	ja, jb, jn := ownedJob(a), ownedJob(b), ownedJob(nil)
	for _, j := range []*jobs.Job{ja, jb, jn} {
		if err := s.Create(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.GetFor(ctx, ja.ID, jobs.OwnedBy(a)); err != nil || !bytes.Equal(got.Owner, a) {
		t.Fatalf("owner cannot read own job: %v", err)
	}
	for name, sc := range map[string]jobs.Scope{"other owner": jobs.OwnedBy(b), "nil owner": jobs.OwnedBy(nil), "zero scope": {}} {
		if _, err := s.GetFor(ctx, ja.ID, sc); !errors.Is(err, jobs.ErrNotFound) {
			t.Errorf("%s: get %v", name, err)
		}
		if _, err := s.DeleteFor(ctx, ja.ID, sc); !errors.Is(err, jobs.ErrNotFound) {
			t.Errorf("%s: delete %v", name, err)
		}
		if err := s.RequestCancelFor(ctx, ja.ID, sc); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.Get(ctx, ja.ID); got.Status != jobs.StatusQueued || got.CancelRequested {
		t.Fatalf("a foreign cancel touched the job: %+v", got)
	}
	if l, _ := s.ListFor(ctx, jobs.OwnedBy(a), 10); len(l) != 1 || l[0].ID != ja.ID {
		t.Fatalf("list a: %v", l)
	}
	if l, _ := s.ListFor(ctx, jobs.OwnedBy(nil), 10); len(l) != 0 {
		t.Fatalf("nil owner lists %d jobs", len(l))
	}
	if l, _ := s.ListFor(ctx, jobs.AllJobs(), 10); len(l) != 3 {
		t.Fatalf("all: %d", len(l))
	}
	if _, err := s.GetFor(ctx, jn.ID, jobs.AllJobs()); err != nil {
		t.Fatal(err)
	}
	// busy only inside the scope
	if _, err := s.Claim(ctx, "w", presets.IDs(presets.All())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteFor(ctx, ja.ID, jobs.OwnedBy(b)); !errors.Is(err, jobs.ErrNotFound) {
		t.Fatalf("foreign delete of an active job: %v", err)
	}
	if _, err := s.DeleteFor(ctx, ja.ID, jobs.OwnedBy(a)); !errors.Is(err, jobs.ErrBusy) {
		t.Fatalf("own active job: %v", err)
	}
	if err := s.RequestCancelFor(ctx, ja.ID, jobs.OwnedBy(a)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Get(ctx, ja.ID); !got.CancelRequested {
		t.Fatal("own cancel not recorded")
	}
	if got, err := s.DeleteFor(ctx, jb.ID, jobs.OwnedBy(b)); err != nil || got.ID != jb.ID {
		t.Fatalf("own delete: %v", err)
	}
}

func TestQuotaBoundaries(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	a := key("a")
	create := func(l jobs.Limits, size int64, ip []byte) error {
		j := ownedJob(a)
		j.InputSize, j.ClientKey = size, ip
		return s.CreateWithQuota(ctx, j, l)
	}
	kind := func(err error) string {
		var qe *jobs.QuotaError
		if errors.As(err, &qe) {
			return qe.Kind
		}
		if err != nil {
			return err.Error()
		}
		return ""
	}
	l := jobs.Limits{ActiveJobs: 2}
	if kind(create(l, 10, nil)) != "" || kind(create(l, 10, nil)) != "" {
		t.Fatal("below the limit refused")
	}
	if k := kind(create(l, 10, nil)); k != jobs.QuotaActiveJobs {
		t.Fatalf("third active job: %q", k)
	}
	// another owner is not affected; finishing a job frees the slot
	other := ownedJob(key("b"))
	if err := s.CreateWithQuota(ctx, other, l); err != nil {
		t.Fatal(err)
	}
	all, _ := s.ListFor(ctx, jobs.OwnedBy(a), 10)
	if _, err := s.DeleteFor(ctx, all[0].ID, jobs.OwnedBy(a)); err != nil {
		t.Fatal(err)
	}
	if kind(create(l, 10, nil)) != "" {
		t.Fatal("freed slot not usable")
	}

	// queued bytes: 2 active jobs of 10 bytes each
	if k := kind(create(jobs.Limits{QueuedBytes: 30}, 10, nil)); k != "" {
		t.Fatalf("exactly at the byte limit: %q", k)
	}
	if k := kind(create(jobs.Limits{QueuedBytes: 30}, 1, nil)); k != jobs.QuotaQueuedBytes {
		t.Fatalf("over the byte limit: %q", k)
	}
	if k := kind(create(jobs.Limits{QueuedBytes: 30}, 31, nil)); k != jobs.QuotaInputTooLarge {
		t.Fatalf("single input over the byte limit: %q", k)
	}
	if u, _ := s.QuotaUsage(ctx, a, nil); u.ActiveJobs != 3 || u.QueuedBytes != 30 {
		t.Fatalf("usage %+v", u)
	}
	// declared size counts while uploading
	up := ownedJob(a)
	up.Status, up.DeclaredSize, up.InputSize = jobs.StatusUploading, 5, 5
	if err := s.Create(ctx, up); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.QuotaUsage(ctx, a, nil); u.ActiveJobs != 4 || u.QueuedBytes != 35 {
		t.Fatalf("usage with uploading %+v", u)
	}

	// per IP
	ip := key("ip")
	if kind(create(jobs.Limits{ActiveJobsPerIP: 1}, 1, ip)) != "" {
		t.Fatal("first job of an IP refused")
	}
	j := ownedJob(key("someone else"))
	j.ClientKey = ip
	if k := kind(s.CreateWithQuota(ctx, j, jobs.Limits{ActiveJobsPerIP: 1})); k != jobs.QuotaActiveJobsPerIP {
		t.Fatalf("second job of an IP under another owner: %q", k)
	}
}

func TestJobsPerHourSurvivesDelete(t *testing.T) {
	pool := dbtest.New(t)
	s := jobs.NewStore(pool)
	ctx := context.Background()
	a := key("a")
	l := jobs.Limits{JobsPerHour: 2}
	for i := 0; i < 2; i++ {
		j := ownedJob(a)
		if err := s.CreateWithQuota(ctx, j, l); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteFor(ctx, j.ID, jobs.OwnedBy(a)); err != nil {
			t.Fatal(err)
		}
	}
	var qe *jobs.QuotaError
	if err := s.CreateWithQuota(ctx, ownedJob(a), l); !errors.As(err, &qe) || qe.Kind != jobs.QuotaJobsPerHour {
		t.Fatalf("delete-and-recreate reset the hourly count: %v", err)
	}
	if qe.RetryAfter < 59*time.Minute || qe.RetryAfter > time.Hour {
		t.Fatalf("retry after %s", qe.RetryAfter)
	}
	// age the events: the oldest leaves the window first
	if _, err := pool.Exec(ctx, `UPDATE rate_events SET at = now() - interval '70 minutes' WHERE at = (SELECT min(at) FROM rate_events)`); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateWithQuota(ctx, ownedJob(a), l); err != nil {
		t.Fatalf("after the window: %v", err)
	}
	if n, err := s.PruneRateEvents(ctx); err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
}

// Parallel creates for one owner must never exceed the limits: the check and the insert run under
// an advisory lock per owner and IP key.
func TestQuotaRaceParallelCreates(t *testing.T) {
	s := jobs.NewStore(dbtest.New(t))
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		l     jobs.Limits
		owner bool
		want  int
	}{
		{"active jobs", jobs.Limits{ActiveJobs: 3}, true, 3},
		{"jobs per hour", jobs.Limits{JobsPerHour: 4}, true, 4},
		{"queued bytes", jobs.Limits{QueuedBytes: 50}, true, 5},
		{"per ip", jobs.Limits{ActiveJobsPerIP: 2}, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner, ip := key(uuid.NewString()), key(uuid.NewString())
			var wg sync.WaitGroup
			var mu sync.Mutex
			ok, refused := 0, 0
			for i := 0; i < 24; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					j := newJob()
					j.InputSize, j.ClientKey = 10, ip
					if tc.owner {
						j.Owner = owner
					} else {
						j.Owner = key(uuid.NewString()) // a fresh owner per request: only the IP limit binds
					}
					err := s.CreateWithQuota(ctx, j, tc.l)
					var qe *jobs.QuotaError
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						ok++
					case errors.As(err, &qe):
						refused++
					default:
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			if ok != tc.want || refused != 24-tc.want {
				t.Fatalf("created %d refused %d, want exactly %d created", ok, refused, tc.want)
			}
		})
	}
}

func TestSessions(t *testing.T) {
	pool := dbtest.New(t)
	s := jobs.NewStore(pool)
	ctx := context.Background()
	k, mint := key("tok"), key("mint")
	if _, err := s.TouchSession(ctx, k, time.Hour); !errors.Is(err, jobs.ErrNoSession) {
		t.Fatalf("unknown key: %v", err)
	}
	if err := s.CreateSession(ctx, k, mint, 2); err != nil {
		t.Fatal(err)
	}
	if refreshed, err := s.TouchSession(ctx, k, time.Hour); err != nil || refreshed {
		t.Fatalf("fresh session: %v %v", refreshed, err)
	}
	// last_seen older than the refresh interval is renewed (and reported)
	if _, err := pool.Exec(ctx, `UPDATE sessions SET last_seen = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if refreshed, err := s.TouchSession(ctx, k, 24*time.Hour); err != nil || !refreshed {
		t.Fatalf("stale session not refreshed: %v %v", refreshed, err)
	}
	// idle beyond the TTL: no longer accepted, then removed
	if _, err := pool.Exec(ctx, `UPDATE sessions SET last_seen = now() - interval '3 hours'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TouchSession(ctx, k, time.Hour); !errors.Is(err, jobs.ErrNoSession) {
		t.Fatalf("expired session accepted: %v", err)
	}
	if err := s.CreateSession(ctx, key("tok2"), mint, 2); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteIdleSessions(ctx, time.Hour); err != nil || n != 1 {
		t.Fatalf("deleted %d %v", n, err)
	}
	// mint limit per IP key
	var qe *jobs.QuotaError
	if err := s.CreateSession(ctx, key("tok3"), mint, 2); !errors.As(err, &qe) || qe.Kind != jobs.QuotaSessionsPerHour || qe.RetryAfter <= 0 {
		t.Fatalf("third session of an IP: %v", err)
	}
	if err := s.CreateSession(ctx, key("tok3"), key("other ip"), 2); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, key("tok4"), mint, 0); err != nil {
		t.Fatalf("unlimited: %v", err)
	}
	if err := s.CreateSession(ctx, []byte("short"), mint, 0); err == nil {
		t.Fatal("short key accepted")
	}
}
