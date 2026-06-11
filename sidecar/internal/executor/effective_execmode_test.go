package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestEffectiveExecMode(t *testing.T) {
	mk := func(execMode, trustOverride, cfgTrust string) *Executor {
		return &Executor{
			execMode:           execMode,
			trustLevelOverride: trustOverride,
			cfg:                &config.Config{Trust: config.TrustConfig{Level: cfgTrust}},
		}
	}
	cases := []struct {
		name                        string
		execMode, override, cfgTrust string
		want                        string
	}{
		{"manual+observation stays manual", "manual", "", "observation", "manual"},
		{"manual+advisory -> auto", "manual", "", "advisory", "auto"},
		{"manual+autonomous -> auto", "manual", "", "autonomous", "auto"},
		{"manual+autonomous via override -> auto", "manual", "autonomous", "observation", "auto"},
		{"manual+empty trust stays manual", "manual", "", "", "manual"},
		{"auto always auto", "auto", "", "observation", "auto"},
		{"approval is respected (not promoted)", "approval", "", "autonomous", "approval"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mk(c.execMode, c.override, c.cfgTrust).effectiveExecMode(); got != c.want {
				t.Errorf("effectiveExecMode() = %q, want %q", got, c.want)
			}
		})
	}
}
