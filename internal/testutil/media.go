// Package testutil generates tiny media files for tests using FFmpeg's native encoders.
package testutil

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func RequireTools(t *testing.T) {
	t.Helper()
	for _, b := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not installed", b)
		}
	}
}

// MakeClip writes a 1-second clip. kind: "av" (mkv, video+audio), "audio" (wav), "video" (mkv, no audio).
func MakeClip(t *testing.T, dir, name, kind string) string {
	t.Helper()
	RequireTools(t)
	out := filepath.Join(dir, name)
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	video := []string{"-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=10"}
	audio := []string{"-f", "lavfi", "-i", "sine=frequency=440:duration=1"}
	switch kind {
	case "av":
		args = append(append(append(args, video...), audio...), "-c:v", "mpeg4", "-c:a", "pcm_s16le", "-f", "matroska")
	case "audio":
		args = append(append(args, audio...), "-c:a", "pcm_s16le", "-f", "wav")
	case "video":
		args = append(append(args, video...), "-c:v", "mpeg4", "-f", "matroska")
	default:
		t.Fatalf("unknown clip kind %q", kind)
	}
	args = append(args, out)
	if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("making clip: %v\n%s", err, b)
	}
	return out
}
