package ffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type tailBuffer struct {
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if t.buf.Len() > t.max {
		b := t.buf.Bytes()
		t.buf = *bytes.NewBuffer(append([]byte(nil), b[len(b)-t.max:]...))
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return t.buf.String() }

// Run executes bin with args (no shell), reports progress parsed from `-progress pipe:1`,
// and returns the tail of stderr. Cancelling ctx kills the process.
func Run(ctx context.Context, bin string, args []string, dir string, durationSec float64, onProgress func(pct float64)) (string, error) {
	// #nosec G204 -- bin is the configured ffmpeg path and args come from the allow-list builder; no shell is involved
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = minimalEnv()
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	tail := &tailBuffer{max: 4096}
	cmd.Stderr = tail
	if err := cmd.Start(); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "out_time_us=")
		if !ok || onProgress == nil || durationSec <= 0 {
			continue
		}
		us, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || us < 0 {
			continue
		}
		pct := float64(us) / 1e6 / durationSec * 100
		if pct > 99.9 {
			pct = 99.9
		}
		onProgress(pct)
	}
	// Drain so ffmpeg can never block on a full stdout pipe if the scanner stopped early.
	_, _ = io.Copy(io.Discard, stdout)
	err = cmd.Wait()
	return tail.String(), err
}

// Sanitize makes FFmpeg output safe to show users: strips the given paths, collapses
// whitespace and truncates to 500 bytes.
func Sanitize(msg string, paths ...string) string {
	for _, p := range paths {
		if p != "" {
			msg = strings.ReplaceAll(msg, p, "<data>")
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 500 {
		msg = msg[:500]
	}
	return strings.ToValidUTF8(msg, "")
}
