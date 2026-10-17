package e2e_test

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/rand"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	msgUnreadable = "file is not a readable media file"
	msgFormat     = "unsupported container format"
)

// checkFailedClean asserts a job failed with a message that is sanitized: not empty, free of any
// filesystem path of this stack, of the given secrets, and not path-shaped at all.
func (s *stack) checkFailedClean(name string, v map[string]any, secrets ...string) string {
	s.t.Helper()
	if v["status"] != "failed" {
		s.t.Errorf("%s: want failed, got %v", name, v)
		return ""
	}
	msg, _ := v["error"].(string)
	if msg == "" {
		s.t.Errorf("%s: empty error", name)
	}
	if pathLike.MatchString(msg) {
		s.t.Errorf("%s: error looks like a path: %q", name, msg)
	}
	b, _ := json.Marshal(v)
	s.assertNoLeak(name+" API JSON", string(b), secrets...)
	return msg
}

// Review Focus 1: garbage and mislabelled files fail with a clean message, the input is removed,
// no output remains, and neither API JSON, audit log nor server log mentions a path.
func TestGarbageInputsFailCleanly(t *testing.T) {
	s := start(t, opts{})
	rnd := make([]byte, 64<<10)
	_, _ = rand.New(rand.NewSource(7)).Read(rnd)

	dir := t.TempDir()
	mp4 := filepath.Join(dir, "v.mp4")
	ff(t, "-f", "lavfi", "-i", "testsrc=d=2:s=320x240:r=10", "-f", "lavfi", "-i", "sine=d=2",
		"-c:v", "mpeg4", "-c:a", "aac", mp4) // moov atom at the end: a truncated copy is unreadable
	full := readFile(t, mp4)
	truncated := full[:len(full)*6/10]
	ebml := append([]byte{0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x86, 0x81, 0x01}, rnd[:200]...)

	failing := []struct {
		name    string
		content []byte
		exact   string // required message; empty accepts any clean message
	}{
		{"x.mp4", []byte("this is just text, not a video"), msgUnreadable},
		{"x.mp3", []byte("ID3 this is not an mp3 either"), msgUnreadable},
		{"x.mkv", []byte("definitely not matroska"), msgUnreadable},
		{"random.mp4", rnd, msgUnreadable},
		{"zeros.mp4", make([]byte, 4096), msgUnreadable},
		{"onebyte.mp4", []byte{0x42}, msgUnreadable},
		{"ebml-header.mkv", ebml, ""},
		{"truncated.mp4", truncated, ""},
		{"html.mp4", []byte("<html><script>alert(1)</script></html>"), msgUnreadable},
	}
	for _, tc := range failing {
		id := s.mustUpload("audio-wav", tc.name, tc.content)
		v := s.wait(id)
		msg := s.checkFailedClean(tc.name, v)
		if tc.exact != "" && msg != tc.exact {
			t.Errorf("%s: error %q, want %q", tc.name, msg, tc.exact)
		}
		if v["encoder_used"] != "" {
			t.Errorf("%s: an encoder was started for garbage: %v", tc.name, v["encoder_used"])
		}
	}

	// An empty upload never becomes a job: refused at the API, nothing stored.
	code, v := s.upload("audio-wav", "x.mp4", nil)
	if code != http.StatusBadRequest {
		t.Errorf("empty upload: %d %v", code, v)
	}

	// A valid file with the wrong extension is judged by content, not by name.
	id := s.mustUpload("audio-flac", "x.mp4", clip(t, "audio"))
	if v := s.wait(id); v["status"] != "done" {
		t.Errorf("valid wav named x.mp4: %v", v)
	}

	s.waitDirEmpty("in")
	outs := s.names("out")
	if len(outs) != 1 || !strings.HasPrefix(outs[0], id+".") {
		t.Errorf("out/ must only hold the one good output, got %v", outs)
	}
	if n := s.jobCount(); n != len(failing)+1 {
		t.Errorf("job rows = %d, want %d", n, len(failing)+1)
	}
	logs := s.logs.String()
	s.assertNoLeak("audit and server logs", logs)
	if c := strings.Count(logs, `"event":"job_failed"`); c != len(failing) {
		t.Errorf("audit job_failed records = %d, want %d", c, len(failing))
	}
}

