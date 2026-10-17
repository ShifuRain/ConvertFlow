package jobs_test

import (
	"testing"

	"github.com/ShifuRain/convertflow/internal/jobs"
)

func ip(i int) *int { return &i }

func TestOptionsValidate(t *testing.T) {
	ok := []jobs.Options{
		{Preset: "audio-wav"},
		{Preset: "video-vp9", Speed: ip(0), CRF: ip(18), MaxHeight: ip(1080), AudioBitrateK: ip(128)},
		{Preset: "video-av1", Speed: ip(10), CRF: ip(45), MaxHeight: ip(240), AudioBitrateK: ip(320)},
	}
	for _, o := range ok {
		if err := o.Validate(); err != nil {
			t.Errorf("%+v should be valid: %v", o, err)
		}
	}
	bad := []jobs.Options{
		{},
		{Preset: "nope"},
		{Preset: "audio-wav", Speed: ip(-1)},
		{Preset: "audio-wav", Speed: ip(11)},
		{Preset: "video-vp9", CRF: ip(17)},
		{Preset: "video-vp9", CRF: ip(46)},
		{Preset: "video-vp9", MaxHeight: ip(1081)},
		{Preset: "audio-mp3", AudioBitrateK: ip(100)},
	}
	for _, o := range bad {
		if err := o.Validate(); err == nil {
			t.Errorf("%+v should be invalid", o)
		}
	}
}
