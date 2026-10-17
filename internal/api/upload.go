package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
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

// createJob streams a multipart upload to disk. Fields "preset" and/or "options" (JSON) must
// precede the "file" field so invalid requests are rejected before any bytes are stored.
// Anything after the file part is ignored.
func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	limit := s.cfg.MaxUploadBytes + multipartSlack
	if r.ContentLength > limit {
		WriteProblem(w, http.StatusRequestEntityTooLarge, "Upload too large", "The file exceeds the configured limit.")
		return
	}
	r.Body = http.MaxBytesReader(w, idleDeadlineBody{r.Body, http.NewResponseController(w)}, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		WriteProblem(w, http.StatusBadRequest, "Bad request", "expected multipart/form-data")
		return
	}
	id := uuid.NewString()
	inPath := filepath.Join(s.cfg.DataDir, "in", id) // server-generated; the client filename is never a path
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
				s.uploadError(w, err)
				return
			}
			opts.Preset = strings.TrimSpace(string(b))
		case "options":
			b, err := readSmall(part, 4096)
			if err != nil {
				s.uploadError(w, err)
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
			cerr := f.Close()
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
			case cerr != nil:
				WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
				return
			}
		}
	}
	if !stored {
		WriteProblem(w, http.StatusBadRequest, "Bad request", "missing file field")
		return
	}
	// A completed upload must not be orphaned (file removed but row committed) by a client
	// disconnect racing the insert, so the DB work is detached from the request context.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Second)
	defer cancel()
	j := &jobs.Job{ID: id, Filename: filename, InputPath: inPath, Options: opts}
	if err := s.store.Create(ctx, j); err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	created = true
	s.audit.Event(r, "job_created", id, "preset", opts.Preset)
	got, err := s.store.Get(ctx, id)
	if err != nil {
		WriteProblem(w, http.StatusInternalServerError, "Internal error", "")
		return
	}
	w.Header().Set("Location", "/api/v1/jobs/"+id)
	s.writeJob(w, http.StatusAccepted, got)
}
