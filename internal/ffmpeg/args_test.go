package ffmpeg_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
	"github.com/ShifuRain/convertflow/internal/testutil"
)

func ip(i int) *int { return &i }

func spec(t *testing.T, presetID string, c presets.Candidate, o jobs.Options, hasV, hasA bool) ffmpeg.Spec {
	t.Helper()
	p, ok := presets.Get(presetID)
	if !ok {
		t.Fatalf("no preset %s", presetID)
	}
	o.Preset = presetID
	return ffmpeg.Spec{Input: "/data/in/x", Output: "/data/out/x.webm", Preset: p, Cand: c, Opts: o,
		HasVideo: hasV, HasAudio: hasA, VAAPIDevice: "/dev/dri/renderD128"}
}

func hasSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

func build(t *testing.T, s ffmpeg.Spec) []string {
	t.Helper()
	a, err := ffmpeg.BuildArgs(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestBuildArgsSVTAV1Defaults(t *testing.T) {
	a := build(t, spec(t, "video-av1", presets.Candidate{Video: "libsvtav1", Audio: "libopus"}, jobs.Options{}, true, true))
	for _, seq := range [][]string{
		{"-c:v", "libsvtav1"}, {"-preset", "8"}, {"-crf", "35"}, {"-pix_fmt", "yuv420p"},
		{"-c:a", "libopus"}, {"-b:a", "128k"}, {"-map", "0:V:0"}, {"-map", "0:a:0"},
		{"-f", "webm"}, {"-protocol_whitelist", "file"}, {"-map_metadata", "-1"},
	} {
		if !hasSeq(a, seq...) {
			t.Errorf("missing %v in %v", seq, a)
		}
	}
	if a[len(a)-1] != "/data/out/x.webm" {
		t.Errorf("output must be last, got %v", a)
	}
	if slices.Index(a, "-protocol_whitelist") > slices.Index(a, "-i") {
		t.Error("-protocol_whitelist must precede -i")
	}
}

func TestBuildArgsOptions(t *testing.T) {
	o := jobs.Options{Speed: ip(0), CRF: ip(20), MaxHeight: ip(720), AudioBitrateK: ip(192)}
	a := build(t, spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, o, true, true))
	for _, seq := range [][]string{
		{"-cpu-used", "0"}, {"-crf", "20"}, {"-b:v", "0"}, {"-b:a", "192k"}, {"-vf", `scale=-2:min(ih\,720)`},
	} {
		if !hasSeq(a, seq...) {
			t.Errorf("missing %v in %v", seq, a)
		}
	}
}

func TestBuildArgsX264Preset(t *testing.T) {
	a := build(t, spec(t, "video-h264", presets.Candidate{Video: "libx264", Audio: "aac"}, jobs.Options{Speed: ip(10)}, true, true))
	if !hasSeq(a, "-preset", "ultrafast") || !hasSeq(a, "-crf", "23") {
		t.Errorf("bad x264 args %v", a)
	}
}

func TestBuildArgsVAAPI(t *testing.T) {
	a := build(t, spec(t, "video-av1", presets.Candidate{Video: "av1_vaapi", Audio: "libopus"}, jobs.Options{MaxHeight: ip(1080)}, true, true))
	if !hasSeq(a, "-vaapi_device", "/dev/dri/renderD128") {
		t.Errorf("missing vaapi device: %v", a)
	}
	if !hasSeq(a, "-vf", `scale=-2:min(ih\,1080),format=nv12,hwupload`) {
		t.Errorf("bad vaapi filter: %v", a)
	}
}

func TestBuildArgsAudioPreset(t *testing.T) {
	a := build(t, spec(t, "audio-mp3", presets.Candidate{Audio: "libmp3lame"}, jobs.Options{}, true, true))
	if !slices.Contains(a, "-vn") || slices.Contains(a, "0:V:0") || !hasSeq(a, "-ac", "2") {
		t.Errorf("audio preset must drop video and downmix: %v", a)
	}
	a = build(t, spec(t, "audio-flac", presets.Candidate{Audio: "flac"}, jobs.Options{}, false, true))
	if slices.Contains(a, "-b:a") {
		t.Errorf("flac must not get a bitrate: %v", a)
	}
}

func TestBuildArgsNeedsVideo(t *testing.T) {
	_, err := ffmpeg.BuildArgs(spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, jobs.Options{}, false, true))
	if err != ffmpeg.ErrNoVideo {
		t.Fatalf("want ErrNoVideo, got %v", err)
	}
	_, err = ffmpeg.BuildArgs(spec(t, "audio-wav", presets.Candidate{Audio: "pcm_s16le"}, jobs.Options{}, true, false))
	if err != ffmpeg.ErrNoAudio {
		t.Fatalf("want ErrNoAudio, got %v", err)
	}
}

