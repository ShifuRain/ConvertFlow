// Package blob stores job inputs and outputs. The API, worker and janitor reach input/output
// bytes only through a Store, so the storage backend can be replaced without touching them.
//
// A Store hands out opaque locators (persisted in jobs.input_path / jobs.output_path). Callers
// never build or parse locators themselves; they only pass back what the Store returned.
package blob

import (
	"context"
	"io"
	"time"
)

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
	// servable object (missing, not a regular file) reports fs.ErrNotExist.
	OpenOutput(ctx context.Context, locator string) (io.ReadSeekCloser, Info, error)
	// LocalInput makes an input available as a local file for FFmpeg. scratchDir is where a
	// remote store may place a copy. cleanup is always non-nil on success and must be called when
	// the file is no longer needed; it never deletes the stored input itself.
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
	// FreeBytes reports the space still available for new inputs.
	FreeBytes(ctx context.Context) (int64, error)
}
