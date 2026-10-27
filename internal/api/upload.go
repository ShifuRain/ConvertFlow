package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/google/uuid"
)

const (
	multipartSlack  = 64 << 10
	maxFilenameLen  = 200
	maxExtLen       = 16
	uploadIdleLimit = 60 * time.Second // max silence between body reads before the request is cut
)

var (
	errTooLarge = errors.New("file exceeds the configured upload limit")
	errEmpty    = errors.New("file is empty")
)

// SanitizeFilename reduces a client-supplied filename to a safe display name: no path components,
// no control/format characters (NUL, CR/LF, bidi overrides, zero-width), no quotes, valid UTF-8,
// at most 200 bytes. It is only ever used for display; stored paths are server-generated.
func SanitizeFilename(s string) string {
	s = strings.ToValidUTF8(s, "")
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if r == '"' || r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return -1
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if s == "" {
		return "upload"
	}
	if len(s) > maxFilenameLen {
		ext := path.Ext(s)
		if len(ext) > maxExtLen {
			ext = ""
		}
		stem := s[:len(s)-len(ext)]
		for len(stem)+len(ext) > maxFilenameLen {
			_, size := utf8.DecodeLastRuneInString(stem)
			stem = stem[:len(stem)-size]
		}
		s = strings.TrimRight(stem, " .") + ext
		if strings.Trim(s, " .") == "" {
			return "upload"
		}
	}
	return s
}

// ContentDisposition builds a safe attachment header (RFC 6266): an ASCII-only quoted fallback
// plus an RFC 5987 percent-encoded filename*. The name is sanitized first, so no input can inject
// header lines or break out of the quoted string.
func ContentDisposition(name string) string {
	name = SanitizeFilename(name)
	var fb strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r > 0x7e || r == '"' || r == '\\' || r == ';' || r == '%':
			fb.WriteByte('_')
		default:
			fb.WriteRune(r)
		}
	}
	const hex = "0123456789ABCDEF"
	var enc strings.Builder
	for _, b := range []byte(name) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			strings.IndexByte("!#$&+-.^_`|~", b) >= 0:
			enc.WriteByte(b)
		default:
			enc.WriteByte('%')
			enc.WriteByte(hex[b>>4])
			enc.WriteByte(hex[b&15])
		}
	}
	return `attachment; filename="` + fb.String() + `"; filename*=UTF-8''` + enc.String()
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

// srvWriteError marks an error as coming from the destination (server) side of the copy, so it
// is never mistaken for a client read error.
type srvWriteError struct{ err error }

func (e srvWriteError) Error() string { return e.err.Error() }
func (e srvWriteError) Unwrap() error { return e.err }

type tagWriter struct{ w io.Writer }

func (t tagWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil {
		err = srvWriteError{err}
	}
	return n, err
}

// reject answers a refused upload and writes an audit record (client IP, status, reason; never content).
func (s *Server) reject(w http.ResponseWriter, r *http.Request, status int, title, detail, reason string) {
	s.audit.Event(r, "upload_rejected", "", "status", status, "reason", reason)
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "30")
	}
	WriteProblem(w, status, title, detail)
}

func isNoSpace(err error) bool {
	return errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT)
}

// serverFault logs and answers a server-side failure (5xx) without exposing internals.
func (s *Server) serverFault(w http.ResponseWriter, r *http.Request, id, class string, err error) {
	slog.Error("upload failed", "job_id", id, "class", class, "err", err)
	if isNoSpace(err) {
		s.reject(w, r, http.StatusInsufficientStorage, "Insufficient storage", "The server has run out of storage space.", "insufficient storage ("+class+")")
		return
	}
	s.audit.Event(r, "upload_failed", id, "status", http.StatusInternalServerError, "class", class)
	WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
}

