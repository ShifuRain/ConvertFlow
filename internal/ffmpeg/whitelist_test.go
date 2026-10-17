package ffmpeg_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/jobs"
	"github.com/ShifuRain/convertflow/internal/presets"
	"github.com/ShifuRain/convertflow/internal/testutil"
)

func mk(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	out := filepath.Join(dir, name)
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	full = append(full, out)
	if b, err := exec.Command("ffmpeg", full...).CombinedOutput(); err != nil {
		t.Skipf("cannot build %s here: %v %s", name, err, b)
	}
	return out
}

// Every container kind the service accepts must pass FFmpeg's own demuxer whitelist, in ffprobe
// and in the transcoding invocation (grouped demuxers such as "mov,mp4,m4a,..." match by alias).
func TestFormatWhitelistAcceptsSupportedContainers(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	cover := mk(t, dir, "cover.png", "-f", "lavfi", "-i", "color=c=red:s=64x64:d=1", "-frames:v", "1")
	sine := []string{"-f", "lavfi", "-i", "sine=duration=1"}
	vid := []string{"-f", "lavfi", "-i", "testsrc=d=1:s=64x64:r=5"}
	files := []string{
		mk(t, dir, "a.wav", append(sine, "-c:a", "pcm_s16le")...),
		mk(t, dir, "a.flac", append(sine, "-c:a", "flac")...),
		mk(t, dir, "a.mp3", append(sine, "-c:a", "libmp3lame")...),
		mk(t, dir, "a.opus", append(sine, "-c:a", "libopus")...),
		mk(t, dir, "a.ogg", append(sine, "-c:a", "libvorbis")...),
		mk(t, dir, "a.m4a", append(sine, "-c:a", "aac")...),
		mk(t, dir, "v.webm", append(vid, "-c:v", "libvpx-vp9")...),
		mk(t, dir, "v.mkv", append(vid, "-c:v", "mpeg4")...),
		mk(t, dir, "v.mp4", append(vid, "-c:v", "mpeg4")...),
		mk(t, dir, "v.avi", append(vid, "-c:v", "mpeg4")...),
		mk(t, dir, "v.ts", append(vid, "-c:v", "mpeg4")...),
		mk(t, dir, "v.ivf", append(vid, "-c:v", "libvpx-vp9")...),
		mk(t, dir, "cover.mp3", append(append(sine, "-i", cover), "-map", "0:a", "-map", "1:v", "-c:a", "libmp3lame", "-c:v", "png", "-disposition:v", "attached_pic")...),
		mk(t, dir, "cover.m4a", append(append(sine, "-i", cover), "-map", "0:a", "-map", "1:v", "-c:a", "aac", "-c:v", "png", "-disposition:v", "attached_pic")...),
	}
	for _, f := range files {
		p, _, err := ffmpeg.Probe(context.Background(), "ffprobe", f, t.TempDir())
		if err != nil {
			t.Errorf("%s: probe refused by whitelist: %v", filepath.Base(f), err)
			continue
		}
		if err := p.Validate(lim); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
		out, err := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-format_whitelist", ffmpeg.FormatWhitelist(), "-i", f, "-t", "0.2", "-f", "null", "-").CombinedOutput()
		if err != nil {
			t.Errorf("%s: ffmpeg refused by whitelist: %v %s", filepath.Base(f), err, out)
		}
	}
}

// FFmpeg itself refuses the concat/HLS/SDP demuxers when they are not whitelisted, independent of
// the post-hoc check in Validate: Probe fails before any probe result exists.
func TestFormatWhitelistRejectsPlaylistDemuxersInFFmpeg(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	mk(t, dir, "a.wav", "-f", "lavfi", "-i", "sine=duration=1", "-c:a", "pcm_s16le")
	cases := map[string]string{
		"concat.txt": "ffconcat version 1.0\nfile 'a.wav'\n",
		"hls.m3u8":   "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\na.wav\n#EXT-X-ENDLIST\n",
		"sdp.sdp":    "v=0\no=- 0 0 IN IP4 127.0.0.1\ns=x\nc=IN IP4 127.0.0.1\nt=0 0\nm=audio 5004 RTP/AVP 0\na=rtpmap:0 PCMU/8000\n",
	}
	for name, content := range cases {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		// Without the whitelist FFmpeg would open the demuxer (control) ...
		ctl, _ := exec.Command("ffprobe", "-v", "error", "-protocol_whitelist", "file", "-i", p).CombinedOutput()
		if strings.Contains(string(ctl), "Format not on whitelist") {
			t.Fatalf("%s: control run unexpectedly whitelisted: %s", name, ctl)
		}
		// ... with it, FFmpeg refuses by itself.
		_, err := exec.Command("ffprobe", "-v", "error", "-protocol_whitelist", "file", "-format_whitelist", ffmpeg.FormatWhitelist(), "-i", p).CombinedOutput()
		if err == nil {
			t.Errorf("%s: ffprobe accepted a non-whitelisted demuxer", name)
		}
		if _, _, err := ffmpeg.Probe(context.Background(), "ffprobe", p, t.TempDir()); err == nil {
			t.Errorf("%s: Probe accepted it", name)
		}
	}
	// Explicitly: concat is rejected by FFmpeg even though validation never saw a result.
	out, _ := exec.Command("ffprobe", "-v", "error", "-format_whitelist", ffmpeg.FormatWhitelist(), "-i", filepath.Join(dir, "concat.txt")).CombinedOutput()
	if !strings.Contains(string(out), "Format not on whitelist") {
		t.Errorf("expected FFmpeg's own whitelist refusal for concat, got %q", out)
	}
}

func TestBuildArgsWhitelistsInputFormats(t *testing.T) {
	p, _ := presets.Get("audio-flac")
	a, err := ffmpeg.BuildArgs(ffmpeg.Spec{Input: "/d/in/x", Output: "/d/out/x.1.flac", Preset: p, Cand: p.Candidates[0],
		Opts: jobs.Options{Preset: "audio-flac"}, HasAudio: true})
	if err != nil {
		t.Fatal(err)
	}
	wl, in := -1, -1
	for i, v := range a {
		if v == "-format_whitelist" {
			wl = i
		}
		if v == "-i" {
			in = i
		}
	}
	if wl < 0 || in < 0 || wl > in || a[wl+1] != ffmpeg.FormatWhitelist() {
		t.Fatalf("whitelist must precede -i: %v", a)
	}
	for _, bad := range []string{"concat", "hls", "sdp", "rtsp", "image2", "lavfi", "tee"} {
		for _, n := range strings.Split(ffmpeg.FormatWhitelist(), ",") {
			if n == bad {
				t.Errorf("%s must not be whitelisted", bad)
			}
		}
	}
}