// loopback records every connection attempt (TCP) and datagram (UDP) on 127.0.0.1.
type loopback struct {
	tcp   net.Listener
	udp   net.PacketConn
	hits  atomic.Int64
	wg    sync.WaitGroup
	TCP   int
	UDP   int
	close sync.Once
}

func newLoopback(t *testing.T) *loopback {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	u, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		_ = l.Close()
		t.Skipf("no loopback udp: %v", err)
	}
	lb := &loopback{tcp: l, udp: u, TCP: l.Addr().(*net.TCPAddr).Port, UDP: u.LocalAddr().(*net.UDPAddr).Port}
	lb.wg.Add(2)
	go func() {
		defer lb.wg.Done()
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			lb.hits.Add(1)
			_ = c.Close()
		}
	}()
	go func() {
		defer lb.wg.Done()
		buf := make([]byte, 2048)
		for {
			if _, _, err := u.ReadFrom(buf); err != nil {
				return
			}
			lb.hits.Add(1)
		}
	}()
	t.Cleanup(lb.stop)
	return lb
}

func (lb *loopback) stop() {
	lb.close.Do(func() { _ = lb.tcp.Close(); _ = lb.udp.Close(); lb.wg.Wait() })
}

const canarySecret = "CANARY-SECRET-5f3a9c1e7b"

// makeCanary writes a valid WAV whose metadata and bytes contain the secret.
func makeCanary(t *testing.T, path string) {
	t.Helper()
	ff(t, "-f", "lavfi", "-i", "sine=d=1", "-metadata", "title="+canarySecret, "-metadata", "comment="+canarySecret, "-c:a", "pcm_s16le", path)
}

