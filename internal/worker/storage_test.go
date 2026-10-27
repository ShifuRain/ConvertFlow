package worker_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/audit"
	"github.com/ShifuRain/convertflow/internal/blob"
	"github.com/ShifuRain/convertflow/internal/blob/s3test"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/testutil"
	"github.com/google/uuid"
)

// faultBlob wraps the local store and injects storage failures.
type faultBlob struct {
	*blob.LocalFS
	inputErr  error
	commitErr error
}

func (f faultBlob) LocalInput(ctx context.Context, loc, scratch string) (string, func(), error) {
	if f.inputErr != nil {
		return "", nil, f.inputErr
	}
	return f.LocalFS.LocalInput(ctx, loc, scratch)
}

func (f faultBlob) NewOutput(ctx context.Context, id string, attempt int, ext string) (string, func(context.Context) (string, error), error) {
	p, commit, err := f.LocalFS.NewOutput(ctx, id, attempt, ext)
	if err != nil || f.commitErr == nil {
		return p, commit, err
	}
	return p, func(context.Context) (string, error) { return "", f.commitErr }, nil
}

// A transient storage failure (input download or output upload) never fails the job: the worker
// releases it, the input is kept, and the janitor's stale requeue retries it within MaxAttempts.
func TestTransientStorageErrorsReleaseForRetry(t *testing.T) {
	outage := errors.New("dial tcp 10.0.0.5:9000: connect: connection refused")
	for name, fb := range map[string]func(dir string) faultBlob{
		"input download": func(dir string) faultBlob { return faultBlob{LocalFS: blob.OpenLocalFS(dir), inputErr: outage} },
		"output upload":  func(dir string) faultBlob { return faultBlob{LocalFS: blob.OpenLocalFS(dir), commitErr: outage} },
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, defaultLimits)
			var log strings.Builder
			e.w.Audit = audit.New(&log)
			e.w.Blob = fb(e.dir)
			src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
			id := e.enqueue(t, src, "audio-wav")
			ctx := context.Background()
			if ok, err := e.w.ProcessOne(ctx); !ok || err != nil {
				t.Fatalf("ProcessOne: %v %v", ok, err)
			}
			j, _ := e.store.Get(ctx, id)
			if j.Status.Terminal() || j.Error != "" {
				t.Fatalf("a storage outage must not end the job: %+v", j)
			}
			if !exists(j.InputPath) || len(e.outFiles(t)) != 0 {
				t.Fatalf("input must be kept and no output left: in=%v outs=%v", exists(j.InputPath), e.outFiles(t))
			}
			if !strings.Contains(log.String(), `"event":"job_released"`) || strings.Contains(log.String(), "10.0.0.5") {
				t.Fatalf("audit: %s", log.String())
			}
			// The janitor's next pass requeues it at once (the lease was expired) ...
			if rq, _, err := e.store.RequeueStale(ctx, time.Hour, 2); err != nil || rq != 1 {
				t.Fatalf("requeue: %d %v", rq, err)
			}
			// ... and after MaxAttempts the usual "attempts exhausted" failure applies.
			if ok, _ := e.w.ProcessOne(ctx); !ok {
				t.Fatal("requeued job not claimed")
			}
			if _, failed, _ := e.store.RequeueStale(ctx, time.Hour, 2); failed != 1 {
				t.Fatal("attempts exhausted must fail the job")
			}
		})
	}
}

// Permanent storage errors fail the job with a fixed message.
func TestPermanentStorageErrorsFailTheJob(t *testing.T) {
	for name, c := range map[string]struct {
		err error
		msg string
	}{
		"missing input":   {fs.ErrNotExist, "internal error"},
		"foreign locator": {blob.ErrOutsideRoot, "internal error"},
		"input too large": {blob.ErrTooLarge, "input exceeds the upload size limit"},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, defaultLimits)
			e.w.Blob = faultBlob{LocalFS: blob.OpenLocalFS(e.dir), inputErr: c.err}
			src := testutil.MakeClip(t, t.TempDir(), "a.wav", "audio")
			id := e.enqueue(t, src, "audio-wav")
			_, _ = e.w.ProcessOne(context.Background())
			j, _ := e.store.Get(context.Background(), id)
			if j.Status != jobs.StatusFailed || j.Error != c.msg {
				t.Fatalf("job %s %q", j.Status, j.Error)
			}
		})
	}
}

// The worker path through a real bucket: download to scratch, transcode, upload, complete, and
// remove the input; nothing is left in the scratch directory.
func TestWorkerWithS3Store(t *testing.T) {
	s3e := s3test.New(t)
	e := setup(t, defaultLimits)
	s3e.Opts.ScratchDir = e.w.ScratchDir
	s3e.Opts.MaxInputBytes = 1 << 30
	s := s3e.Store(t)
	e.w.Blob = s
	ctx := context.Background()

	enqueue := func(src, preset string) string {
		id := uuid.NewString()
		w, loc, err := s.CreateInput(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(src)
		_, _ = w.Write(b)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := e.store.Create(ctx, &jobs.Job{ID: id, Filename: "x", InputPath: loc, Options: jobs.Options{Preset: preset}}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := enqueue(testutil.MakeClip(t, t.TempDir(), "a.wav", "audio"), "audio-flac")
	if ok, err := e.w.ProcessOne(ctx); !ok || err != nil {
		t.Fatalf("ProcessOne: %v %v", ok, err)
	}
	j, _ := e.store.Get(ctx, id)
	if j.Status != jobs.StatusDone || j.OutputPath != "out/"+id+".1.flac" {
		t.Fatalf("job %+v", j)
	}
	r, info, err := s.OpenOutput(ctx, j.OutputPath)
	if err != nil || info.Size == 0 {
		t.Fatalf("output: %+v %v", info, err)
	}
	_ = r.Close()
	if _, err := s.StatInput(ctx, j.InputPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("input must be removed after completion: %v", err)
	}

	// A failing input: the stored message names neither the bucket nor the key and is not mangled
	// by stripping the key's directory ("in").
	garbage := filepath.Join(t.TempDir(), "g.mp4")
	_ = os.WriteFile(garbage, []byte("not media at all"), 0o600)
	gid := enqueue(garbage, "audio-wav")
	_, _ = e.w.ProcessOne(ctx)
	g, _ := e.store.Get(ctx, gid)
	if g.Status != jobs.StatusFailed || g.Error == "" {
		t.Fatalf("garbage job %+v", g)
	}
	for _, bad := range []string{s3e.Opts.Bucket, gid, "<data>put", "<data>g", e.w.ScratchDir} {
		if strings.Contains(g.Error, bad) {
			t.Fatalf("error %q contains %q", g.Error, bad)
		}
	}
	if ents, _ := os.ReadDir(e.w.ScratchDir); len(ents) != 0 {
		var names []string
		for _, en := range ents {
			names = append(names, en.Name())
		}
		t.Fatalf("scratch not cleaned: %v", names)
	}
}
