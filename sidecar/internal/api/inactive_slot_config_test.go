package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// analyzer.inactive_slot_min_minutes can be written through the API: the
// override reaches the configuration the analyzer reads each cycle.
func TestHotReloadAnalyzer_InactiveSlotMinMinutes(t *testing.T) {
	cfg := config.DefaultConfig()
	for _, tc := range []struct {
		value string
		want  int
	}{{"30", 30}, {"1", 1}, {"5", 5}} {
		hotReloadAnalyzer(cfg, "analyzer.inactive_slot_min_minutes", tc.value)
		if cfg.Analyzer.InactiveSlotMinMinutes != tc.want {
			t.Fatalf("after %q: %d, want %d", tc.value,
				cfg.Analyzer.InactiveSlotMinMinutes, tc.want)
		}
	}
}