// Review Focus 2: playlist, concat, SDP/RTSP and pattern inputs are rejected, nothing is fetched
// from the network, no secondary file is read into any result, and nothing leaks into JSON/logs.
func TestPlaylistAndConcatInputsRejected(t *testing.T) {
	s := start(t, opts{})
	lb := newLoopback(t)
	root := filepath.Dir(s.dir)
	canaryAbs := filepath.Join(root, "canary-abs.wav")
	canaryRel := filepath.Join(s.dir, "in", "canary.wav") // same directory as the uploaded inputs
	makeCanary(t, canaryAbs)
	makeCanary(t, canaryRel)

	// Positive control: the listener sees a plain connection, so a fetch by FFmpeg would be visible.
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", lb.TCP)); err == nil {
		_ = c.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for lb.hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if lb.hits.Load() != 1 {
		t.Fatalf("loopback recorder does not work (hits=%d)", lb.hits.Load())
	}
	lb.hits.Store(0)

	// Second positive control: the same kind of playlist, probed by the host ffprobe with the
	// network protocols enabled, does reach the recorder. This proves the zero-hit result below
	// comes from the service's protocol restrictions and not from a blind listener.
	if out, _ := exec.Command("ffprobe", "-protocols", "-hide_banner").CombinedOutput(); strings.Contains(string(out), "http") {
		ctl := filepath.Join(t.TempDir(), "ctl.m3u8")
		_ = os.WriteFile(ctl, []byte(fmt.Sprintf("#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXTINF:1,\nhttp://127.0.0.1:%d/seg.ts\n#EXT-X-ENDLIST\n", lb.TCP)), 0o600)
		cmd := exec.Command("ffprobe", "-v", "quiet", "-protocol_whitelist", "file,http,tcp,hls", "-i", ctl)
		cmd.WaitDelay = 5 * time.Second
		_ = cmd.Run()
		if lb.hits.Load() == 0 {
			t.Fatal("control: an unrestricted ffprobe did not hit the loopback recorder")
		}
		lb.hits.Store(0)
	}

	httpURL := fmt.Sprintf("http://127.0.0.1:%d", lb.TCP)
	hls := func(body string) string {
		return "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n" + body + "#EXT-X-ENDLIST\n"
	}
	concat := func(lines ...string) string { return "ffconcat version 1.0\n" + strings.Join(lines, "\n") + "\n" }
	png := filepath.Join(t.TempDir(), "i.png")
	jpg := filepath.Join(t.TempDir(), "i.jpg")
	gif := filepath.Join(t.TempDir(), "i.gif")
	ff(t, "-f", "lavfi", "-i", "testsrc=d=1:s=64x64:r=1", "-frames:v", "1", png)
	ff(t, "-f", "lavfi", "-i", "testsrc=d=1:s=64x64:r=1", "-frames:v", "1", jpg)
	ff(t, "-f", "lavfi", "-i", "testsrc=d=1:s=64x64:r=2", gif)

	cases := []struct {
		name, content string
		exact         string
	}{
		{"concat-abs-canary.mp4", concat("file '"+canaryAbs+"'", "file '/etc/passwd'"), ""},
		{"concat-rel-canary.mp4", concat("file 'canary.wav'"), ""},
		{"concat-dotdot.mp4", concat("file '../canary-abs.wav'"), ""},
		{"concat-http.mp4", concat("file '" + httpURL + "/a.wav'"), ""},
		{"concat-tcp.txt", concat(fmt.Sprintf("file 'tcp://127.0.0.1:%d'", lb.TCP)), ""},
		{"concat-udp.txt", concat(fmt.Sprintf("file 'udp://127.0.0.1:%d'", lb.UDP)), ""},
		{"concat-file-scheme.txt", concat("file 'file:/etc/passwd'", "file 'file:"+canaryAbs+"'"), ""},
		{"concat-pipe.txt", concat("file 'pipe:0'", "file 'pipe:1'"), ""},
		{"concat-subfile.txt", concat("file 'subfile,,start,0,end,4096,,:" + canaryAbs + "'"), ""},
		{"concat-data-uri.txt", concat("file 'data:audio/wav;base64,UklGRiQAAABXQVZFZm10IBAAAAABAAEA'"), ""},
		{"concat-crypto.txt", concat("file 'crypto:" + canaryAbs + "'"), ""},
		{"concat-option-whitelist.txt", concat("option protocol_whitelist file,http,tcp,udp,crypto,data,pipe", "file '"+httpURL+"/x'"), ""},
		{"concat-stream-directives.txt", concat("file 'canary.wav'", "duration 5", "inpoint 0", "outpoint 1", "stream", "exact_stream_id 0"), ""},
		{"concat-no-header.txt", "file '/etc/passwd'\nfile '" + canaryAbs + "'\n", msgUnreadable},
		{"concat-passwd-only.txt", concat("file '/etc/passwd'"), ""},
		{"hls-http.mp4", hls("#EXTINF:1,\n" + httpURL + "/seg0.ts\n"), ""},
		{"hls-abs-file.mp4", hls("#EXTINF:1,\n" + canaryAbs + "\n"), ""},
		{"hls-passwd.m3u8", hls("#EXTINF:1,\n/etc/passwd\n"), ""},
		{"hls-file-url.m3u8", hls("#EXTINF:1,\nfile:///etc/passwd\nfile://" + canaryAbs + "\n"), ""},
		{"hls-key.m3u8", hls("#EXT-X-KEY:METHOD=AES-128,URI=\"" + httpURL + "/key\"\n#EXTINF:1,\nseg.ts\n"), ""},
		{"hls-map.m3u8", hls("#EXT-X-MAP:URI=\"" + httpURL + "/init.mp4\"\n#EXTINF:1,\nseg.ts\n"), ""},
		{"hls-master.m3u8", "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1000\n" + httpURL + "/v.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2000\n/etc/passwd\n", ""},
		{"hls-media-audio.m3u8", "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"" + httpURL + "/a.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=1,AUDIO=\"a\"\nv.m3u8\n", ""},
		{"plain-m3u.mp3", "#EXTM3U\n#EXTINF:1,x\n" + httpURL + "/a.mp3\n/etc/passwd\n", ""},
		{"sdp.mp4", fmt.Sprintf("v=0\no=- 0 0 IN IP4 127.0.0.1\ns=x\nc=IN IP4 127.0.0.1\nt=0 0\nm=audio %d RTP/AVP 0\na=rtpmap:0 PCMU/8000\n", lb.UDP), ""},
		{"sdp-tcp.sdp", fmt.Sprintf("v=0\no=- 0 0 IN IP4 127.0.0.1\ns=x\nc=IN IP4 127.0.0.1\nt=0 0\nm=audio %d TCP/RTP/AVP 0\na=rtpmap:0 PCMU/8000\n", lb.TCP), ""},
		{"rtsp.txt", fmt.Sprintf("rtsp://127.0.0.1:%d/stream\n", lb.TCP), msgUnreadable},
		{"rtsp-in-m3u.m3u8", fmt.Sprintf("#EXTM3U\nrtsp://127.0.0.1:%d/stream\n", lb.TCP), ""},
		{"image2-png-%03d.mp4", string(readFile(t, png)), ""},
		{"image2-glob-*.jpg", string(readFile(t, jpg)), ""},
		{"gif-%d.gif", string(readFile(t, gif)), ""},
		{"movie-source.txt", "movie=" + canaryAbs + "[a];amovie=/etc/passwd\n", msgUnreadable},
	}
	var ids []string
	for _, tc := range cases {
		id := s.mustUpload("audio-wav", tc.name, []byte(tc.content))
		v := s.wait(id)
		msg := s.checkFailedClean(tc.name, v, canarySecret, "canary-abs", "canary.wav")
		t.Logf("%-32s -> %s", tc.name, msg)
		if tc.exact != "" && msg != tc.exact {
			t.Errorf("%s: error %q, want %q", tc.name, msg, tc.exact)
		}
		if msg != msgUnreadable && msg != msgFormat {
			t.Errorf("%s: unexpected failure reason %q (must be a probe-stage rejection)", tc.name, msg)
		}
		if v["encoder_used"] != "" {
			t.Errorf("%s: job went past the probe stage: %v", tc.name, v)
		}
		if p, ok := v["probe"]; ok && p != nil {
			b, _ := json.Marshal(p)
			if strings.Contains(string(b), canarySecret) {
				t.Errorf("%s: probe JSON contains the canary", tc.name)
			}
		}
		ids = append(ids, id)
	}

	s.waitDirOnly("in", "canary.wav")
	s.waitDirEmpty("out") // not a single byte was produced for any rejected input

	// Glob and pattern lookalike file names with valid media are converted from their own bytes only.
	for _, name := range []string{"*.wav", "%d.wav", "img%03d.wav", "{a,b}.wav", "[0-9].wav"} {
		id := s.mustUpload("audio-wav", name, clip(t, "audio"))
		if v := s.wait(id); v["status"] != "done" {
			t.Errorf("%s: %v", name, v)
		}
	}

	if n := lb.hits.Load(); n != 0 {
		t.Errorf("FFmpeg/ffprobe contacted the network %d time(s)", n)
	}
	s.waitDirOnly("in", "canary.wav")
	if n := s.names("out"); len(n) != 5 {
		t.Errorf("out/ must hold exactly the five valid conversions, got %v", n)
	}
	if n, _ := os.ReadDir(s.workers[0].scratch); len(n) != 0 {
		t.Errorf("worker scratch dir not empty: %v", n)
	}
	s.assertNoLeak("audit and server logs", s.logs.String(), canarySecret, "canary-abs")
	_ = ids
}

