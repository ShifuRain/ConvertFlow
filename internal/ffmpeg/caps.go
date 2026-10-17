package ffmpeg

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/ShifuRain/convertflow/internal/presets"
)

var hardwareEncoders = map[string]bool{"av1_nvenc": true, "av1_qsv": true, "av1_vaapi": true}

// ParseEncoders extracts encoder names from `ffmpeg -encoders` output.
func ParseEncoders(output string) []string {
	var out []string
	started := false
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "------") {
			started = true
			continue
		}
		if !started {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 && len(f[0]) == 6 {
			out = append(out, f[1])
		}
	}
	return out
}

// DetectEncoders lists the preset-relevant encoders this FFmpeg can really use under the
// given flavor. Hardware encoders must also pass a short trial encode.
func DetectEncoders(ctx context.Context, ffmpegBin, flavor, vaapiDevice string) ([]string, error) {
	cmd := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-encoders")
	cmd.Env = minimalEnv()
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, e := range presets.AllEncoders() {
		wanted[e] = true
	}
	var out []string
	for _, e := range ParseEncoders(string(b)) {
		if !wanted[e] || !presets.FlavorAllows(flavor, e) {
			continue
		}
		if hardwareEncoders[e] && !hwTrial(ctx, ffmpegBin, e, vaapiDevice) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func hwTrial(ctx context.Context, bin, enc, vaapiDevice string) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if enc == "av1_vaapi" {
		args = append(args, "-vaapi_device", vaapiDevice)
	}
	args = append(args, "-f", "lavfi", "-i", "testsrc=duration=0.2:size=256x256:rate=5")
	if enc == "av1_vaapi" {
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	args = append(args, "-c:v", enc, "-f", "null", "-")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = minimalEnv()
	return cmd.Run() == nil
}
