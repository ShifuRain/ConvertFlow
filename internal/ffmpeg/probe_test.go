package ffmpeg_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ShifuRain/convertflow/internal/ffmpeg"
	"github.com/ShifuRain/convertflow/internal/testutil"
)

const goodJSON = `{"format":{"format_name":"matroska,webm","duration":"12.5"},
 "streams":[{"codec_type":"video","codec_name":"vp9","width":1920,"height":1080,"disposition":{"attached_pic":0}},
            {"codec_type":"audio","codec_name":"opus"}]}`

var lim = ffmpeg.Limits{MaxDurationSec: 3600, MaxPixels: 8192 * 4320, MaxStreams: 16}

func TestParseProbeAndFlags(t *testing.T) {
	p, err := ffmpeg.ParseProbe([]byte(goodJSON))
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasVideo() || !p.HasAudio() || p.DurationSeconds() != 12.5 {
		t.Fatalf("bad parse %+v", p)
	}
	if err := p.Validate(lim); err != nil {
		t.Fatal(err)
	}
}

func TestCoverArtIsNotVideo(t *testing.T) {
	p, _ := ffmpeg.ParseProbe([]byte(`{"format":{"format_name":"mp3","duration":"3"},
	 "streams":[{"codec_type":"audio","codec_name":"mp3"},
	            {"codec_type":"video","codec_name":"mjpeg","width":500,"height":500,"disposition":{"attached_pic":1}}]}`))
	if p.HasVideo() {
		t.Fatal("attached picture must not count as video")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"concat":     `{"format":{"format_name":"concat","duration":"1"},"streams":[{"codec_type":"audio"}]}`,
		"hls":        `{"format":{"format_name":"hls,applehttp","duration":"1"},"streams":[{"codec_type":"audio"}]}`,
		"image2":     `{"format":{"format_name":"image2","duration":"1"},"streams":[{"codec_type":"video","width":1,"height":1}]}`,
		"no streams": `{"format":{"format_name":"wav","duration":"1"},"streams":[]}`,
		"too long":   `{"format":{"format_name":"wav","duration":"999999"},"streams":[{"codec_type":"audio"}]}`,
		"huge":       `{"format":{"format_name":"mov,mp4","duration":"1"},"streams":[{"codec_type":"video","width":60000,"height":60000}]}`,
	}
	for name, js := range cases {
		p, err := ffmpeg.ParseProbe([]byte(js))
		if err != nil {
			t.Errorf("%s: parse: %v", name, err)
			continue
		}
		if err := p.Validate(lim); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	many := `{"format":{"format_name":"wav","duration":"1"},"streams":[` + strings.Repeat(`{"codec_type":"audio"},`, 20) + `{"codec_type":"audio"}]}`
	p, _ := ffmpeg.ParseProbe([]byte(many))
	if p.Validate(lim) == nil {
		t.Error("too many streams must be rejected")
	}
}

func TestProbeRealClip(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	clip := testutil.MakeClip(t, dir, "a.mkv", "av")
	p, _, err := ffmpeg.Probe(context.Background(), "ffprobe", clip, dir)
	if err != nil || !p.HasVideo() || !p.HasAudio() {
		t.Fatalf("probe: %+v %v", p, err)
	}
	if err := p.Validate(lim); err != nil {
		t.Fatal(err)
	}
}

func TestProbeRejectsGarbage(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	for name, body := range map[string]string{"x.mp4": "this is not media at all", "empty.mp4": ""} {
		path := filepath.Join(dir, name)
		_ = os.WriteFile(path, []byte(body), 0o600)
		_, _, err := ffmpeg.Probe(context.Background(), "ffprobe", path, dir)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if strings.Contains(err.Error(), dir) {
			t.Fatalf("%s: error leaks path: %v", name, err)
		}
	}
}

func TestProbeRejectsConcat(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "evil.mp4")
	_ = os.WriteFile(path, []byte("ffconcat version 1.0\nfile '/etc/passwd'\n"), 0o600)
	p, _, err := ffmpeg.Probe(context.Background(), "ffprobe", path, dir)
	if err == nil && p.Validate(lim) == nil {
		t.Fatal("ffconcat playlist must be rejected by probe or validation")
	}
}

func TestProbeRealCoverArtMP3(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "c.mp3")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "sine=d=1", "-f", "lavfi", "-i", "testsrc=d=1:s=64x64:r=1",
		"-frames:v", "1", "-map", "0", "-map", "1", "-c:a", "libmp3lame", "-c:v", "mjpeg", "-disposition:v", "attached_pic", path)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot build cover-art mp3 (no libmp3lame?): %v %s", err, b)
	}
	p, _, err := ffmpeg.Probe(context.Background(), "ffprobe", path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.HasVideo() || !p.HasAudio() {
		t.Fatalf("cover-art mp3 must be audio-only: %+v", p)
	}
	if err := p.Validate(lim); err != nil {
		t.Fatal(err)
	}
}

func TestProbeRejectsHLSPlaylistText(t *testing.T) {
	testutil.RequireTools(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "p.mp4")
	_ = os.WriteFile(path, []byte("#EXTM3U\n#EXTINF:1,\nhttp://127.0.0.1/x\n"), 0o600)
	if p, _, err := ffmpeg.Probe(context.Background(), "ffprobe", path, dir); err == nil && p.Validate(lim) == nil {
		t.Fatal("HLS playlist must be rejected")
	}
}
