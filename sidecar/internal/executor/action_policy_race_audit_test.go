package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// TestActionPolicyConcurrentHotReload is primarily a race-detector
// regression test. A live config writer uses the documented hot-reload lock;
// the gate's runtime snapshot must read under the matching read lock.
func TestActionPolicyConcurrentHotReload(t *testing.T) {
	cfg := &config.Config{Trust: config.TrustConfig{
		Level: "autonomous", Tier3Safe: true, Tier3Moderate: true,
	}}
	exec := New(nil, cfg, nil, time.Now().Add(-32*24*time.Hour), noopExecLog)
	withTestStandingGate(exec)
	exec.SetExecutionMode("auto")
	contracts := []ActionContract{AnalyzeTableContract()}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1_000; i++ {
			config.LockForHotReload()
			if i%2 == 0 {
				cfg.Trust.Level = "observation"
			} else {
				cfg.Trust.Level = "autonomous"
			}
			config.UnlockForHotReload()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1_000; i++ {
			decision := exec.ExplainFamilies(context.Background(), contracts, false)[0]
			switch decision.Decision {
			case PolicyDecisionExecute, PolicyDecisionObserveOnly:
			default:
				t.Errorf("unexpected decision during reload: %#v", decision)
				return
			}
		}
	}()
	wg.Wait()
}

// No integration test: evaluation is in-memory and the production race is
// exercised directly with the same package-level lock.