// An MP3 with cover art has a "video" stream that the probe marks attached_pic; HasVideo=false
// for it, and an audio preset must still map only audio.
func TestBuildArgsIgnoresCoverArt(t *testing.T) {
	a := build(t, spec(t, "audio-opus", presets.Candidate{Audio: "libopus"}, jobs.Options{}, false, true))
	if slices.Contains(a, "0:V:0") || !slices.Contains(a, "-vn") {
		t.Errorf("cover art must not be mapped: %v", a)
	}
}

func TestBuildArgsVideoOnlyInputDropsAudio(t *testing.T) {
	a := build(t, spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, jobs.Options{}, true, false))
	if !slices.Contains(a, "-an") || hasSeq(a, "-map", "0:a:0") {
		t.Errorf("expected -an: %v", a)
	}
}

func TestBuildArgsRejectsUnsafePaths(t *testing.T) {
	s := spec(t, "audio-wav", presets.Candidate{Audio: "pcm_s16le"}, jobs.Options{}, false, true)
	s.Input = "-i evil"
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("relative/dash-leading input must be rejected")
	}
	s = spec(t, "audio-wav", presets.Candidate{Audio: "pcm_s16le"}, jobs.Options{}, false, true)
	s.Output = "rel.wav"
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("relative output must be rejected")
	}
}

func TestBuildArgsRejectsInvalidOptions(t *testing.T) {
	s := spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, jobs.Options{CRF: ip(999)}, true, true)
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("invalid options must be rejected")
	}
}

func TestBuildArgsRejectsEncoderOutsideAllowList(t *testing.T) {
	s := spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9; rm -rf /", Audio: "libopus"}, jobs.Options{}, true, true)
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("unknown encoder must be rejected")
	}
}

func TestBuildArgsAudioPresetIgnoresCRFAndSpeed(t *testing.T) {
	o := jobs.Options{CRF: ip(30), Speed: ip(3), MaxHeight: ip(720)}
	a := build(t, spec(t, "audio-opus", presets.Candidate{Audio: "libopus"}, o, true, true))
	for _, bad := range []string{"-crf", "-preset", "-cpu-used", "-cq", "-global_quality", "-vf", "-c:v"} {
		if slices.Contains(a, bad) {
			t.Errorf("audio preset must not contain %s: %v", bad, a)
		}
	}
}

func argAfter(a []string, flag string) string {
	i := slices.Index(a, flag)
	if i < 0 || i+1 >= len(a) {
		return ""
	}
	return a[i+1]
}

func TestBuildArgsSpeedMappedIntoEncoderRange(t *testing.T) {
	cases := []struct {
		preset, enc, flag string
		max               int
	}{
		{"video-av1", "libsvtav1", "-preset", 13},
		{"video-vp9", "libvpx-vp9", "-cpu-used", 8},
	}
	for _, c := range cases {
		for sp := 0; sp <= 10; sp++ {
			a := build(t, spec(t, c.preset, presets.Candidate{Video: c.enc, Audio: "libopus"}, jobs.Options{Speed: ip(sp)}, true, true))
			v, err := strconv.Atoi(argAfter(a, c.flag))
			if err != nil || v < 0 || v > c.max {
				t.Errorf("%s speed %d: %s=%q out of 0..%d", c.enc, sp, c.flag, argAfter(a, c.flag), c.max)
			}
		}
	}
	for _, enc := range []string{"libx264", "libx265"} {
		for sp := 0; sp <= 10; sp++ {
			id := "video-h264"
			if enc == "libx265" {
				id = "video-h265"
			}
			a := build(t, spec(t, id, presets.Candidate{Video: enc, Audio: "aac"}, jobs.Options{Speed: ip(sp)}, true, true))
			if !slices.Contains([]string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow"}, argAfter(a, "-preset")) {
				t.Errorf("%s speed %d: bad preset %q", enc, sp, argAfter(a, "-preset"))
			}
		}
	}
	for _, enc := range []string{"av1_nvenc", "av1_qsv", "av1_vaapi"} {
		a := build(t, spec(t, "video-av1", presets.Candidate{Video: enc, Audio: "libopus"}, jobs.Options{Speed: ip(10)}, true, true))
		for _, bad := range []string{"-preset", "-cpu-used"} {
			if slices.Contains(a, bad) {
				t.Errorf("%s must ignore speed, found %s", enc, bad)
			}
		}
	}
}