func (s *Server) uploadError(w http.ResponseWriter, r *http.Request, id string, err error) {
	var mbe *http.MaxBytesError
	var swe srvWriteError
	switch {
	case errors.As(err, &swe):
		s.serverFault(w, r, id, "write", swe.err)
	case errors.Is(err, errTooLarge) || errors.As(err, &mbe):
		s.reject(w, r, http.StatusRequestEntityTooLarge, "Upload too large", "The file exceeds the configured limit.", "upload too large")
	case errors.Is(err, errEmpty):
		s.reject(w, r, http.StatusBadRequest, "Empty file", "The uploaded file is empty.", "empty file")
	default:
		s.reject(w, r, http.StatusBadRequest, "Upload failed", "The upload was interrupted or malformed.", "interrupted or malformed upload")
	}
}

// checkOptions validates options and confirms a live worker can run the preset.
func (s *Server) checkOptions(r *http.Request, o jobs.Options) (string, bool) {
	if err := o.Validate(); err != nil {
		return err.Error(), false
	}
	run, err := s.runnablePresets(r.Context())
	if err != nil {
		return "could not determine worker capabilities", false
	}
	for _, p := range run {
		if p.ID == o.Preset {
			return "", true
		}
	}
	return "preset " + o.Preset + " is not available on any running worker", false
}

// idleDeadlineBody extends the connection read deadline before every body read, so a multi-GB
// upload may take arbitrarily long as long as data keeps flowing, while a stalled client is cut.
type idleDeadlineBody struct {
	io.ReadCloser
	rc *http.ResponseController
}

func (b idleDeadlineBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(uploadIdleLimit)) // unsupported on test recorders: ignore
	return b.ReadCloser.Read(p)
}

// takeUploadSlot reserves one of the MaxConcurrentUploads slots (503 when none is free). The
// caller releases it with <-s.uploadSlots.
func (s *Server) takeUploadSlot(w http.ResponseWriter, r *http.Request) bool {
	select {
	case s.uploadSlots <- struct{}{}:
		return true
	default:
		s.reject(w, r, http.StatusServiceUnavailable, "Server busy", "Too many uploads are in progress; retry later.", "too many concurrent uploads")
		return false
	}
}

// checkCapacity applies the queue cap (503) and the free-space reserve (507) before a new job is
// accepted. announced is the expected input size (<= 0 if unknown). Stores without a free-space
// figure (object stores report errors.ErrUnsupported) skip the free-space check.
func (s *Server) checkCapacity(w http.ResponseWriter, r *http.Request, announced int64) bool {
	if s.cfg.MaxQueuedJobs > 0 {
		n, err := s.store.CountActive(r.Context())
		if err != nil {
			s.serverFault(w, r, "", "count_active", err)
			return false
		}
		if n >= s.cfg.MaxQueuedJobs {
			s.reject(w, r, http.StatusServiceUnavailable, "Server busy", "The job queue is full; retry later.", "queue full")
			return false
		}
	}
	if s.cfg.MinFreeBytes > 0 {
		free, err := s.blob.FreeBytes(r.Context())
		if errors.Is(err, errors.ErrUnsupported) {
			return true
		}
		if err != nil {
			s.serverFault(w, r, "", "statfs", err)
			return false
		}
		need := s.cfg.MinFreeBytes
		if announced > 0 {
			need += announced
		}
		if free < need {
			s.reject(w, r, http.StatusInsufficientStorage, "Insufficient storage", "The server does not have enough free storage.", "insufficient storage")
			return false
		}
	}
	return true
}

// errAborted aborts a streaming store upload whose request failed, so nothing is committed.
var errAborted = errors.New("upload aborted")

