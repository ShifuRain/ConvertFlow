// Package ffmpeg builds, runs and supervises FFmpeg/ffprobe processes.
package ffmpeg

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
)

var (
	ErrNoVideo = errors.New("input has no video stream")
	ErrNoAudio = errors.New("input has no audio stream")
)

type Spec struct {
	Input, Output string
	Preset        presets.Preset
	Cand          presets.Candidate
	Opts          jobs.Options
	HasVideo      bool
	HasAudio      bool
	VAAPIDevice   string
	// MaxOutputBytes, when > 0, is passed as -fs so FFmpeg itself stops writing at the cap.
	// A run that stops at the cap yields a truncated file; callers must treat
	// size >= MaxOutputBytes as over-limit.
	MaxOutputBytes int64
}

var x26xPresets = []string{"veryslow", "slower", "slow", "medium", "medium", "fast", "fast", "faster", "veryfast", "superfast", "ultrafast"}

var defaultCRF = map[string]int{
	"libsvtav1": 35, "libvpx-vp9": 33, "libx264": 23, "libx265": 28,
	"av1_nvenc": 32, "av1_qsv": 32, "av1_vaapi": 32,
}

var lossyAudio = map[string]bool{"libopus": true, "libvorbis": true, "libmp3lame": true, "aac": true}
var losslessAudio = map[string]bool{"flac": true, "pcm_s16le": true}

// BuildArgs is the only place FFmpeg command lines are assembled. Every value comes from a
// fixed set or a validated integer; no user string reaches the argument list except the two
// absolute server-generated paths.
func BuildArgs(s Spec) ([]string, error) {
	if !filepath.IsAbs(s.Input) || !filepath.IsAbs(s.Output) {
		return nil, errors.New("input and output must be absolute paths")
	}
	if err := s.Opts.Validate(); err != nil {
		return nil, err
	}
	video := s.Preset.Kind == presets.KindVideo
	if _, ok := defaultCRF[s.Cand.Video]; video && !ok {
		return nil, fmt.Errorf("unsupported video encoder %q", s.Cand.Video)
	}
	if !lossyAudio[s.Cand.Audio] && !losslessAudio[s.Cand.Audio] {
		return nil, fmt.Errorf("unsupported audio encoder %q", s.Cand.Audio)
	}
	if video && !s.HasVideo {
		return nil, ErrNoVideo
	}
	if !video && !s.HasAudio {
		return nil, ErrNoAudio
	}

	a := []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-protocol_whitelist", "file"}
	if s.Cand.Video == "av1_vaapi" {
		a = append(a, "-vaapi_device", s.VAAPIDevice)
	}
	a = append(a, "-format_whitelist", FormatWhitelist(), "-i", s.Input, "-map_metadata", "-1", "-map_chapters", "-1", "-sn", "-dn")
	if video {
		a = append(a, "-map", "0:V:0")
	} else {
		a = append(a, "-vn")
	}
	if s.HasAudio {
		a = append(a, "-map", "0:a:0")
	} else {
		a = append(a, "-an")
	}
	if video {
		a = append(a, videoArgs(s.Cand.Video, s.Opts)...)
	}
	if s.HasAudio {
		a = append(a, audioArgs(s.Cand.Audio, s.Opts)...)
	}
	if s.MaxOutputBytes > 0 {
		a = append(a, "-fs", strconv.FormatInt(s.MaxOutputBytes, 10))
	}
	a = append(a, "-f", s.Preset.Container, "-progress", "pipe:1", "-nostats", s.Output)
	return a, nil
}

func videoArgs(enc string, o jobs.Options) []string {
	speed := 6
	if o.Speed != nil {
		speed = *o.Speed
	}
	crf := defaultCRF[enc]
	if o.CRF != nil {
		crf = *o.CRF
	}
	c := strconv.Itoa(crf)

	var filters []string
	if o.MaxHeight != nil {
		filters = append(filters, fmt.Sprintf(`scale=-2:min(ih\,%d)`, *o.MaxHeight))
	}
	var enc_ []string
	switch enc {
	case "libsvtav1":
		enc_ = []string{"-c:v", enc, "-preset", strconv.Itoa(4 + speed*8/10), "-crf", c, "-pix_fmt", "yuv420p"}
	case "libvpx-vp9":
		enc_ = []string{"-c:v", enc, "-b:v", "0", "-crf", c, "-cpu-used", strconv.Itoa(speed * 8 / 10), "-row-mt", "1", "-pix_fmt", "yuv420p"}
	case "libx264":
		enc_ = []string{"-c:v", enc, "-preset", x26xPresets[speed], "-crf", c, "-pix_fmt", "yuv420p"}
	case "libx265":
		enc_ = []string{"-c:v", enc, "-preset", x26xPresets[speed], "-crf", c, "-pix_fmt", "yuv420p", "-tag:v", "hvc1"}
	case "av1_nvenc":
		enc_ = []string{"-c:v", enc, "-cq", c}
	case "av1_qsv":
		enc_ = []string{"-c:v", enc, "-global_quality", c}
	case "av1_vaapi":
		filters = append(filters, "format=nv12", "hwupload")
		enc_ = []string{"-c:v", enc, "-global_quality", c}
	}
	out := []string{}
	if len(filters) > 0 {
		out = append(out, "-vf", strings.Join(filters, ","))
	}
	return append(out, enc_...)
}

func audioArgs(enc string, o jobs.Options) []string {
	out := []string{"-c:a", enc}
	if lossyAudio[enc] {
		br := 128
		if o.AudioBitrateK != nil {
			br = *o.AudioBitrateK
		}
		out = append(out, "-b:a", strconv.Itoa(br)+"k")
	}
	if enc == "libmp3lame" {
		out = append(out, "-ac", "2")
	}
	return out
}
