package api_test

import (
	"testing"

	"github.com/ShifuRain/convertflow/internal/api"
)

func TestValidID(t *testing.T) {
	good := []string{"3f2b8c1e-5d4a-4b7e-9a10-0c1d2e3f4a5b"}
	bad := []string{"", "abc", "../etc", "3F2B8C1E-5D4A-4B7E-9A10-0C1D2E3F4A5B", "urn:uuid:3f2b8c1e-5d4a-4b7e-9a10-0c1d2e3f4a5b",
		"{3f2b8c1e-5d4a-4b7e-9a10-0c1d2e3f4a5b}", "3f2b8c1e5d4a4b7e9a100c1d2e3f4a5b", "3f2b8c1e-5d4a-4b7e-9a10-0c1d2e3f4a5b\n"}
	for _, g := range good {
		if !api.ValidID(g) {
			t.Errorf("%q should be valid", g)
		}
	}
	for _, b := range bad {
		if api.ValidID(b) {
			t.Errorf("%q should be invalid", b)
		}
	}
}
