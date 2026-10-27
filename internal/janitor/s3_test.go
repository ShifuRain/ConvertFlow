package janitor_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/ShifuRain/convertflow/internal/blob/s3test"
	"github.com/ShifuRain/convertflow/internal/db/dbtest"
	"github.com/ShifuRain/convertflow/internal/janitor"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
)

// Against a real bucket: orphans (no job), inputs of finished jobs and superseded outputs are
// swept through List/Remove; the recorded output, inputs of live jobs and foreign keys are kept;
// abandoned uploads are failed and their object removed.
func TestSweepS3Bucket(t *testing.T) {
	e := s3test.New(t)
	s := e.Store(t)
	pool := dbtest.New(t)
	store := jobs.NewStore(pool)
	var log strings.Builder
	jn := &janitor.Janitor{Store: store, Audit: audit.New(&log), Blob: s, Retention: 24 * time.Hour, StaleAfter: time.Minute,
		MaxAttempts: 3, OrphanGrace: time.Nanosecond, JobTimeout: time.Nanosecond, UploadTTL: time.Minute}
	ctx := context.Background()

	put := func(id string) string {
		w, loc, err := s.CreateInput(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("x"))
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return loc
	}
	out := func(id string, attempt int) string {
		p, commit, err := s.NewOutput(ctx, id, attempt, "wav")
		if err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(p, []byte("o"), 0o600)
		loc, err := commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	exists := func(loc string) bool {
		if strings.HasPrefix(loc, "in/") {
			_, err := s.StatInput(ctx, loc)
			return !errors.Is(err, fs.ErrNotExist)
		}
		r, _, err := s.OpenOutput(ctx, loc)
		if err == nil {
			_ = r.Close()
		}
		return err == nil
	}

	orphanIn := put(uuid.NewString())
	orphanOut := out(uuid.NewString(), 1)

	queued := uuid.NewString()
	queuedIn := put(queued)
	_ = store.Create(ctx, &jobs.Job{ID: queued, Filename: "q", InputPath: queuedIn, Options: jobs.Options{Preset: "audio-wav"}})

	done := uuid.NewString()
	doneIn := put(done)
	_ = store.Create(ctx, &jobs.Job{ID: done, Filename: "d", InputPath: doneIn, Options: jobs.Options{Preset: "audio-wav"}})
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'done', finished_at = now(), output_path = $2 WHERE id = $1::uuid`, done, "out/"+done+".2.wav"); err != nil {
		t.Fatal(err)
	}
	superseded := out(done, 1)
	recorded := out(done, 2)

	abandoned := uuid.NewString()
	abandonedIn := put(abandoned)
	_ = store.Create(ctx, &jobs.Job{ID: abandoned, Status: jobs.StatusUploading, Filename: "a", InputPath: abandonedIn, Options: jobs.Options{Preset: "audio-wav"}})
	if _, err := pool.Exec(ctx, `UPDATE jobs SET updated_at = now() - interval '2 hours' WHERE id = $1::uuid`, abandoned); err != nil {
		t.Fatal(err)
	}
	uploading := uuid.NewString()
	uploadingIn := put(uploading)
	_ = store.Create(ctx, &jobs.Job{ID: uploading, Status: jobs.StatusUploading, Filename: "u", InputPath: uploadingIn, Options: jobs.Options{Preset: "audio-wav"}})

	if err := jn.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	for loc, want := range map[string]bool{orphanIn: false, orphanOut: false, queuedIn: true, doneIn: false,
		superseded: false, recorded: true, abandonedIn: false, uploadingIn: true} {
		if got := exists(loc); got != want {
			t.Errorf("%s: exists=%v, want %v", loc, got, want)
		}
	}
	if j, _ := store.Get(ctx, abandoned); j.Status != jobs.StatusFailed || j.Error != jobs.ErrUploadExpired {
		t.Errorf("abandoned upload: %+v", j)
	}
	if strings.Contains(log.String(), e.Opts.Bucket) || strings.Contains(log.String(), e.Opts.Endpoint) {
		t.Errorf("audit log names the bucket or endpoint: %s", log.String())
	}
	var _ blob.Store = s
}
