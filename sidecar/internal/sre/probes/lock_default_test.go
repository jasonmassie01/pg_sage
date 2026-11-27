package probes

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// The catalog default and the autoexplain on-demand EXPLAIN share one lock
// bound (rca-05): a change to it moves both.
func TestSpecDefaultLockTimeoutIsTheSharedProbeBound(t *testing.T) {
	got := spec(LockChains, "locks", ArgsNone).LockTimeout
	if got != selfmonitor.ProbeLockTimeout {
		t.Fatalf("spec default lock timeout = %v, want selfmonitor.ProbeLockTimeout %v",
			got, selfmonitor.ProbeLockTimeout)
	}
}
