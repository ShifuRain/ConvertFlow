package blob_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/ShifuRain/convertflow/internal/blob"
)

var ctx = context.Background()

func newFS(t *testing.T) (*blob.LocalFS, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "data") // the parent is ours too, so escapes are visible
	l, err := blob.NewLocalFS(root)
	if err != nil {
		t.Fatal(err)
	}
	return l, root
}

func TestNewLocalFSCreatesPrivateDirs(t *testing.T) {
	l, root := newFS(t)
	if l.Root() != root {
		t.Fatalf("root %q want %q", l.Root(), root)
	}
	for _, d := range []string{"in", "out"} {
		st, err := os.Stat(filepath.Join(root, d))
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, st, err)
		}
	}
}

func TestOpenLocalFSMakesRootAbsolute(t *testing.T) {
	t.Chdir(t.TempDir())
	l := blob.OpenLocalFS("rel/data")
	if !filepath.IsAbs(l.Root()) {
		t.Fatalf("root %q is not absolute", l.Root())
	}
	if _, err := os.Stat("rel"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("OpenLocalFS must not create anything")
	}
}

func TestCreateInputIsExclusiveAndPrivate(t *testing.T) {
	l, root := newFS(t)
	w, loc, err := l.CreateInput(ctx, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if loc != filepath.Join(root, "in", "abc") {
		t.Fatalf("locator %q", loc)
	}
	_, _ = io.WriteString(w, "data")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(loc)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st, err)
	}
	if _, _, err := l.CreateInput(ctx, "abc"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: want ErrExist, got %v", err)
	}
	if b, _ := os.ReadFile(loc); string(b) != "data" {
		t.Fatalf("existing input overwritten: %q", b)
	}
	// A pre-placed symlink is never followed (O_EXCL fails on any existing name).
	target := filepath.Join(filepath.Dir(root), "target")
	_ = os.WriteFile(target, []byte("keep"), 0o600)
	_ = os.Symlink(target, filepath.Join(root, "in", "link"))
	if _, _, err := l.CreateInput(ctx, "link"); err == nil {
		t.Fatal("create through a symlink must fail")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("symlink target modified")
	}
}

func TestCreateInputRejectsNonNames(t *testing.T) {
	l, _ := newFS(t)
	for _, id := range []string{"", ".", "..", "../x", "a/b", `a\b`, "a\x00b"} {
		if _, _, err := l.CreateInput(ctx, id); err == nil {
			t.Errorf("%q: want error", id)
		}
	}
}

func TestNewOutputPerAttemptName(t *testing.T) {
	l, root := newFS(t)
	p, commit, err := l.NewOutput(ctx, "id", 3, "webm")
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(root, "out", "id.3.webm") {
		t.Fatalf("path %q", p)
	}
	loc, err := commit(ctx)
	if err != nil || loc != p {
		t.Fatalf("commit %q %v", loc, err)
	}
	for _, bad := range [][2]string{{"", "webm"}, {"../x", "webm"}, {"a.b", "webm"}, {"id", ""}, {"id", "a/b"}} {
		if _, _, err := l.NewOutput(ctx, bad[0], 1, bad[1]); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestOpenOutput(t *testing.T) {
	l, root := newFS(t)
	p := filepath.Join(root, "out", "id.1.wav")
	_ = os.WriteFile(p, []byte("RIFFdata"), 0o600)
	r, info, err := l.OpenOutput(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "RIFFdata" || info.Size != 8 || info.ModTime.IsZero() {
		t.Fatalf("got %q %+v", b, info)
	}
	dir := filepath.Join(root, "out", "dir.1.wav")
	_ = os.Mkdir(dir, 0o700)
	for _, loc := range []string{"", filepath.Join(root, "out", "missing"), dir} {
		if _, _, err := l.OpenOutput(ctx, loc); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%q: want ErrNotExist, got %v", loc, err)
		}
	}
}

func TestLocalInputIsThePathItself(t *testing.T) {
	l, root := newFS(t)
	loc := filepath.Join(root, "in", "x")
	p, cleanup, err := l.LocalInput(ctx, loc, t.TempDir())
	if err != nil || p != loc || cleanup == nil {
		t.Fatalf("%q %v", p, err)
	}
	cleanup()
}

// Moved from the API's and janitor's removeData: containment, missing-is-success, and (new in the
// seam) no symlink or non-regular file is ever removed.
func TestRemoveSafety(t *testing.T) {
	l, root := newFS(t)
	parent := filepath.Dir(root)
	in := filepath.Join(root, "in", "a")
	_ = os.WriteFile(in, []byte("x"), 0o600)
	if err := l.Remove(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(in); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("not removed")
	}
	if err := l.Remove(ctx, in); err != nil {
		t.Fatalf("missing file must be success, got %v", err)
	}
	if err := l.Remove(ctx, ""); err != nil {
		t.Fatalf("empty locator: %v", err)
	}

	outside := filepath.Join(parent, "precious")
	_ = os.WriteFile(outside, []byte("keep"), 0o600)
	for _, p := range []string{outside, root, parent, filepath.Join(root, "..", "precious"), filepath.Join(root, "in", "..", "..", "precious"), "/etc/passwd"} {
		if err := l.Remove(ctx, p); !errors.Is(err, blob.ErrOutsideRoot) {
			t.Errorf("%q: want ErrOutsideRoot, got %v", p, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("file outside the data dir was removed")
	}

	link := filepath.Join(root, "out", "link.1.wav")
	_ = os.Symlink(outside, link)
	if err := l.Remove(ctx, link); !errors.Is(err, blob.ErrNotRegular) {
		t.Fatalf("symlink: want ErrNotRegular, got %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("symlink removed")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("symlink target removed")
	}
	dir := filepath.Join(root, "in", "dir")
	_ = os.Mkdir(dir, 0o700)
	if err := l.Remove(ctx, dir); !errors.Is(err, blob.ErrNotRegular) {
		t.Fatalf("dir: want ErrNotRegular, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "in")); err != nil {
		t.Fatal("in/ removed")
	}
}

func TestListSkipsNonRegular(t *testing.T) {
	l, root := newFS(t)
	_ = os.WriteFile(filepath.Join(root, "out", "a.1.wav"), []byte("abc"), 0o600)
	_ = os.Mkdir(filepath.Join(root, "out", "d"), 0o700)
	_ = os.Symlink(filepath.Join(root, "out", "a.1.wav"), filepath.Join(root, "out", "s"))
	es, err := l.List(ctx, blob.KindOut)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Name != "a.1.wav" || es[0].Locator != filepath.Join(root, "out", "a.1.wav") || es[0].Info.Size != 3 {
		t.Fatalf("entries %+v", es)
	}
	if es, err := l.List(ctx, blob.KindIn); err != nil || len(es) != 0 {
		t.Fatalf("in: %v %v", es, err)
	}
	if _, err := blob.OpenLocalFS(filepath.Join(root, "nope")).List(ctx, blob.KindIn); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir: want ErrNotExist, got %v", err)
	}
}

func TestFreeBytes(t *testing.T) {
	l, _ := newFS(t)
	if n, err := l.FreeBytes(ctx); err != nil || n <= 0 {
		t.Fatalf("free %d %v", n, err)
	}
	if _, err := blob.OpenLocalFS(filepath.Join(t.TempDir(), "nope")).FreeBytes(ctx); err == nil {
		t.Fatal("missing root: want error")
	}
}

func TestKindString(t *testing.T) {
	if blob.KindIn.String() != "in" || blob.KindOut.String() != "out" {
		t.Fatal("kind names")
	}
}