// waitDirOnly is waitDirEmpty for a directory that legitimately holds the listed names.
func (s *stack) waitDirOnly(sub string, keep ...string) {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var extra []string
		for _, n := range s.names(sub) {
			if !contains(keep, n) {
				extra = append(extra, n)
			}
		}
		if len(extra) == 0 {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("data/%s has unexpected files: %v", sub, extra)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The canary is the only file besides the uploads; assert it was left alone and nothing was written
// anywhere that would indicate a secondary read being materialised.
func TestPlaylistInputsLeaveNoTraces(t *testing.T) {
	s := start(t, opts{})
	canary := filepath.Join(s.dir, "in", "canary.wav")
	makeCanary(t, canary)
	before := readFile(t, canary)
	id := s.mustUpload("audio-flac", "list.txt", []byte("ffconcat version 1.0\nfile 'canary.wav'\n"))
	v := s.wait(id)
	s.checkFailedClean("concat", v, canarySecret)
	s.waitDirOnly("in", "canary.wav")
	s.waitDirEmpty("out")
	if string(readFile(t, canary)) != string(before) {
		t.Error("canary file was modified")
	}
	if n, _ := os.ReadDir(s.workers[0].scratch); len(n) != 0 {
		t.Errorf("worker scratch dir not empty: %v", n)
	}
	s.assertNoLeak("logs", s.logs.String(), canarySecret)
}

// Review Focus 3: hostile client file names are only ever display metadata. The data directory
// holds server-named files only, nothing escapes it, the stored/returned name is sanitized, the
// Content-Disposition header on download is safe, and the converted file downloads correctly.
func TestHostileFilenamesEndToEnd(t *testing.T) {
	s := start(t, opts{})
	escapeTarget := "/tmp/cf-e2e-pwned.wav"
	_, preexisting := os.Stat(escapeTarget) // never delete or judge a file we did not create

	names := []struct {
		name     string
		mayRefus bool // raw NUL / CRLF may legitimately be refused as a malformed request
	}{
		{"../../etc/passwd", false}, {"../../../../tmp/cf-e2e-pwned.wav", false}, {"/etc/passwd", false},
		{"/abs/path/to/x.wav", false}, {`..\..\windows\system32\x.wav`, false}, {`C:\Windows\x.wav`, false},
		{"a\x00b.wav", true}, {"a\r\nX-Evil: 1\r\n.wav", true}, {"a\nb.wav", true},
		{`q"uote";x.wav`, false}, {"back\\slash.wav", false}, {"\u202egpj.wav", false}, {"zero\u200bwidth\ufeff.wav", false},
		{strings.Repeat("A", 5000) + ".wav", false}, {strings.Repeat("é", 400) + ".wav", false},
		{"-rf.wav", false}, {"-rf", false}, {"--help.wav", false}, {"con.mp4", false}, {"nul", false}, {"aux.wav", false},
		{".", false}, {"..", false}, {" ", false}, {"", false}, {"~root/x.wav", false}, {"$(touch pwned).wav", false},
		{"`id`.wav", false}, {"x;rm -rf.wav", false}, {"日本語.wav", false}, {"a%00b%2e%2e%2f.wav", false},
		{"file:/etc/passwd.wav", false}, {"*.wav", false}, {"x.wav\u2028y.wav", false},
	}
	in := clip(t, "audio")
	want := 0
	done := map[string]map[string]any{}
	for _, tc := range names {
		code, v := s.upload("audio-flac", tc.name, in)
		switch {
		case code == http.StatusAccepted:
			want++
			id := v["id"].(string)
			done[tc.name] = s.wait(id)
			if done[tc.name]["status"] != "done" {
				t.Errorf("%q: %v", tc.name, done[tc.name])
			}
		case tc.mayRefus && code == http.StatusBadRequest:
			t.Logf("%q refused as malformed (400)", tc.name)
		default:
			t.Errorf("%q: unexpected status %d %v", tc.name, code, v)
		}
	}
	if want < len(names)-3 {
		t.Errorf("only %d of %d uploads accepted", want, len(names))
	}

	// File system: only server-named files, nothing outside data/.
	if ents, _ := os.ReadDir(filepath.Dir(s.dir)); len(ents) != 1 || ents[0].Name() != "data" {
		t.Errorf("stack root has extra entries: %v", ents)
	}
	if ents, _ := os.ReadDir(s.dir); len(ents) != 2 {
		t.Errorf("data dir has extra entries: %v", ents)
	}
	if _, err := os.Stat(escapeTarget); err == nil && preexisting != nil {
		t.Errorf("%s was created by a traversal file name", escapeTarget)
	}
	s.waitDirEmpty("in")
	outRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.1\.flac$`)
	outs := s.names("out")
	if len(outs) != want {
		t.Errorf("out/ has %d files, want %d: %v", len(outs), want, outs)
	}
	for _, n := range outs {
		if !outRe.MatchString(n) {
			t.Errorf("out/ entry %q is not server-named", n)
		}
	}

	dispRe := regexp.MustCompile("^attachment; filename=\"[ -~]*\"; filename\\*=UTF-8''[A-Za-z0-9!#$&+\\-.^_`|~%]*$")
	for name, v := range done {
		fn, _ := v["filename"].(string)
		bad := strings.ContainsAny(fn, "/\\\"\x00\r\n") || !utf8.ValidString(fn) || len(fn) > 200 || fn == ""
		for _, r := range fn {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
				bad = true
			}
		}
		if bad {
			t.Errorf("%q: API returns unsafe display name %q", name, fn)
		}
		r, path := s.download(v)
		if r.Code != 200 {
			t.Errorf("%q: download %d", name, r.Code)
			continue
		}
		cd := r.Head.Get("Content-Disposition")
		if !dispRe.MatchString(cd) {
			t.Errorf("%q: unsafe Content-Disposition %q", name, cd)
		}
		if len(r.Head.Values("Content-Disposition")) != 1 || r.Head.Get("X-Evil") != "" {
			t.Errorf("%q: header injection: %v", name, r.Head)
		}
		_, params, err := mime.ParseMediaType(cd)
		if err != nil {
			t.Errorf("%q: Content-Disposition does not parse: %v", name, err)
		} else {
			got := params["filename"]
			stem := strings.TrimSuffix(got, ".flac")
			if !strings.HasSuffix(got, ".flac") || len(got) > 200 || !strings.HasPrefix(strings.TrimSuffix(fn, filepath.Ext(fn)), stem) {
				t.Errorf("%q: download name %q does not derive from display name %q", name, got, fn)
			}
		}
		if r.Head.Get("Content-Type") != "audio/flac" || r.Head.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%q: headers %v", name, r.Head)
		}
		if p := probe(t, path); !strings.Contains(p.Format.FormatName, "flac") {
			t.Errorf("%q: download is %s", name, p.Format.FormatName)
		}
	}
}

