package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
)

const (
	maxPresignBody     = 8 << 10
	defaultPresignTTL  = 15 * time.Minute
	maxDownloadURLLife = 5 * time.Minute // presigned output URLs are used at once by the browser
)

func isJSON(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// presignRequest is the JSON body of a direct upload. Exactly one of Preset and Options is set.
type presignRequest struct {
	Preset   *string       `json:"preset"`
	Options  *jobs.Options `json:"options"`
	Filename string        `json:"filename"`
	Size     *int64        `json:"size"`
}

type presignedUpload struct {
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
}

type presignResponse struct {
	ID        string          `json:"id"`
	Upload    presignedUpload `json:"upload"`
	ExpiresAt time.Time       `json:"expires_at"`
}

// createPresigned answers POST /api/v1/jobs with a JSON body (only routed here when the store is
// a Presigner): it validates the request exactly
// like a multipart upload (same option checks, size limit, queue cap and upload slot), creates the
// job in 'uploading' and returns a presigned POST form for the job's input key. The upload bytes
// go from the browser straight to the bucket; POST /api/v1/jobs/{id}/complete then queues the job.
// The URL and form fields are credentials and are never logged or audited.
func (s *Server) createPresigned(w http.ResponseWriter, r *http.Request) {
	if !s.checkCapacity(w, r, 0) || !s.ensureOwner(w, r, true) {
		return
	}
	body, err := readSmall(http.MaxBytesReader(w, r.Body, maxPresignBody), maxPresignBody)
	if err != nil {
		s.reject(w, r, http.StatusBadRequest, "Bad request", "the request body is too large or unreadable", "bad presign body")
		return
	}
	var req presignRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		s.reject(w, r, http.StatusUnprocessableEntity, "Invalid options", "the body must be a single JSON object with known fields", "invalid presign json")
		return
	}
	if (req.Preset == nil) == (req.Options == nil) {
		s.reject(w, r, http.StatusBadRequest, "Bad request", "send exactly one preset or options field", "preset or options required")
		return
	}
	var opts jobs.Options
	if req.Options != nil {
		opts = *req.Options
	} else {
		if len(*req.Preset) > 128 {
			s.reject(w, r, http.StatusBadRequest, "Bad request", "preset too long", "preset too long")
			return
		}
		opts.Preset = strings.TrimSpace(*req.Preset)
	}
	if opts.Preset == "" {
		s.reject(w, r, http.StatusBadRequest, "Bad request", "send the preset (or options) field", "missing preset")
		return
	}
	switch {
	case req.Size == nil:
		s.reject(w, r, http.StatusBadRequest, "Bad request", "size is required", "missing size")
		return
	case *req.Size <= 0:
		s.uploadError(w, r, "", errEmpty)
		return
	case *req.Size > s.cfg.MaxUploadBytes:
		s.uploadError(w, r, "", errTooLarge)
		return
	}
	if msg, ok := s.checkOptions(r, opts); !ok {
		s.reject(w, r, http.StatusUnprocessableEntity, "Invalid options", msg, "invalid options")
		return
	}

	id := uuid.NewString()
	expires := time.Now().UTC().Add(s.presignTTL).Truncate(time.Second)
	pp, err := s.presign.PresignUpload(r.Context(), id, s.cfg.MaxUploadBytes, s.presignTTL)
	if err != nil {
		s.serverFault(w, r, id, "presign_upload", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	j := &jobs.Job{ID: id, Status: jobs.StatusUploading, Filename: SanitizeFilename(req.Filename), InputPath: pp.Key,
		Options: opts, DeclaredSize: *req.Size, InputSize: *req.Size}
	s.stamp(r, j)
	var qe *jobs.QuotaError
	if err := s.store.CreateWithQuota(ctx, j, s.limits()); errors.As(err, &qe) {
		s.quotaReject(w, r, qe) // the presigned form is dropped unused
		return
	} else if err != nil {
		s.serverFault(w, r, id, "create_job", err)
		return
	}
	s.audit.Event(r, "upload_presigned", id, "preset", opts.Preset, "declared_size", *req.Size)
	w.Header().Set("Location", "/api/v1/jobs/"+id)
	writeJSON(w, http.StatusCreated, presignResponse{ID: id, Upload: presignedUpload{URL: pp.URL, Fields: pp.Fields}, ExpiresAt: expires})
}

// completeUpload answers POST /api/v1/jobs/{id}/complete: it checks the uploaded object (the
// store's size is the authority; the declared size must match it) and queues the job. It is
// idempotent: a job that already left 'uploading' is returned as is.
func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request) {
	if s.presign == nil {
		WriteProblem(w, http.StatusNotFound, "Not found", "no such endpoint")
		return
	}
	j, ok := s.lookup(w, r)
	if !ok {
		return
	}
	if j.Status != jobs.StatusUploading {
		s.completedOrGone(w, r, j)
		return
	}
	size, err := s.presign.StatInput(r.Context(), j.InputPath)
	if errors.Is(err, fs.ErrNotExist) {
		s.reject(w, r, http.StatusConflict, "Upload missing", "The file has not been uploaded yet.", "upload missing")
		return
	}
	if err != nil {
		s.serverFault(w, r, j.ID, "stat_input", err)
		return
	}
	// A rejected object is removed so the client can upload again while the form is valid.
	bad := func(status int, title, detail, reason string) {
		s.removeData(r.Context(), j.ID, j.InputPath)
		s.reject(w, r, status, title, detail, reason)
	}
	switch {
	case size < 1:
		bad(http.StatusBadRequest, "Empty file", "The uploaded file is empty.", "empty file")
		return
	case size > s.cfg.MaxUploadBytes:
		bad(http.StatusRequestEntityTooLarge, "Upload too large", "The file exceeds the configured limit.", "upload too large")
		return
	case j.DeclaredSize > 0 && size != j.DeclaredSize:
		bad(http.StatusConflict, "Size mismatch", "The uploaded file does not have the announced size.", "size mismatch")
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	switch err := s.store.MarkUploaded(ctx, j.ID); {
	case errors.Is(err, jobs.ErrNotFound): // a concurrent complete, delete or expiry won the race
		cur, gerr := s.store.Get(ctx, j.ID)
		if errors.Is(gerr, jobs.ErrNotFound) {
			WriteProblem(w, http.StatusNotFound, "Not found", "no such job")
			return
		}
		if gerr != nil {
			s.serverFault(w, r, j.ID, "get_job", gerr)
			return
		}
		s.completedOrGone(w, r, cur)
		return
	case err != nil:
		s.serverFault(w, r, j.ID, "mark_uploaded", err)
		return
	}
	s.audit.Event(r, "job_created", j.ID, "preset", j.Options.Preset)
	got, err := s.store.GetFor(ctx, j.ID, jobs.AllJobs()) // ownership was checked by lookup
	if err != nil {
		s.serverFault(w, r, j.ID, "get_job", err)
		return
	}
	s.writeJob(w, http.StatusOK, got)
}

// completedOrGone answers a complete call for a job that is no longer uploading: an expired
// upload is a conflict, anything else was completed before (200 with the job).
func (s *Server) completedOrGone(w http.ResponseWriter, r *http.Request, j *jobs.Job) {
	if j.Status == jobs.StatusFailed && j.Error == jobs.ErrUploadExpired {
		s.reject(w, r, http.StatusConflict, "Upload expired", "The upload was not completed in time; create a new job.", "upload expired")
		return
	}
	s.writeJob(w, http.StatusOK, j)
}

// redirectOutput sends the browser to a short-lived presigned GET for the output, so download
// bytes bypass the API. The URL is never logged.
func (s *Server) redirectOutput(w http.ResponseWriter, r *http.Request, j *jobs.Job, name, ctype string) {
	ttl := min(s.presignTTL, maxDownloadURLLife)
	u, err := s.presign.PresignDownload(r.Context(), j.OutputPath, downloadHeaders(name, ctype), ttl)
	if errors.Is(err, fs.ErrNotExist) {
		WriteProblem(w, http.StatusGone, "Output removed", "the result is no longer available")
		return
	}
	if err != nil {
		slog.Error("request failed", "class", "presign_download", "job_id", j.ID, "err", err)
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u, http.StatusFound)
}
