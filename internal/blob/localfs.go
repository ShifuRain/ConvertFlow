package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// LocalFS stores inputs in <root>/in/<id> and outputs in <root>/out/<id>.<attempt>.<ext>.
// Locators are the absolute file paths, so job rows written before the Store existed stay valid.
type LocalFS struct {
	root string // absolute and clean
}

var _ Store = (*LocalFS)(nil)

// NewLocalFS opens a LocalFS at root and creates its in/ and out/ directories (0700).
func NewLocalFS(root string) (*LocalFS, error) {
	l := OpenLocalFS(root)
	for _, k := range []Kind{KindIn, KindOut} {
		if err := os.MkdirAll(l.dir(k), 0o700); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// OpenLocalFS opens a LocalFS at root without creating anything.
func OpenLocalFS(root string) *LocalFS {
	if abs, err := filepath.Abs(root); err == nil {
		return &LocalFS{root: abs}
	}
	return &LocalFS{root: filepath.Clean(root)}
}

// Root returns the absolute data directory.
func (l *LocalFS) Root() string { return l.root }

func (l *LocalFS) dir(k Kind) string { return filepath.Join(l.root, k.String()) }

// validName reports whether s can be used as one path element of a server-generated name.
func validName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`+"\x00")
}

func (l *LocalFS) CreateInput(_ context.Context, id string) (io.WriteCloser, string, error) {
	if !validName(id) {
		return nil, "", errors.New("blob: invalid input id")
	}
	p := filepath.Join(l.dir(KindIn), id) // server-generated; the client filename is never a path
	// #nosec G304 -- path is server-generated under the data dir (never client-supplied); O_EXCL prevents overwrite
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, p, nil
}

// inside returns the absolute form of locator if it lies strictly inside the data directory,
// else ErrOutsideRoot. The check is lexical (symlinks inside the root are not resolved here; reads
// refuse non-regular files and Remove refuses symlinks).
func (l *LocalFS) inside(locator string) (string, error) {
	abs, err := filepath.Abs(locator)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(l.root, abs); err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}
	return abs, nil
}

// OpenOutput refuses locators outside the data directory with ErrOutsideRoot.
func (l *LocalFS) OpenOutput(_ context.Context, locator string) (io.ReadSeekCloser, Info, error) {
	if locator == "" {
		return nil, Info{}, fs.ErrNotExist
	}
	p, err := l.inside(locator)
	if err != nil {
		return nil, Info{}, err
	}
	// #nosec G304 -- p is a server-recorded output path (jobs.output_path) verified to lie inside the data dir
	f, err := os.Open(p)
	if err != nil {
		return nil, Info{}, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, Info{}, err
	}
	if !st.Mode().IsRegular() {
		_ = f.Close()
		return nil, Info{}, fmt.Errorf("blob: output is not a regular file: %w", fs.ErrNotExist)
	}
	return f, Info{Size: st.Size(), ModTime: st.ModTime()}, nil
}

// LocalInput returns the input file itself (no copy). Locators outside the data directory are
// refused with ErrOutsideRoot.
func (l *LocalFS) LocalInput(_ context.Context, locator, _ string) (string, func(), error) {
	if locator == "" {
		return "", nil, fs.ErrNotExist
	}
	p, err := l.inside(locator)
	if err != nil {
		return "", nil, err
	}
	return p, func() {}, nil
}

func (l *LocalFS) NewOutput(_ context.Context, jobID string, attempt int, ext string) (string, func(context.Context) (string, error), error) {
	if !validName(jobID) || strings.Contains(jobID, ".") || !validName(ext) {
		return "", nil, errors.New("blob: invalid output name")
	}
	// Per-attempt name: a stale worker's cleanup can never touch a later attempt's output.
	p := filepath.Join(l.dir(KindOut), jobID+"."+strconv.Itoa(attempt)+"."+ext)
	return p, func(context.Context) (string, error) { return p, nil }, nil
}

// Remove deletes a file recorded on a job, but only strictly inside the data directory
// (ErrOutsideRoot otherwise) and only if it is a regular file (ErrNotRegular otherwise: a symlink
// is never followed or removed). A missing file is success.
func (l *LocalFS) Remove(_ context.Context, locator string) error {
	if locator == "" {
		return nil
	}
	abs, err := l.inside(locator)
	if err != nil {
		return err
	}
	st, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return ErrNotRegular
	}
	// #nosec G703 G304 -- abs was verified above to lie strictly inside the data dir
	if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Errors returned by LocalFS when it refuses a locator. ErrOutsideRoot satisfies
// errors.Is(err, ErrInvalidLocator).
var (
	ErrOutsideRoot = fmt.Errorf("%w: path is outside the data dir", ErrInvalidLocator)
	ErrNotRegular  = errors.New("blob: not a regular file")
)

// List returns the regular files in <root>/in or <root>/out. Entries come from ReadDir, which
// does not follow symlinks; symlinks, directories and other non-regular entries are skipped.
func (l *LocalFS) List(ctx context.Context, kind Kind) ([]Entry, error) {
	dir := l.dir(kind)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(ents))
	for _, e := range ents {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !e.Type().IsRegular() { // DirEntry type comes from lstat semantics: symlinks are not regular
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue // vanished or replaced concurrently
		}
		out = append(out, Entry{Name: e.Name(), Locator: filepath.Join(dir, e.Name()),
			Info: Info{Size: info.Size(), ModTime: info.ModTime()}})
	}
	return out, nil
}

// FreeBytes reports the space available to unprivileged users on the data volume (statfs).
func (l *LocalFS) FreeBytes(context.Context) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(l.root, &st); err != nil {
		return 0, err
	}
	// #nosec G115 -- Bsize is a positive block size reported by statfs
	free := st.Bavail * uint64(st.Bsize)
	if free > math.MaxInt64 {
		return math.MaxInt64, nil
	}
	return int64(free), nil
}