// Review Focus 5: stream-kind mismatches fail with the exact messages and cover art is not video.
func TestStreamKindMismatches(t *testing.T) {
	s := start(t, opts{})
	dir := t.TempDir()
	cover := filepath.Join(dir, "cover.png")
	ff(t, "-f", "lavfi", "-i", "color=c=red:s=64x64:d=1", "-frames:v", "1", cover)
	mp3 := filepath.Join(dir, "a.mp3")
	ff(t, "-f", "lavfi", "-i", "sine=duration=1", "-i", cover, "-map", "0:a", "-map", "1:v",
		"-c:a", "libmp3lame", "-c:v", "png", "-disposition:v", "attached_pic", "-id3v2_version", "3", mp3)
	if p := probe(t, mp3); p.count("video") != 1 {
		t.Fatalf("test input has no attached picture: %+v", p.Streams)
	}
	mp3b := readFile(t, mp3)

	failing := []struct {
		name, preset string
		in           []byte
		want         string
	}{
		{"audio-only.wav", "video-vp9", clip(t, "audio"), "input has no video stream"},
		{"audio-only.wav", "video-av1", clip(t, "audio"), "input has no video stream"},
		{"cover.mp3", "video-vp9", mp3b, "input has no video stream"},
		{"video-only.mkv", "audio-flac", clip(t, "video"), "input has no audio stream"},
		{"video-only.mkv", "audio-opus", clip(t, "video"), "input has no audio stream"},
	}
	for _, tc := range failing {
		id := s.mustUpload(tc.preset, tc.name, tc.in)
		v := s.wait(id)
		if v["status"] != "failed" || v["error"] != tc.want {
			t.Errorf("%s -> %s: want failed %q, got %v / %v", tc.name, tc.preset, tc.want, v["status"], v["error"])
		}
	}

	// Cover art converts to audio presets and does not leak a video stream into the output.
	for _, preset := range []string{"audio-flac", "audio-opus", "audio-mp3"} {
		id := s.mustUpload(preset, "cover.mp3", mp3b)
		v := s.wait(id)
		if v["status"] != "done" {
			t.Errorf("cover-art mp3 -> %s: %v", preset, v)
			continue
		}
		_, out := s.download(v)
		if p := probe(t, out); p.count("video") != 0 || p.count("audio") != 1 {
			t.Errorf("%s output streams: %+v", preset, p.Streams)
		}
	}
	// A normal audio+video file may go to an audio preset (video dropped) and to a video preset.
	id := s.mustUpload("audio-flac", "av.mkv", clip(t, "av"))
	if v := s.wait(id); v["status"] != "done" {
		t.Errorf("av -> audio-flac: %v", v)
	} else if _, out := s.download(v); probe(t, out).count("video") != 0 {
		t.Error("audio preset output kept a video stream")
	}
}

