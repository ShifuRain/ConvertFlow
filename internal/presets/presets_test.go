package presets_test

import (
	"testing"

	"github.com/ShifuRain/convertflow/internal/presets"
)

func set(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestResolvePrefersFirstAvailable(t *testing.T) {
	p, _ := presets.Get("video-av1")
	c, ok := p.Resolve(set("libsvtav1", "libvpx-vp9", "libopus"))
	if !ok || c.Video != "libsvtav1" {
		t.Fatalf("got %+v %v", c, ok)
	}
	c, ok = p.Resolve(set("av1_nvenc", "libsvtav1", "libopus"))
	if !ok || c.Video != "av1_nvenc" {
		t.Fatalf("hardware should win: %+v", c)
	}
}

func TestResolveFallsBackToVP9(t *testing.T) {
	p, _ := presets.Get("video-av1")
	c, ok := p.Resolve(set("libvpx-vp9", "libopus"))
	if !ok || c.Video != "libvpx-vp9" {
		t.Fatalf("got %+v %v", c, ok)
	}
}

func TestResolveNeedsAudioEncoder(t *testing.T) {
	p, _ := presets.Get("video-vp9")
	if _, ok := p.Resolve(set("libvpx-vp9")); ok {
		t.Fatal("must not resolve without audio encoder")
	}
}

func TestAvailableFilters(t *testing.T) {
	av := presets.Available(set("pcm_s16le"))
	if len(av) != 1 || av[0].ID != "audio-wav" {
		t.Fatalf("got %v", av)
	}
}

func TestFlavorPolicy(t *testing.T) {
	for _, e := range []string{"libx264", "libx265", "aac"} {
		if presets.FlavorAllows("lgpl", e) || !presets.FlavorAllows("full", e) {
			t.Errorf("%s flavor policy wrong", e)
		}
	}
	if !presets.FlavorAllows("lgpl", "libsvtav1") {
		t.Error("royalty-free encoders must be allowed in lgpl")
	}
}

func TestUniqueIDsAndEncoders(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range presets.All() {
		if seen[p.ID] {
			t.Fatalf("duplicate id %s", p.ID)
		}
		seen[p.ID] = true
		if p.Ext == "" || p.Container == "" || len(p.Candidates) == 0 {
			t.Fatalf("incomplete preset %+v", p)
		}
	}
	all := set(presets.AllEncoders()...)
	if !all["libsvtav1"] || !all["pcm_s16le"] || !all["aac"] {
		t.Fatalf("AllEncoders incomplete: %v", all)
	}
}

func TestFlavorAllowsFailsClosed(t *testing.T) {
	// Test that patent-encumbered and unknown encoders are denied in lgpl and unknown flavors
	deniedEncoders := []string{"h264_nvenc", "hevc_nvenc", "libx264", "libx265", "aac", "libfoo"}
	for _, e := range deniedEncoders {
		if presets.FlavorAllows("lgpl", e) {
			t.Errorf("%s should be denied in lgpl flavor", e)
		}
		if presets.FlavorAllows("LGPL", e) {
			t.Errorf("%s should be denied in unknown flavor LGPL", e)
		}
		if presets.FlavorAllows("", e) {
			t.Errorf("%s should be denied in empty flavor", e)
		}
	}
	// Full flavor allows all encoders
	for _, e := range deniedEncoders {
		if !presets.FlavorAllows("full", e) {
			t.Errorf("%s should be allowed in full flavor", e)
		}
	}
}

func TestAllEncodersClassified(t *testing.T) {
	// Every encoder in AllEncoders() must be either allowed under lgpl or in fullOnly
	lgplAllowed := set()
	for _, e := range presets.AllEncoders() {
		// Encoder is allowed in lgpl if FlavorAllows returns true
		if presets.FlavorAllows("lgpl", e) {
			lgplAllowed[e] = true
		}
	}
	// Check that all encoders are either allowed in lgpl or explicitly in fullOnly
	for _, p := range presets.All() {
		for _, c := range p.Candidates {
			for _, e := range []string{c.Video, c.Audio} {
				if e == "" {
					continue
				}
				if !presets.FlavorAllows("lgpl", e) && !presets.FlavorAllows("full", e) {
					t.Errorf("encoder %s is unclassified", e)
				}
			}
		}
	}
}
