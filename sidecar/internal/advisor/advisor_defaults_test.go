package advisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

// advisor.enabled now defaults to on. These tests run the real advisor
// code with config.DefaultConfig(): it reaches a configured provider,
// and the default llm block alone (no endpoint, no key) never does.

func countingAdvisorLLM(t *testing.T, content string) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]string{
						"role": "assistant", "content": content,
					},
					"finish_reason": "stop",
				}},
				"usage": map[string]int{"total_tokens": 80},
			})
		}))
	t.Cleanup(srv.Close)
	return srv.URL, &calls
}

func vacuumSnapshots() (*collector.Snapshot, *collector.Snapshot) {
	now := time.Now()
	lastVac := now.Add(-2 * time.Hour)
	snap := &collector.Snapshot{
		CollectedAt: now,
		ConfigData: &collector.ConfigSnapshot{PGSettings: []collector.PGSetting{
			{Name: "autovacuum", Setting: "on"},
			{Name: "autovacuum_vacuum_scale_factor", Setting: "0.2"},
		}},
		Tables: []collector.TableStats{{
			SchemaName: "public", RelName: "orders", NLiveTup: 10000,
			NDeadTup: 2000, NTupIns: 5000, NTupUpd: 3000, NTupDel: 500,
			LastAutovacuum: &lastVac, AutovacuumCount: 10,
		}},
	}
	prev := &collector.Snapshot{
		CollectedAt: now.Add(-time.Hour),
		Tables: []collector.TableStats{{
			SchemaName: "public", RelName: "orders", NLiveTup: 9500,
			NDeadTup: 1000, NTupIns: 2000, NTupUpd: 1000, NTupDel: 200,
		}},
	}
	return snap, prev
}

// vacuumAdvice claims its own action_risk; the advisor must ignore it.
const vacuumAdvice = `[{"object_identifier":"public.orders",` +
	`"severity":"info","rationale":"Scale factor too high",` +
	`"action_risk":"none",` +
	`"recommended_sql":"ALTER TABLE public.orders SET ` +
	`(autovacuum_vacuum_scale_factor = 0.02)"}]`

func TestAdvisorDefaultsCallConfiguredLLM(t *testing.T) {
	cfg := config.DefaultConfig()
	if !cfg.Advisor.Enabled || !cfg.LLM.Enabled || !cfg.Advisor.VacuumEnabled {
		t.Fatal("precondition: advisor, llm and vacuum advice default on")
	}
	url, calls := countingAdvisorLLM(t, vacuumAdvice)
	cfg.LLM.Endpoint, cfg.LLM.APIKey, cfg.LLM.Model = url, "k", "m"
	client := llm.New(&cfg.LLM, noopLog)
	mgr := llm.NewManager(client, nil, cfg.LLM.OptimizerLLM.FallbackToGeneral)
	snap, prev := vacuumSnapshots()
	findings, err := analyzeVacuum(context.Background(), mgr, snap, prev, cfg, noopLog)
	if err != nil {
		t.Fatalf("analyzeVacuum: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1", calls.Load())
	}
	if len(findings) != 1 || findings[0].Category != "vacuum_tuning" {
		t.Fatalf("findings = %+v, want one vacuum_tuning finding", findings)
	}
	// Risk is derived from the SQL, never taken from the model's answer.
	if findings[0].ActionRisk != deriveActionRisk(findings[0].RecommendedSQL) {
		t.Errorf("action risk %q, want %q derived from the SQL",
			findings[0].ActionRisk, deriveActionRisk(findings[0].RecommendedSQL))
	}
	if client.TokensUsedToday() != 80 {
		t.Errorf("tokens charged = %d, want 80", client.TokensUsedToday())
	}
}

func TestAdvisorDefaultLLMBlockWithoutEndpointNeverCalls(t *testing.T) {
	cfg := config.DefaultConfig()
	url, calls := countingAdvisorLLM(t, vacuumAdvice)
	client := llm.New(&cfg.LLM, noopLog)
	mgr := llm.NewManager(client, nil, true)
	snap, prev := vacuumSnapshots()
	findings, err := analyzeVacuum(context.Background(), mgr, snap, prev, cfg, noopLog)
	if err == nil || len(findings) != 0 {
		t.Fatalf("findings=%+v err=%v, want an LLM-unavailable error", findings, err)
	}
	if calls.Load() != 0 {
		t.Errorf("provider %s called %d times without an endpoint", url, calls.Load())
	}
}

// The master switch still governs the advisor when it is constructed.
func TestAdvisorExplicitLLMOffEmitsDegradedFinding(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LLM.Enabled = false
	a := New(nil, cfg, nil, nil, noopLog)
	findings, err := a.Analyze(context.Background())
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(findings) != 1 || findings[0].Category != "advisor_degraded" ||
		findings[0].RecommendedSQL != "" {
		t.Fatalf("findings = %+v, want one SQL-less advisor_degraded finding",
			findings)
	}
}
