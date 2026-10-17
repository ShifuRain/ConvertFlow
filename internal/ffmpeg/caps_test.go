package ffmpeg_test

import (
	"context"
	"slices"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/testutil"
)

const encodersFixture = ` Encoders:
 V..... = Video
 ------
 V....D libsvtav1            SVT-AV1(Scalable Video Technology for AV1) encoder (codec av1)
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 V....D av1_nvenc            NVIDIA NVENC av1 encoder (codec av1)
 V....D libvpx-vp9           libvpx VP9 (codec vp9)
 A....D aac                  AAC (Advanced Audio Coding)
 A....D libopus              libopus Opus (codec opus)
 A....D somethingelse        not ours
`

func TestParseEncoders(t *testing.T) {
	got := ffmpeg.ParseEncoders(encodersFixture)
	for _, want := range []string{"libsvtav1", "libx264", "aac", "libopus", "libvpx-vp9", "av1_nvenc", "somethingelse"} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if slices.Contains(got, "Encoders:") || slices.Contains(got, "=") {
		t.Errorf("header leaked: %v", got)
	}
}

func TestDetectEncodersRespectsFlavor(t *testing.T) {
	testutil.RequireTools(t)
	lgpl, err := ffmpeg.DetectEncoders(context.Background(), "ffmpeg", "lgpl", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"libx264", "libx265", "aac"} {
		if slices.Contains(lgpl, banned) {
			t.Errorf("lgpl flavor must not advertise %s", banned)
		}
	}
	if !slices.Contains(lgpl, "pcm_s16le") {
		t.Errorf("pcm_s16le should always be available: %v", lgpl)
	}
	full, _ := ffmpeg.DetectEncoders(context.Background(), "ffmpeg", "full", "")
	if !slices.Contains(full, "aac") {
		t.Errorf("full flavor should advertise native aac: %v", full)
	}
}
