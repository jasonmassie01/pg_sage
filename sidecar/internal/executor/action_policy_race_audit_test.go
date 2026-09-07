package executor

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

// TestEvaluateActionPolicyConcurrentHotReload is primarily a race-detector
// regression test. A live config writer uses the documented hot-reload lock;
// policy evaluation must use the matching read lock before copying fields.
func TestEvaluateActionPolicyConcurrentHotReload(t *testing.T) {
	cfg := &config.Config{
		Trust: config.TrustConfig{
			Level:         "autonomous",
			Tier3Safe:     true,
			Tier3Moderate: true,
		},
	}
	contract := ActionContract{
		ActionType:   "create_index",
		BaseRiskTier: "safe",
	}
	ctx := ActionPolicyContext{
		Config:        cfg,
		ExecutionMode: "auto",
		Now:           time.Now(),
		RampStart:     time.Now().Add(-32 * 24 * time.Hour),
	}

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
			decision := EvaluateActionPolicy(contract, ctx)
			switch decision.Decision {
			case PolicyDecisionExecute, PolicyDecisionObserveOnly:
			default:
				t.Errorf("unexpected decision during reload: %q", decision.Decision)
				return
			}
		}
	}()
	wg.Wait()
}

// No integration test: policy evaluation is a pure in-memory boundary and the
// production race is exercised directly with the same package-level lock.
