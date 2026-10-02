package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// retention.sage_size_warning_pct can be written through the API: the
// override reaches the configuration the analyzer reads (applied per the
// config lifecycle registry).
func TestHotReloadRetention_SageSizeWarningPct(t *testing.T) {
	cfg := config.DefaultConfig()
	for _, tc := range []struct {
		value string
		want  int
	}{{"25", 25}, {"0", 0}, {"100", 100}} {
		hotReloadRetention(cfg, "retention.sage_size_warning_pct", tc.value)
		if cfg.Retention.SageSizeWarningPct != tc.want {
			t.Fatalf("after %q: %d, want %d", tc.value, cfg.Retention.SageSizeWarningPct,
				tc.want)
		}
	}
}
