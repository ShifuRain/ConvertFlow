package ffmpeg

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > t.max {
		b := t.buf.Bytes()
		t.buf = *bytes.NewBuffer(append([]byte(nil), b[len(b)-t.max:]...))
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// ErrStalled is returned by RunWith when the stall watchdog killed FFmpeg.
var ErrStalled = errors.New("ffmpeg stalled")

// StallCPUFraction is the share of one CPU core below which a run that reports no progress counts
// as stalled. A blocked FFmpeg (an encoder that failed to start but did not exit, a deadlock) uses
// next to no CPU; an encoder that is still working on its first frames (lookahead of a slow encoder
// on a huge frame) keeps at least one core busy.
const StallCPUFraction = 0.05

// RunOpts tunes RunWith.
type RunOpts struct {
	// DurationSec is the input duration; <= 0 disables percentage progress.
	DurationSec float64
	// OnProgress receives the progress percentage (0..99.9) from `-progress pipe:1`.
	OnProgress func(pct float64)
	// StallTimeout kills the run (RunWith then returns ErrStalled) when, for this long, none of
	// FFmpeg's progress counters (out_time_us, frame, total_size) advanced and the process used
	// less than StallCPUFraction of one core on average. Zero disables the watchdog.
	StallTimeout time.Duration
}

// Run executes bin with args (no shell), reports progress parsed from `-progress pipe:1`,
// and returns the tail of stderr. Cancelling ctx kills the process.
func Run(ctx context.Context, bin string, args []string, dir string, durationSec float64, onProgress func(pct float64)) (string, error) {
	return RunWith(ctx, bin, args, dir, RunOpts{DurationSec: durationSec, OnProgress: onProgress})
}

// RunWith is Run with a stall watchdog (see RunOpts.StallTimeout).
func RunWith(ctx context.Context, bin string, args []string, dir string, o RunOpts) (string, error) {
	ctx, kill := context.WithCancel(ctx)
	defer kill()
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
	var wd *watchdog
	if o.StallTimeout > 0 {
		wd = newWatchdog(o.StallTimeout, cmd.Process.Pid, kill)
		go wd.run(ctx)
	}
	var counters progressCounters
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		if wd != nil && counters.advance(line) {
			wd.progressed()
		}
		v, ok := strings.CutPrefix(line, "out_time_us=")
		if !ok || o.OnProgress == nil || o.DurationSec <= 0 {
			continue
		}
		us, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil || us < 0 {
			continue
		}
		pct := float64(us) / 1e6 / o.DurationSec * 100
		if pct > 99.9 {
			pct = 99.9
		}
		o.OnProgress(pct)
	}
	// Drain so ffmpeg can never block on a full stdout pipe if the scanner stopped early.
	_, _ = io.Copy(io.Discard, stdout)
	err = cmd.Wait()
	if wd != nil && wd.fired() {
		return tail.String(), ErrStalled
	}
	return tail.String(), err
}

// progressCounters tracks the highest value seen for each counter FFmpeg reports with -progress.
// Only an increase counts as progress: FFmpeg repeats its report every stats period even when
// nothing moves, so the mere presence of output (or stderr chatter) is not progress.
type progressCounters struct {
	outTime, frame, size int64
}

func (c *progressCounters) advance(line string) bool {
	k, v, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}
	var p *int64
	switch k {
	case "out_time_us":
		p = &c.outTime
	case "frame":
		p = &c.frame
	case "total_size":
		p = &c.size
	default:
		return false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= *p {
		return false // "N/A", negative or unchanged
	}
	*p = n
	return true
}

// watchdog kills a run that made no progress and was essentially idle for a whole stall window.
type watchdog struct {
	timeout time.Duration
	pid     int
	kill    context.CancelFunc

	mu       sync.Mutex
	since    time.Time     // start of the current window: last progress, or last busy window
	cpuSince time.Duration // process CPU time at since
	stalled  bool
}

func newWatchdog(timeout time.Duration, pid int, kill context.CancelFunc) *watchdog {
	cpu, _ := processCPU(pid)
	return &watchdog{timeout: timeout, pid: pid, kill: kill, since: time.Now(), cpuSince: cpu}
}

func (w *watchdog) progressed() {
	cpu, _ := processCPU(w.pid)
	w.mu.Lock()
	w.since, w.cpuSince = time.Now(), cpu
	w.mu.Unlock()
}

func (w *watchdog) fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stalled
}

func (w *watchdog) run(ctx context.Context) {
	every := min(max(w.timeout/10, 10*time.Millisecond), time.Second)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cpu, _ := processCPU(w.pid) // unreadable (non-Linux, process gone): 0, i.e. only progress counts
		now := time.Now()
		w.mu.Lock()
		elapsed := now.Sub(w.since)
		if elapsed < w.timeout {
			w.mu.Unlock()
			continue
		}
		if float64(cpu-w.cpuSince) >= StallCPUFraction*float64(elapsed) {
			// No progress report, but the process is working (for example an encoder's lookahead
			// on very large frames): start a new window.
			w.since, w.cpuSince = now, cpu
			w.mu.Unlock()
			continue
		}
		w.stalled = true
		w.mu.Unlock()
		w.kill()
		return
	}
}

// clockTick is the unit of the utime/stime fields of /proc/<pid>/stat (USER_HZ, 100 on every
// Linux architecture Go supports in practice).
const clockTick = 10 * time.Millisecond

// processCPU returns the user+system CPU time of a process (all its threads) from /proc.
func processCPU(pid int) (time.Duration, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// The command name (field 2) is parenthesised and may contain spaces; fields after it are
	// space separated: state is field 3, utime field 14, stime field 15.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 13 {
		return 0, errors.New("malformed stat")
	}
	ut, err1 := strconv.ParseInt(f[11], 10, 64)
	st, err2 := strconv.ParseInt(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, errors.New("malformed stat")
	}
	return time.Duration(ut+st) * clockTick, nil
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
