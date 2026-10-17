package ffmpeg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Limits struct {
	MaxDurationSec float64
	MaxPixels      int64
	MaxStreams     int
}

type Stream struct {
	CodecType   string `json:"codec_type"`
	CodecName   string `json:"codec_name"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Duration    string `json:"duration"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

type ProbeResult struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
	Streams []Stream `json:"streams"`
}

// allowedFormats are plain media containers. Playlist/network/pattern demuxers
// (concat, hls, sdp, rtsp, image2, tee, lavfi, ...) are intentionally absent.
var allowedFormats = map[string]bool{
	"matroska": true, "webm": true, "mov": true, "mp4": true, "m4a": true, "3gp": true, "3g2": true, "mj2": true,
	"avi": true, "flv": true, "mpegts": true, "mpeg": true, "ogg": true, "wav": true, "flac": true, "mp3": true,
	"aac": true, "asf": true, "ac3": true, "eac3": true, "aiff": true, "caf": true, "ape": true, "wv": true,
	"mxf": true, "amr": true, "au": true, "w64": true, "ivf": true, "nut": true, "dts": true, "tta": true,
}

// FormatWhitelist is the allow-list as a value for FFmpeg's -format_whitelist input option, so the
// demuxer allow-list is enforced by FFmpeg itself (a demuxer outside it is never selected or opened)
// and not only by validating the probe result. FFmpeg matches any alias of a demuxer's name, so the
// grouped "mov,mp4,m4a,3gp,3g2,mj2" demuxer is covered by listing its member names.
func FormatWhitelist() string {
	names := make([]string, 0, len(allowedFormats))
	for n := range allowedFormats {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func ParseProbe(b []byte) (*ProbeResult, error) {
	var p ProbeResult
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, errors.New("unreadable probe output")
	}
	return &p, nil
}

func (p *ProbeResult) HasVideo() bool {
	for _, s := range p.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 {
			return true
		}
	}
	return false
}

func (p *ProbeResult) HasAudio() bool {
	for _, s := range p.Streams {
		if s.CodecType == "audio" {
			return true
		}
	}
	return false
}

// parseDuration returns (seconds, true) only for finite, non-negative values.
func parseDuration(s string) (float64, bool) {
	d, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || !(d >= 0) || math.IsInf(d, 0) {
		return 0, false
	}
	return d, true
}

// DurationSeconds returns the larger of the container and per-stream durations,
// or 0 when no valid duration is known (see DurationKnown).
func (p *ProbeResult) DurationSeconds() float64 {
	best := 0.0
	if d, ok := parseDuration(p.Format.Duration); ok {
		best = d
	}
	for _, s := range p.Streams {
		if d, ok := parseDuration(s.Duration); ok && d > best {
			best = d
		}
	}
	return best
}

// DurationKnown reports whether any finite, non-negative duration was probed.
func (p *ProbeResult) DurationKnown() bool {
	if _, ok := parseDuration(p.Format.Duration); ok {
		return true
	}
	for _, s := range p.Streams {
		if _, ok := parseDuration(s.Duration); ok {
			return true
		}
	}
	return false
}

// Validate checks the container allow-list, stream count, duration and dimensions.
// Limits semantics: MaxDurationSec <= 0 means "no duration limit"; MaxPixels and
// MaxStreams are always enforced (a zero value rejects everything with video/streams).
// An UNKNOWN duration (missing, N/A, NaN, Inf, negative) deliberately passes the
// duration check: the worker enforces a wall-clock timeout instead. A known duration
// above a positive limit fails.
func (p *ProbeResult) Validate(l Limits) error {
	ok := false
	for _, n := range strings.Split(p.Format.FormatName, ",") {
		if allowedFormats[strings.TrimSpace(n)] {
			ok = true
		}
	}
	if !ok {
		return errors.New("unsupported container format")
	}
	if len(p.Streams) == 0 {
		return errors.New("file contains no media streams")
	}
	if len(p.Streams) > l.MaxStreams {
		return fmt.Errorf("too many streams (limit %d)", l.MaxStreams)
	}
	if p.DurationKnown() && l.MaxDurationSec > 0 && p.DurationSeconds() > l.MaxDurationSec {
		return fmt.Errorf("duration exceeds limit of %.0f seconds", l.MaxDurationSec)
	}
	for _, s := range p.Streams {
		if s.Width < 0 || s.Height < 0 {
			return errors.New("invalid video dimensions")
		}
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 && (s.Width <= 0 || s.Height <= 0) {
			return errors.New("video dimensions unknown")
		}
		if int64(s.Width)*int64(s.Height) > l.MaxPixels {
			return errors.New("video dimensions exceed limit")
		}
	}
	return nil
}

func minimalEnv() []string { return []string{"PATH=" + os.Getenv("PATH")} }

// Probe runs ffprobe with a local-files-only protocol whitelist. dir is the working directory
// (use an empty scratch dir so relative references inside hostile files resolve to nothing).
// path must be a server-generated absolute path (it is passed as a "file:" URL via -i, so a leading
// dash is not parsed as an option). Errors never include ffprobe's stderr or file paths.
func Probe(ctx context.Context, bin, path, dir string) (*ProbeResult, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// #nosec G204 -- bin is the configured ffprobe path; path is server-generated, passed as "file:"+path with protocol_whitelist=file
	cmd := exec.CommandContext(ctx, bin, "-v", "error", "-protocol_whitelist", "file",
		"-format_whitelist", FormatWhitelist(),
		"-print_format", "json",
		"-show_entries", "format=format_name,duration:stream=codec_type,codec_name,width,height,duration:stream_disposition=attached_pic",
		"-i", "file:"+path)
	cmd.Dir = dir
	cmd.Env = minimalEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, errors.New("file is not a readable media file")
	}
	p, err := ParseProbe(out)
	if err != nil {
		return nil, nil, errors.New("file is not a readable media file")
	}
	return p, out, nil
}
