package executor

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// collection-08: verify's host-CPU admission reads
// safety.host_cpu_ceiling_pct, inheriting cpu_ceiling_pct when unset.
func TestVerificationHostCPUCeilingKey(t *testing.T) {
	cases := []struct {
		name      string
		host, cpu int
		want      float64
	}{
		{"host key set", 85, 70, 85},
		{"host key unset inherits the session ceiling", 0, 70, 70},
		{"defaults", 0, config.DefaultCPUCeilingPct, 90},
	}
	for _, tc := range cases {
		cfg := config.DefaultConfig()
		cfg.Safety.HostCPUCeilingPct, cfg.Safety.CPUCeilingPct = tc.host, tc.cpu
		if got := verificationOptions(cfg).CPUCeilingPct; got != tc.want {
			t.Errorf("%s: verify CPU ceiling = %v, want %v", tc.name, got, tc.want)
		}
	}
}
