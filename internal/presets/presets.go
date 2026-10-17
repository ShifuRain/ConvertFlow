// Package presets defines the transcode presets, their encoder fallback chains and flavor policy.
package presets

type Kind string

const (
	KindVideo Kind = "video"
	KindAudio Kind = "audio"
)

// Candidate is one encoder combination; Video is empty for audio presets.
type Candidate struct{ Video, Audio string }

type Preset struct {
	ID         string
	Label      string
	Kind       Kind
	Container  string // ffmpeg muxer name
	Ext        string // output file extension
	Candidates []Candidate
}

var all = []Preset{
	{ID: "video-av1", Label: "Video – AV1 + Opus (WebM)", Kind: KindVideo, Container: "webm", Ext: "webm", Candidates: []Candidate{
		{"av1_nvenc", "libopus"}, {"av1_qsv", "libopus"}, {"av1_vaapi", "libopus"},
		{"libsvtav1", "libopus"}, {"libvpx-vp9", "libopus"}}},
	{ID: "video-vp9", Label: "Video – VP9 + Opus (WebM)", Kind: KindVideo, Container: "webm", Ext: "webm", Candidates: []Candidate{
		{"libvpx-vp9", "libopus"}}},
	{ID: "video-h264", Label: "Video – H.264 + AAC (MP4)", Kind: KindVideo, Container: "mp4", Ext: "mp4", Candidates: []Candidate{
		{"libx264", "aac"}}},
	{ID: "video-h265", Label: "Video – H.265 + AAC (MP4)", Kind: KindVideo, Container: "mp4", Ext: "mp4", Candidates: []Candidate{
		{"libx265", "aac"}}},
	{ID: "audio-opus", Label: "Audio – Opus", Kind: KindAudio, Container: "ogg", Ext: "opus", Candidates: []Candidate{{"", "libopus"}}},
	{ID: "audio-vorbis", Label: "Audio – Vorbis (Ogg)", Kind: KindAudio, Container: "ogg", Ext: "ogg", Candidates: []Candidate{{"", "libvorbis"}}},
	{ID: "audio-flac", Label: "Audio – FLAC", Kind: KindAudio, Container: "flac", Ext: "flac", Candidates: []Candidate{{"", "flac"}}},
	{ID: "audio-mp3", Label: "Audio – MP3", Kind: KindAudio, Container: "mp3", Ext: "mp3", Candidates: []Candidate{{"", "libmp3lame"}}},
	{ID: "audio-wav", Label: "Audio – WAV (PCM)", Kind: KindAudio, Container: "wav", Ext: "wav", Candidates: []Candidate{{"", "pcm_s16le"}}},
	{ID: "audio-aac", Label: "Audio – AAC (M4A)", Kind: KindAudio, Container: "ipod", Ext: "m4a", Candidates: []Candidate{{"", "aac"}}},
}

// lgplSafe encoders are royalty-free and allowed in the LGPL flavor.
// This is an explicit allow-list (fail-closed) to protect against future patent-encumbered encoders.
var lgplSafe = map[string]bool{
	// AV1 encoders (all royalty-free)
	"av1_nvenc":  true,
	"av1_qsv":    true,
	"av1_vaapi":  true,
	"libsvtav1":  true,
	"libaom-av1": true,
	// VP9 encoders (royalty-free)
	"libvpx-vp9": true,
	"libvpx":     true,
	// Audio encoders (royalty-free)
	"libopus":    true,
	"libvorbis":  true,
	"flac":       true,
	"libmp3lame": true,
	"pcm_s16le":  true,
}

// fullOnly encoders are withheld in the royalty-free (lgpl) flavor for patent reasons.
// These are NOT in lgplSafe (the allow-list).
var fullOnly = map[string]bool{"libx264": true, "libx265": true, "aac": true}

func All() []Preset { return append([]Preset(nil), all...) }

func Get(id string) (Preset, bool) {
	for _, p := range all {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// Resolve returns the first candidate whose encoders are all in enc.
func (p Preset) Resolve(enc map[string]bool) (Candidate, bool) {
	for _, c := range p.Candidates {
		if (c.Video == "" || enc[c.Video]) && enc[c.Audio] {
			return c, true
		}
	}
	return Candidate{}, false
}

func Available(enc map[string]bool) []Preset {
	out := []Preset{}
	for _, p := range all {
		if _, ok := p.Resolve(enc); ok {
			out = append(out, p)
		}
	}
	return out
}

func AllEncoders() []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range all {
		for _, c := range p.Candidates {
			for _, e := range []string{c.Video, c.Audio} {
				if e != "" && !seen[e] {
					seen[e] = true
					out = append(out, e)
				}
			}
		}
	}
	return out
}

// FlavorAllows returns whether the given encoder is allowed in the specified flavor.
// For "full" flavor, all encoders are allowed.
// For "lgpl" or any unknown flavor, only encoders in the explicit allow-list (lgplSafe) are allowed (fail-closed).
func FlavorAllows(flavor, encoder string) bool {
	if flavor == "full" {
		return true
	}
	// For lgpl or any unknown flavor, use the allow-list (fail-closed)
	return lgplSafe[encoder]
}
