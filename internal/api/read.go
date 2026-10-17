package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
)

const (
	defaultMaxSSEStreams  = 64
	defaultSSEHeartbeat   = 15 * time.Second // keepalive comment when nothing changed
	defaultSSEMaxLifetime = 10 * time.Minute // hard cap per connection; EventSource reconnects
	sseWriteLimit         = 10 * time.Second // a stalled SSE reader is cut after this
	sseQueryTimeout       = 5 * time.Second

	downloadChunk     = 256 << 10
	downloadIdleLimit = 60 * time.Second // max time to push one chunk to a (slow) client

	healthPingTimeout = 2 * time.Second // below the 3s client timeout of `convertflow-api healthcheck`
	readQueryTimeout  = 10 * time.Second
)

// lookup validates the {id} path value (404 for anything that is not a canonical UUID, so a
// malformed id can never reach Postgres) and loads the job.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (*jobs.Job, bool) {
	id := r.PathValue("id")
	if !validID(id) {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
		return nil, false
	}
	j, err := s.store.Get(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
		return nil, false
	}
	if err != nil {
		s.readFault(w, r, "get_job", err)
		return nil, false
	}
	return j, true
}

func (s *Server) readFault(w http.ResponseWriter, r *http.Request, class string, err error) {
	slog.Error("request failed", "class", class, "path", r.URL.Path, "err", err)
	WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	js, err := s.store.List(r.Context(), 200)
	if err != nil {
		s.readFault(w, r, "list_jobs", err)
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

// events streams job state as Server-Sent Events. Bounds: at most MaxSSEStreams concurrent
// streams (503 beyond), one DB read per poll interval per stream, a keepalive comment while
// nothing changes, a hard maximum lifetime, and a rolling write deadline so a stalled reader
// cannot pin the connection. The stream ends on terminal status, deletion, client disconnect or server shutdown.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
		return
	}
	select {
	case s.sseSlots <- struct{}{}:
		defer func() { <-s.sseSlots }()
	default:
		w.Header().Set("Retry-After", "5")
		WriteProblem(w, http.StatusServiceUnavailable, "Server busy", "Too many progress streams are open; retry later.")
		return
	}
	rc := http.NewResponseController(w)
	if _, ok := w.(http.Flusher); !ok {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	j, err := s.fetch(r.Context(), id)
	if errors.Is(err, jobs.ErrNotFound) {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
		return
	}
	if err != nil {
		s.readFault(w, r, "sse_get", err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(format string, a ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteLimit)) // unsupported on test recorders: ignore
		if _, err := fmt.Fprintf(w, format, a...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("retry: 3000\n\n") {
		return
	}
	life := time.NewTimer(s.sseMaxLife)
	defer life.Stop()
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	lastWrite := time.Now()
	var last []byte
	for {
		b, _ := json.Marshal(toView(*j))
		if string(b) != string(last) {
			last = b
			if !send("data: %s\n\n", b) {
				return
			}
			lastWrite = time.Now()
		} else if time.Since(lastWrite) >= s.sseHeartbeat {
			if !send(": keepalive\n\n") {
				return
			}
			lastWrite = time.Now()
		}
		if j.Status.Terminal() {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-life.C:
			return
		case <-s.shutdown:
			send(": shutdown\n\n") // server is going away: tell the client, which reconnects
			return
		case <-tick.C:
		}
		if j, err = s.fetch(r.Context(), id); err != nil {
			return // deleted, cancelled request or DB trouble: the client reconnects
		}
	}
}

func (s *Server) fetch(ctx context.Context, id string) (*jobs.Job, error) {
	ctx, cancel := context.WithTimeout(ctx, sseQueryTimeout)
	defer cancel()
	return s.store.Get(ctx, id)
}

// outputContentType maps a preset's container to a fixed media type. It never depends on the
// user's filename.
var outputContentType = map[string]string{
	"webm": "video/webm", "mp4": "video/mp4", "ogg": "audio/ogg", "flac": "audio/flac",
	"mp3": "audio/mpeg", "wav": "audio/wav", "ipod": "audio/mp4",
}

// deadlineWriter pushes data in bounded chunks and renews the write deadline before each one, so
// a download may take arbitrarily long while a stalled client is cut after downloadIdleLimit.
type deadlineWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := p
		if len(c) > downloadChunk {
			c = c[:downloadChunk]
		}
		_ = d.rc.SetWriteDeadline(time.Now().Add(downloadIdleLimit))
		m, err := d.ResponseWriter.Write(c)
		n += m
		if err != nil {
			return n, err
		}
		p = p[m:]
	}
	return n, nil
}

func (d deadlineWriter) ReadFrom(src io.Reader) (int64, error) {
	var total int64
	for {
		_ = d.rc.SetWriteDeadline(time.Now().Add(downloadIdleLimit))
		n, err := io.CopyN(d.ResponseWriter, src, downloadChunk)
		total += n
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func (d deadlineWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

func (s *Server) output(w http.ResponseWriter, r *http.Request) {
	j, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if j.Status != jobs.StatusDone {
		WriteProblem(w, http.StatusConflict, "Output not ready", "job status is "+string(j.Status))
		return
	}
	gone := func() {
		WriteProblem(w, http.StatusGone, "Output removed", "the result is no longer available")
	}
	if j.OutputPath == "" {
		gone()
		return
	}
	f, err := os.Open(j.OutputPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			gone()
		} else {
			s.readFault(w, r, "open_output", err)
		}
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.readFault(w, r, "stat_output", err)
		return
	}
	if !st.Mode().IsRegular() {
		gone()
		return
	}
	ctype, ext := "application/octet-stream", strings.TrimPrefix(filepath.Ext(j.OutputPath), ".")
	if p, ok := presets.Get(j.Options.Preset); ok {
		ext = p.Ext
		if t, ok := outputContentType[p.Container]; ok {
			ctype = t
		}
	}
	name := strings.TrimSuffix(j.Filename, filepath.Ext(j.Filename))
	if ext != "" {
		name += "." + ext
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", ContentDisposition(name))
	h.Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(deadlineWriter{w, http.NewResponseController(w)}, r, "", st.ModTime(), f)
}

// removeData deletes a file recorded on a job, but only inside the data directory, and treats an
// already-missing file as success. Failures are logged (never sent to the client).
func (s *Server) removeData(id, p string) {
	if p == "" {
		return
	}
	root, err1 := filepath.Abs(s.cfg.DataDir)
	abs, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return
	}
	if rel, err := filepath.Rel(root, abs); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		slog.Warn("refusing to remove path outside the data dir", "job_id", id)
		return
	}
	// #nosec G703 -- abs was verified above to lie strictly inside the data dir
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Error("removing job file failed", "job_id", id, "err", err)
	}
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
		return
	}
	j, err := s.store.Delete(r.Context(), id)
	switch {
	case err == nil:
		s.removeData(id, j.InputPath)
		s.removeData(id, j.OutputPath)
		s.audit.Event(r, "job_deleted", id)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, jobs.ErrNotFound):
		WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
	case errors.Is(err, jobs.ErrBusy):
		// The job is probing or running: ask its worker to stop. The worker finalizes it as
		// canceled; the client deletes it again afterwards.
		if err := s.store.RequestCancel(r.Context(), id); err != nil {
			s.readFault(w, r, "request_cancel", err)
			return
		}
		cur, err := s.store.Get(r.Context(), id)
		switch {
		case errors.Is(err, jobs.ErrNotFound):
			WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
			return
		case err != nil:
			s.readFault(w, r, "get_job", err)
			return
		case cur.Status == jobs.StatusDone || cur.Status == jobs.StatusFailed:
			WriteProblem(w, http.StatusConflict, "Job already finished", "the job finished before it could be canceled; delete it again")
			return
		}
		s.audit.Event(r, "job_cancel_requested", id)
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": "canceling"})
	default:
		s.readFault(w, r, "delete_job", err)
	}
}

