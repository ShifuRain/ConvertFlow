// Package blob stores job inputs and outputs. The API, worker and janitor reach input/output
// bytes only through a Store, so the storage backend can be replaced without touching them.
//
// A Store hands out opaque locators (persisted in jobs.input_path / jobs.output_path). Callers
// never build or parse locators themselves; they only pass back what the Store returned.
package blob

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrInvalidLocator is returned (possibly wrapped) when a locator does not name an object of the
// Store it is passed to (a path outside the data directory, a key of the wrong form). Stores refuse
// such locators on reads as well as on Remove.
var ErrInvalidLocator = errors.New("blob: invalid locator")

// Info describes a stored object.
type Info struct {
	Size    int64
	ModTime time.Time
}

// Kind selects the input or the output namespace of a Store.
type Kind int

const (
	KindIn Kind = iota
	KindOut
)

// String returns "in" or "out" (also the directory name used by LocalFS).
func (k Kind) String() string {
	if k == KindOut {
		return "out"
	}
	return "in"
}

// Entry is one object found by List. Name is the object's base name (inputs: <uuid>; outputs:
// <uuid>.<attempt>.<ext>); Locator is what Remove and the job columns expect.
type Entry struct {
	Name    string
	Locator string
	Info    Info
}

// Store is the storage seam for job inputs and outputs.
//
// Errors for a missing object satisfy errors.Is(err, fs.ErrNotExist).
type Store interface {
	// CreateInput opens a new input for exclusive writing; it fails if the input already exists.
	// id must be server-generated. The returned locator is persisted in jobs.input_path. If the
	// upload fails after CreateInput succeeded, the caller removes the locator.
	CreateInput(ctx context.Context, id string) (w io.WriteCloser, locator string, err error)
	// OpenOutput opens a recorded output (jobs.output_path) for serving. Anything that is not a
	// servable object (missing, not a regular file) reports fs.ErrNotExist; a locator that does not
	// belong to the store reports ErrInvalidLocator.
	OpenOutput(ctx context.Context, locator string) (io.ReadSeekCloser, Info, error)
	// LocalInput makes an input available as a local file for FFmpeg. scratchDir is where a
	// remote store may place a copy. cleanup is always non-nil on success and must be called when
	// the file is no longer needed; it never deletes the stored input itself. A missing input
	// reports fs.ErrNotExist and a foreign locator ErrInvalidLocator; any other error is treated
	// by the worker as transient.
	LocalInput(ctx context.Context, locator, scratchDir string) (path string, cleanup func(), err error)
	// NewOutput returns a local path for the given attempt's output (a per-attempt name, so a
	// stale attempt can never touch a later one) and a commit func that publishes the finished
	// file and returns its locator. Until commit succeeds, the caller owns the local file.
	NewOutput(ctx context.Context, jobID string, attempt int, ext string) (localPath string, commit func(context.Context) (locator string, err error), err error)
	// Remove deletes a stored object. A missing object is success. Implementations refuse
	// locators that do not name one of their own objects.
	Remove(ctx context.Context, locator string) error
	// List returns the objects of one namespace (for the janitor). Only regular objects are
	// returned; a missing namespace reports fs.ErrNotExist.
	List(ctx context.Context, kind Kind) ([]Entry, error)
	// FreeBytes reports the space still available for new inputs. Stores without a meaningful
	// answer (object stores) return an error satisfying errors.Is(err, errors.ErrUnsupported);
	// callers then skip their free-space check.
	FreeBytes(ctx context.Context) (int64, error)
}

// PresignedPost is a browser upload form for one input: POST multipart/form-data to URL with
// every entry of Fields as a form field, followed by the file as the last field named "file".
type PresignedPost struct {
	URL    string
	Fields map[string]string
	Key    string // the locator the upload creates (jobs.input_path)
}

// DownloadHeaders are response headers a presigned download makes the object store send.
type DownloadHeaders struct {
	ContentDisposition string
	ContentType        string
}

// Presigner is an optional Store capability: stores that are reachable by browsers directly
// (object stores) hand out short-lived signed URLs, so upload and download bytes bypass the API.
// Presigned URLs and form fields are credentials: they must never be logged.
type Presigner interface {
	// PresignUpload returns a POST form that can create exactly the input of job id, with a size
	// of 1..maxBytes bytes, until ttl has passed.
	PresignUpload(ctx context.Context, id string, maxBytes int64, ttl time.Duration) (PresignedPost, error)
	// StatInput returns the stored size of an input (fs.ErrNotExist if it was not uploaded).
	StatInput(ctx context.Context, locator string) (size int64, err error)
	// PresignDownload returns a GET URL for a recorded output that is valid for ttl and makes the
	// store answer with the given headers.
	PresignDownload(ctx context.Context, locator string, h DownloadHeaders, ttl time.Duration) (string, error)
	// UploadOrigin is the origin (scheme://host[:port]) that presigned upload forms are posted
	// to, so the API can allow exactly that origin in its Content-Security-Policy. "" means
	// unknown: the API then does not offer direct uploads.
	UploadOrigin() string
}