func TestBuildArgsVideoPresetNeedsVideoEncoder(t *testing.T) {
	s := spec(t, "video-vp9", presets.Candidate{Audio: "libopus"}, jobs.Options{}, true, true)
	if _, err := ffmpeg.BuildArgs(s); err == nil {
		t.Error("video preset without a video encoder must be rejected")
	}
}

func TestBuildArgsFileSizeCap(t *testing.T) {
	s := spec(t, "audio-flac", presets.Candidate{Audio: "flac"}, jobs.Options{}, false, true)
	if a := build(t, s); slices.Contains(a, "-fs") {
		t.Errorf("no cap configured, -fs must be absent: %v", a)
	}
	s.MaxOutputBytes = 12345
	a := build(t, s)
	if !hasSeq(a, "-fs", "12345") {
		t.Fatalf("missing -fs: %v", a)
	}
	// -fs is an output option: it must come after -i and before the output path.
	if slices.Index(a, "-fs") < slices.Index(a, "-i") || slices.Index(a, "-fs") > len(a)-2 {
		t.Errorf("-fs misplaced: %v", a)
	}
}

// Video presets map "0:V:0" (capital V excludes attached pictures such as cover art), never "0:v:0".
// A container whose cover picture comes BEFORE the real video cannot be produced with the available
// muxers (mp4 sorts attached pictures last, matroska drops the disposition), so the argument itself is
// asserted, and a real cover-art MP4 is converted end to end to prove the output is the real video.
func TestVideoMapExcludesAttachedPictures(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	cover := filepath.Join(dir, "cover.png")
	run := func(args ...string) {
		t.Helper()
		if b, err := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, b)
		}
	}
	run("-f", "lavfi", "-i", "color=red:size=64x64", "-frames:v", "1", cover)
	in := filepath.Join(dir, "in.mp4")
	run("-i", cover, "-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10", "-f", "lavfi", "-i", "sine=duration=1",
		"-map", "0:v", "-map", "1:v", "-map", "2:a", "-c:v:1", "mpeg4", "-c:v:0", "png", "-c:a", "aac", "-disposition:v:0", "attached_pic", in)

	a := build(t, spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, jobs.Options{}, true, true))
	if !hasSeq(a, "-map", "0:V:0") || slices.Contains(a, "0:v:0") {
		t.Fatalf("video map must use capital V: %v", a)
	}
	pr, _, err := ffmpeg.Probe(context.Background(), "ffprobe", in, dir)
	if err != nil || !pr.HasVideo() || !pr.HasAudio() {
		t.Fatalf("probe: %+v %v", pr, err)
	}
	s := spec(t, "video-vp9", presets.Candidate{Video: "libvpx-vp9", Audio: "libopus"}, jobs.Options{}, true, true)
	s.Input, s.Output = in, filepath.Join(dir, "out.webm")
	if tail, err := ffmpeg.Run(context.Background(), "ffmpeg", build(t, s), dir, 1, nil); err != nil {
		t.Fatalf("convert: %v\n%s", err, tail)
	}
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v", "-show_entries", "stream=width,height", "-of", "csv=p=0", s.Output).Output()
	if err != nil || strings.TrimSpace(string(out)) != "320,240" {
		t.Fatalf("output video is not the real video stream: %q %v", out, err)
	}
}