// ivf builds a minimal IVF file whose header declares dimensions and a frame count; the single
// frame is junk, so no decoder ever produces output.
func ivf(w, h uint16, tbDen, tbNum, frames uint32) []byte {
	b := make([]byte, 32)
	copy(b, "DKIF")
	binary.LittleEndian.PutUint16(b[6:], 32)
	copy(b[8:], "VP90")
	binary.LittleEndian.PutUint16(b[12:], w)
	binary.LittleEndian.PutUint16(b[14:], h)
	binary.LittleEndian.PutUint32(b[16:], tbDen)
	binary.LittleEndian.PutUint32(b[20:], tbNum)
	binary.LittleEndian.PutUint32(b[24:], frames)
	fr := make([]byte, 12+20)
	binary.LittleEndian.PutUint32(fr, 20)
	return append(b, fr...)
}

// wavHeader builds an 8-bit mono WAV with the given sample rate and n data bytes.
func wavFile(rate uint32, n int) []byte {
	b := make([]byte, 44+n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], rate)
	binary.LittleEndian.PutUint32(b[28:], rate)
	binary.LittleEndian.PutUint16(b[32:], 1)
	binary.LittleEndian.PutUint16(b[34:], 8)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(n))
	for i := 44; i < len(b); i++ {
		b[i] = 0x80
	}
	return b
}

