package executor

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// config-10: the executor accepts exactly config.TrustLevels, plus "" to
// clear the per-database override.
func TestSetTrustLevel_AcceptsExactlyConfigTrustLevels(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trust.Level = "observation"
	e := newTestExecutor(cfg, time.Now())
	for _, level := range config.TrustLevels() {
		if err := e.SetTrustLevel(level); err != nil {
			t.Fatalf("SetTrustLevel(%q): %v", level, err)
		}
		if got := e.TrustLevel(); got != level {
			t.Fatalf("TrustLevel() = %q after setting %q", got, level)
		}
	}
	for _, level := range []string{"Autonomous", "auto", "manual", " advisory"} {
		if err := e.SetTrustLevel(level); err == nil {
			t.Fatalf("SetTrustLevel(%q) accepted an unknown level", level)
		}
		if got := e.TrustLevel(); got != "autonomous" {
			t.Fatalf("rejected %q changed the level to %q", level, got)
		}
	}
	if err := e.SetTrustLevel(""); err != nil {
		t.Fatalf("SetTrustLevel(\"\") (clear override): %v", err)
	}
	if got := e.TrustLevel(); got != "observation" {
		t.Fatalf("cleared override reads %q, want the global observation", got)
	}
}