// createJob streams a multipart upload to the blob store. Fields "preset" or "options" (JSON), exactly one
// of them and once, must precede the "file" field so invalid requests are rejected before any
// bytes are stored. Unknown field names are rejected. Anything after the file part is ignored.
//
// When the store supports presigning, a request with Content-Type application/json is a direct
// (presigned) upload instead; see createPresigned. Without it, such a request is refused as before
// (400, expected multipart/form-data).
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	// Resource bounds are checked before reading any body bytes.
	if !s.takeUploadSlot(w, r) {
		return
	}
	defer func() { <-s.uploadSlots }()
	if s.presign != nil && isJSON(r) {
		s.createPresigned(w, r)
		return
	}
	limit := s.cfg.MaxUploadBytes + multipartSlack
	if r.ContentLength > limit {
		s.reject(w, r, http.StatusRequestEntityTooLarge, "Upload too large", "The file exceeds the configured limit.", "upload too large")
		return
	}
	if !s.checkCapacity(w, r, r.ContentLength) {
		return
	}

	r.Body = http.MaxBytesReader(w, idleDeadlineBody{r.Body, http.NewResponseController(w)}, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		s.reject(w, r, http.StatusBadRequest, "Bad request", "expected multipart/form-data", "not multipart")
		return
	}
	id := uuid.NewString() // server-generated; the client filename is never part of the stored name
	var inLoc string       // set once the input exists in the blob store
	var opts jobs.Options
	var filename string
	haveOpts := false
	created := false
	defer func() {
		if inLoc != "" && !created {
			if err := s.blob.Remove(context.WithoutCancel(r.Context()), inLoc); err != nil {
				slog.Error("removing partial upload failed", "job_id", id, "err", err)
			}
		}
	}()

	stored := false
	for !stored {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.uploadError(w, r, id, err)
			return
		}
		name := part.FormName()
		if (name == "preset" || name == "options") && haveOpts {
			s.reject(w, r, http.StatusBadRequest, "Bad request", "send exactly one preset or options field", "duplicate options field")
			return
		}
		switch name {
		case "preset":
			b, err := readSmall(part, 128)
			if err != nil {
				s.uploadError(w, r, id, err)
				return
			}
			haveOpts = true
			opts.Preset = strings.TrimSpace(string(b))
		case "options":
			b, err := readSmall(part, 4096)
			if err != nil {
				s.uploadError(w, r, id, err)
				return
			}
			haveOpts = true
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&opts); err != nil || dec.Decode(&struct{}{}) != io.EOF {
				s.reject(w, r, http.StatusUnprocessableEntity, "Invalid options", "options must be a single JSON object with known fields", "invalid options json")
				return
			}
		case "file":
			if opts.Preset == "" {
				s.reject(w, r, http.StatusBadRequest, "Bad request", "send the preset (or options) field before the file field", "missing preset before file")
				return
			}
			if msg, ok := s.checkOptions(r, opts); !ok {
				s.reject(w, r, http.StatusUnprocessableEntity, "Invalid options", msg, "invalid options")
				return
			}
			filename = SanitizeFilename(part.FileName())
			f, loc, err := s.blob.CreateInput(r.Context(), id)
			if err != nil {
				s.serverFault(w, r, id, "open_input", err)
				return
			}
			inLoc = loc
			n, err := io.Copy(tagWriter{f}, io.LimitReader(part, s.cfg.MaxUploadBytes+1))
			var cerr error
			if a, ok := f.(interface{ CloseWithError(error) error }); ok && (err != nil || n > s.cfg.MaxUploadBytes || n == 0) {
				_ = a.CloseWithError(errAborted) // streaming store: abort instead of committing a rejected upload
			} else {
				cerr = f.Close()
			}
			switch {
			case err != nil:
				s.uploadError(w, r, id, err)
				return
			case n > s.cfg.MaxUploadBytes:
				s.uploadError(w, r, id, errTooLarge)
				return
			case n == 0:
				s.uploadError(w, r, id, errEmpty)
				return
			case cerr != nil:
				s.serverFault(w, r, id, "close_input", cerr)
				return
			}
			stored = true
		default:
			s.reject(w, r, http.StatusBadRequest, "Bad request", "unknown form field", "unknown form field")
			return
		}
	}
	if !stored {
		s.reject(w, r, http.StatusBadRequest, "Bad request", "missing file field", "missing file")
		return
	}
	// A completed upload must not be orphaned (file removed but row committed) by a client
	// disconnect racing the insert, so the DB work is detached from the request context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	j := &jobs.Job{ID: id, Filename: filename, InputPath: inLoc, Options: opts}
	if err := s.store.Create(ctx, j); err != nil {
		s.serverFault(w, r, id, "create_job", err)
		return
	}
	created = true
	s.audit.Event(r, "job_created", id, "preset", opts.Preset)
	got, err := s.store.Get(ctx, id)
	if err != nil {
		s.serverFault(w, r, id, "get_job", err)
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+id)
	s.writeJob(w, http.StatusAccepted, got)
}