// Review Focus 6: decompression-bomb-like media is refused by the limits before any encoder runs.
// Crafted cases: IVF headers declaring huge dimensions / 4.29e9 seconds, a 4 KB WAV at 1 Hz (4000 s),
// and FFmpeg-made tiny files with a 2 hour duration or 40 streams. Not buildable here: an MP4 whose
// mvhd/stsd lie (FFmpeg recomputes duration and takes dimensions from the bitstream).
func TestDecompressionBombsRefused(t *testing.T) {
	s := start(t, opts{})
	dir := t.TempDir()

	longAudio := filepath.Join(dir, "long.flac")
	ff(t, "-f", "lavfi", "-i", "anullsrc=r=8000:cl=mono:d=7200", "-c:a", "flac", longAudio)
	longVideo := filepath.Join(dir, "long.mkv")
	ff(t, "-f", "lavfi", "-i", "color=c=black:s=16x16:r=1:d=7200", "-c:v", "mpeg4", "-g", "100000", longVideo)
	manyArgs := []string{}
	maps := []string{}
	for i := 0; i < 40; i++ {
		manyArgs = append(manyArgs, "-f", "lavfi", "-t", "0.1", "-i", "sine=frequency=440")
		maps = append(maps, "-map", fmt.Sprintf("%d:a", i))
	}
	manyStreams := filepath.Join(dir, "many.mkv")
	ff(t, append(append(manyArgs, maps...), "-c:a", "pcm_s16le", manyStreams)...)

	cases := []struct {
		name, preset string
		content      []byte
		want         []string // acceptable messages
	}{
		{"huge-dims.ivf", "video-vp9", ivf(16000, 16000, 30, 1, 1), []string{"video dimensions exceed limit"}},
		{"invalid-dims.ivf", "video-vp9", ivf(65535, 65535, 30, 1, 1), []string{"video dimensions unknown", "video dimensions exceed limit"}},
		{"huge-duration.ivf", "video-vp9", ivf(64, 64, 1, 1, 0xFFFFFFFF), []string{"duration exceeds limit of 3600 seconds"}},
		{"tiny-wav-long.wav", "audio-flac", wavFile(1, 4000), []string{"duration exceeds limit of 3600 seconds"}},
		{"two-hours-audio.flac", "audio-wav", readFile(t, longAudio), []string{"duration exceeds limit of 3600 seconds"}},
		{"two-hours-video.mkv", "video-vp9", readFile(t, longVideo), []string{"duration exceeds limit of 3600 seconds"}},
		{"forty-streams.mkv", "audio-flac", readFile(t, manyStreams), []string{"too many streams (limit 16)"}},
	}
	if !testing.Short() {
		big := filepath.Join(dir, "big.mkv")
		ff(t, "-f", "lavfi", "-i", "color=c=black:s=9000x9000:r=1", "-frames:v", "1", "-c:v", "ffv1", big)
		cases = append(cases, struct {
			name, preset string
			content      []byte
			want         []string
		}{"real-81-megapixel.mkv", "video-vp9", readFile(t, big), []string{"video dimensions exceed limit"}})
	}
	for _, tc := range cases {
		id := s.mustUpload(tc.preset, tc.name, tc.content)
		v := s.wait(id)
		msg := s.checkFailedClean(tc.name, v)
		if !contains(tc.want, msg) {
			t.Errorf("%s: error %q, want one of %q", tc.name, msg, tc.want)
		}
		if v["encoder_used"] != "" {
			t.Errorf("%s: encoder started for a bomb", tc.name)
		}
	}
	s.waitDirEmpty("in")
	s.waitDirEmpty("out")
}