func (s *Server) listPresets(w http.ResponseWriter, r *http.Request) {
	run, err := s.runnablePresets(r.Context())
	if err != nil {
		s.readFault(w, r, "live_encoders", err)
		return
	}
	type pv struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Kind  string `json:"kind"`
		Ext   string `json:"ext"`
	}
	out := []pv{}
	for _, p := range run {
		out = append(out, pv{p.ID, p.Label, string(p.Kind), p.Ext})
	}
	writeJSON(w, http.StatusOK, out)
}

// healthz is the liveness/readiness probe used by the container HEALTHCHECK. It runs a single
// connection ping bounded by healthPingTimeout (no table access, so a slow query elsewhere
// cannot make it fail); only an unreachable or saturated database yields 503.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthPingTimeout)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		slog.Error("health check failed", "err", err)
		WriteProblem(w, http.StatusServiceUnavailable, "Unhealthy", "database unreachable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// metrics exposes Prometheus text format. Label values come from a fixed vocabulary only (job
// statuses); filenames, paths and client addresses are never used.
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readQueryTimeout)
	defer cancel()
	counts, err := s.store.Counts(ctx)
	if err != nil {
		s.readFault(w, r, "metrics_counts", err)
		return
	}
	live, err := s.store.LiveWorkers(ctx, liveWindow)
	if err != nil {
		s.readFault(w, r, "metrics_workers", err)
		return
	}
	var b strings.Builder
	b.WriteString("# HELP convertflow_jobs Jobs by status.\n# TYPE convertflow_jobs gauge\n")
	for _, st := range []string{"queued", "probing", "running", "done", "failed", "canceled"} {
		fmt.Fprintf(&b, "convertflow_jobs{status=%q} %d\n", st, counts[st])
	}
	b.WriteString("# HELP convertflow_workers_live Workers seen recently.\n# TYPE convertflow_workers_live gauge\n")
	fmt.Fprintf(&b, "convertflow_workers_live %d\n", live)
	b.WriteString("# HELP convertflow_sse_streams Open progress streams.\n# TYPE convertflow_sse_streams gauge\n")
	fmt.Fprintf(&b, "convertflow_sse_streams %d\n", len(s.sseSlots))
	b.WriteString("# HELP convertflow_uploads_in_flight Uploads currently being received.\n# TYPE convertflow_uploads_in_flight gauge\n")
	fmt.Fprintf(&b, "convertflow_uploads_in_flight %d\n", len(s.uploadSlots))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// runnablePresets returns the presets a single live worker can run. The flavor policy is enforced
// here too, so a stale or misconfigured worker can never widen what the API offers.
func (s *Server) runnablePresets(ctx context.Context) ([]presets.Preset, error) {
	ws, err := s.store.LiveWorkerEncoders(ctx, liveWindow)
	if err != nil {
		return nil, err
	}
	return presets.Runnable(ws, s.cfg.Flavor), nil
}
