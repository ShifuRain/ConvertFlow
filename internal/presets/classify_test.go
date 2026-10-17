package presets

import "testing"

// Every encoder a preset can use must be classified explicitly, so adding a preset with a new
// (possibly patent-encumbered) encoder cannot silently pass the lgpl policy.
func TestAllEncodersClassified(t *testing.T) {
	for _, e := range AllEncoders() {
		if !lgplSafe[e] && !fullOnly[e] {
			t.Errorf("encoder %q is in no list: add it to lgplSafe (royalty-free) or fullOnly (withheld in lgpl)", e)
		}
		if lgplSafe[e] && fullOnly[e] {
			t.Errorf("encoder %q is in both lgplSafe and fullOnly", e)
		}
	}
}

// Whatever is not on the lgpl allow-list must be refused by the lgpl flavor (and by an unknown one).
func TestEveryNonSafeEncoderRefusedByLgpl(t *testing.T) {
	for _, e := range AllEncoders() {
		if lgplSafe[e] {
			continue
		}
		for _, flavor := range []string{"lgpl", "", "LGPL", "other"} {
			if FlavorAllows(flavor, e) {
				t.Errorf("flavor %q allows non-lgpl-safe encoder %q", flavor, e)
			}
		}
		if !FlavorAllows("full", e) {
			t.Errorf("full flavor must allow %q", e)
		}
	}
}
