package ffmpeg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

func (p *ProbeResult) DurationSeconds() float64 {
	d, err := strconv.ParseFloat(p.Format.Duration, 64)
	if err != nil || d < 0 {
		return 0
	}
	return d
}

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
	if d := p.DurationSeconds(); l.MaxDurationSec > 0 && d > l.MaxDurationSec {
		return fmt.Errorf("duration exceeds limit of %.0f seconds", l.MaxDurationSec)
	}
	for _, s := range p.Streams {
		if int64(s.Width)*int64(s.Height) > l.MaxPixels {
			return errors.New("video dimensions exceed limit")
		}
	}
	return nil
}

func minimalEnv() []string { return []string{"PATH=" + os.Getenv("PATH")} }

// Probe runs ffprobe with a local-files-only protocol whitelist. dir is the working directory
// (use an empty scratch dir so relative references inside hostile files resolve to nothing).
// Errors never include ffprobe's stderr or file paths.
func Probe(ctx context.Context, bin, path, dir string) (*ProbeResult, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-v", "error", "-protocol_whitelist", "file",
		"-print_format", "json",
		"-show_entries", "format=format_name,duration:stream=codec_type,codec_name,width,height:stream_disposition=attached_pic",
		path)
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
