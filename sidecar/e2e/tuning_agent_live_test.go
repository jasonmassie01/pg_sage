//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/tuning"
)

// The tuning agent against a live model (opt-in: PG_SAGE_LIVE_LLM=1 and
// SAGE_LLM_API_KEY). It replaces the per-feature live prompt tests of the
// removed optimizer, advisor and tuner prompts: whatever the model
// answers, nothing outside the typed proposal forms is admitted, and
// every admitted proposal cites evidence and predicts its effect.
func TestTuningAgentLive(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LLM") != "1" {
		t.Skip("PG_SAGE_LIVE_LLM=1 not set: live LLM tests are opt-in")
	}
	key := requireAPIKey(t)
	pool := pipelinePool(t)
	mustExec(t, pool, "CREATE EXTENSION IF NOT EXISTS hypopg")
	_, prev, cur := tuningWorkload(t, pool)
	client := llm.New(newTestLLMConfig(key, largeBudget), testLogFn(t))
	agent := e2eTuningAgent(t, pool, client)
	out, err := agent.Tune(context.Background(), cur, prev)
	if err != nil {
		t.Fatalf("tune: %v", err)
	}
	for _, f := range out.Findings {
		if f.Detail["producer"] != tuning.Producer {
			continue
		}
		if _, ok := f.Detail["predicted_effect"].(map[string]any); !ok {
			t.Errorf("finding without a prediction: %+v", f)
		}
		if ev, ok := f.Detail["evidence"].([]tuning.Evidence); !ok || len(ev) == 0 {
			t.Errorf("finding without cited evidence: %+v", f)
		}
		upper := strings.ToUpper(f.RecommendedSQL)
		for _, bad := range []string{"DROP TABLE", "TRUNCATE", "DELETE", "VACUUM FULL"} {
			if strings.Contains(upper, bad) {
				t.Errorf("forbidden SQL admitted: %s", f.RecommendedSQL)
			}
		}
	}
	t.Logf("live tuning agent: %d finding(s)", len(out.Findings))
}
