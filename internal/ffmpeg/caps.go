package ffmpeg

import (
	"context"
	"os/exec"
	"slices"
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

// parseKinds maps encoder name to its type letter ('V', 'A', ...).
func parseKinds(output string) map[string]byte {
	kinds := map[string]byte{}
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
			kinds[f[1]] = f[0][0]
		}
	}
	return kinds
}

// DetectEncoders lists the preset-relevant encoders this FFmpeg can really use under the
// given flavor. Every candidate (after the fail-closed flavor filter) must pass a short
// trial encode; failures and timeouts drop the encoder.
func DetectEncoders(ctx context.Context, ffmpegBin, flavor, vaapiDevice string) ([]string, error) {
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(lctx, ffmpegBin, "-hide_banner", "-encoders")
	cmd.Env = minimalEnv()
	cmd.WaitDelay = 2 * time.Second
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	kinds := parseKinds(string(b))
	var out []string
	for _, e := range ParseEncoders(string(b)) {
		if !slices.Contains(presets.AllEncoders(), e) || !presets.FlavorAllows(flavor, e) {
			continue
		}
		if trialEncode(ctx, ffmpegBin, e, kinds[e], vaapiDevice) {
			out = append(out, e)
		}
	}
	return out, nil
}

func trialEncode(ctx context.Context, bin, enc string, kind byte, vaapiDevice string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}
	if enc == "av1_vaapi" {
		args = append(args, "-vaapi_device", vaapiDevice)
	}
	if kind == 'A' {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=0.2", "-t", "0.2", "-c:a", enc)
	} else {
		args = append(args, "-f", "lavfi", "-i", "testsrc=duration=1:size=256x256:rate=10", "-frames:v", "2")
		if enc == "av1_vaapi" {
			args = append(args, "-vf", "format=nv12,hwupload")
		} else if !hardwareEncoders[enc] {
			args = append(args, "-pix_fmt", "yuv420p")
		}
		args = append(args, "-c:v", enc)
	}
	args = append(args, "-f", "null", "-")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = minimalEnv()
	cmd.WaitDelay = 2 * time.Second
	return cmd.Run() == nil
}
