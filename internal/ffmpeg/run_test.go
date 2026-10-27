package ffmpeg_test

import (
	"context"
	"errors"
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

// stub writes an executable shell script that stands in for ffmpeg.
func stub(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func runStub(t *testing.T, body string, stall time.Duration) (string, error, time.Duration) {
	t.Helper()
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tail, err := ffmpeg.RunWith(ctx, stub(t, body), nil, t.TempDir(), ffmpeg.RunOpts{StallTimeout: stall})
	return tail, err, time.Since(start)
}

func TestStallWatchdogKillsSilentProcess(t *testing.T) {
	// An encoder that failed to start but did not exit: no progress, no CPU.
	tail, err, took := runStub(t, "echo 'encoder init failed' >&2\nexec sleep 30\n", 500*time.Millisecond)
	if !errors.Is(err, ffmpeg.ErrStalled) {
		t.Fatalf("want ErrStalled, got %v", err)
	}
	if took > 5*time.Second {
		t.Fatalf("watchdog took %s", took)
	}
	if !strings.Contains(tail, "encoder init failed") {
		t.Fatalf("stderr tail lost: %q", tail)
	}
}

func TestStallWatchdogIgnoresRepeatedReportsAndStderrChatter(t *testing.T) {
	// FFmpeg repeats its progress block every stats period even when nothing moves, and a stuck
	// pipeline can keep printing warnings: neither is progress.
	body := "while :; do printf 'frame=7\\nout_time_us=1000\\ntotal_size=48\\nprogress=continue\\n'; echo 'Past duration too large' >&2; sleep 0.1; done\n"
	_, err, took := runStub(t, body, 700*time.Millisecond)
	if !errors.Is(err, ffmpeg.ErrStalled) {
		t.Fatalf("want ErrStalled, got %v", err)
	}
	if took > 6*time.Second {
		t.Fatalf("watchdog took %s", took)
	}
}

func TestStallWatchdogSparesSlowButProgressingRun(t *testing.T) {
	// One report every 300ms with a growing counter, 3s in total, against a 700ms stall timeout.
	body := "i=0; while [ $i -lt 10 ]; do i=$((i+1)); printf 'frame=%d\\nout_time_us=N/A\\nprogress=continue\\n' $i; sleep 0.3; done\nprintf 'progress=end\\n'\n"
	_, err, _ := runStub(t, body, 700*time.Millisecond)
	if err != nil {
		t.Fatalf("progressing run was killed: %v", err)
	}
}

func TestStallWatchdogSparesBusyProcessWithoutReports(t *testing.T) {
	// A slow encoder working on its first frames (lookahead on a huge frame) reports nothing for a
	// long time but keeps a core busy: that is not a stall.
	body := "end=$(( $(date +%s) + 3 )); while [ $(date +%s) -lt $end ]; do :; done\n"
	_, err, _ := runStub(t, body, 700*time.Millisecond)
	if err != nil {
		t.Fatalf("busy run was killed: %v", err)
	}
}

func TestStallWatchdogDisabled(t *testing.T) {
	_, err, _ := runStub(t, "sleep 1\n", 0)
	if err != nil {
		t.Fatal(err)
	}
}
