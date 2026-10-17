package jobs

import (
	"fmt"
	"slices"

	"github.com/ShifuRain/convertflow/internal/presets"
)

var (
	allowedHeights  = []int{240, 360, 480, 720, 1080, 1440, 2160}
	allowedBitrates = []int{64, 96, 128, 192, 256, 320}
)

// Validate enforces the allow-list; BuildArgs relies on it and must never see unvalidated options.
func (o Options) Validate() error {
	if _, ok := presets.Get(o.Preset); !ok {
		return fmt.Errorf("unknown preset %q", o.Preset)
	}
	if o.Speed != nil && (*o.Speed < 0 || *o.Speed > 10) {
		return fmt.Errorf("speed must be between 0 (best quality) and 10 (fastest)")
	}
	if o.CRF != nil && (*o.CRF < 18 || *o.CRF > 45) {
		return fmt.Errorf("crf must be between 18 and 45")
	}
	if o.MaxHeight != nil && !slices.Contains(allowedHeights, *o.MaxHeight) {
		return fmt.Errorf("max_height must be one of %v", allowedHeights)
	}
	if o.AudioBitrateK != nil && !slices.Contains(allowedBitrates, *o.AudioBitrateK) {
		return fmt.Errorf("audio_bitrate_k must be one of %v", allowedBitrates)
	}
	return nil
}
