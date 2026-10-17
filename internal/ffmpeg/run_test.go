package ffmpeg_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/testutil"
)

func TestRunConverts(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	in := testutil.MakeClip(t, dir, "a.wav", "audio")
	out := filepath.Join(dir, "o.flac")
	var last float64
	var called bool
	tail, err := ffmpeg.Run(context.Background(), "ffmpeg",
		[]string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-i", in, "-c:a", "flac", "-f", "flac", "-progress", "pipe:1", "-nostats", out},
		dir, 1.0, func(p float64) { last = p; called = true })
	if err != nil {
		t.Fatalf("%v\n%s", err, tail)
	}
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatal("no output")
	}
	if !called || last < 0 || last > 99.9 {
		t.Fatalf("progress out of range: %v", last)
	}
}

func TestRunFailureTailIsSanitizable(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.wav")
	tail, err := ffmpeg.Run(context.Background(), "ffmpeg",
		[]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-i", missing, "-f", "null", "-"}, dir, 0, nil)
	if err == nil {
		t.Fatal("expected failure")
	}
	clean := ffmpeg.Sanitize(tail, dir)
	if strings.Contains(clean, dir) || clean == "" {
		t.Fatalf("sanitize failed: %q", clean)
	}
}

func TestRunCancel(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ffmpeg.Run(ctx, "ffmpeg",
			[]string{"-hide_banner", "-nostdin", "-loglevel", "error", "-re", "-f", "lavfi", "-i", "sine=duration=600", "-f", "null", "-"}, dir, 600, nil)
		done <- err
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error after cancel")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestSanitizeCollapsesAndTruncates(t *testing.T) {
	got := ffmpeg.Sanitize("a\n\nb   /data/in/x  c", "/data")
	if got != "a b <data>/in/x c" {
		t.Fatalf("got %q", got)
	}
	if n := len(ffmpeg.Sanitize(strings.Repeat("x", 5000))); n > 500 {
		t.Fatalf("not truncated: %d", n)
	}
}
