package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
	"github.com/pg-sage/sidecar/internal/startup"
)

// LLM features default on. Wiring with config.DefaultConfig(): without
// an endpoint and key every LLM-backed component stays out (deterministic
// paths only); with them the same defaults wire the LLM components.

func standaloneDefaultsRuntime(t *testing.T, endpoint, key string) *databaseRuntime {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	cfg.LLM.Endpoint, cfg.LLM.APIKey, cfg.LLM.Model = endpoint, key, "m"
	llmClient = llm.New(&cfg.LLM, nil)
	llmMgr = newStandaloneLLMManager(llmClient)
	rt := &databaseRuntime{
		spec:   databaseRuntimeSpec{Name: "orders", Shared: true},
		ctx:    context.Background(),
		cfg:    cfg,
		checks: &startup.CheckResult{PGVersionNum: 160000},
	}
	rt.resolveLLM()
	return rt
}

func TestDefaultsWithoutLLMWireDeterministicRuntime(t *testing.T) {
	rt := standaloneDefaultsRuntime(t, "", "")
	if !cfg.LLM.Enabled || !cfg.Advisor.Enabled || !cfg.LLM.Optimizer.Enabled {
		t.Fatal("precondition: llm, advisor and optimizer default on")
	}
	if rt.llmOn {
		t.Fatal("llmOn = true with no endpoint or key")
	}
	if opt := rt.newOptimizer(false); opt != nil {
		t.Error("optimizer built without a usable LLM")
	}
	if adv := rt.newAdvisor(); adv != nil {
		t.Error("advisor built without a usable LLM")
	}
	if rt.advisorActive() {
		t.Error("advisorActive = true without a usable LLM")
	}
	if rt.newCollector().CollectsConfigSnapshots() {
		t.Error("collector snapshots config for an advisor that cannot run")
	}
	if len(rt.features) != 0 {
		t.Errorf("features noted = %v, want none", rt.features)
	}
}

func TestDefaultsWithLLMWireLLMComponents(t *testing.T) {
	rt := standaloneDefaultsRuntime(t, "http://127.0.0.1:1/v1", "fixture-key")
	if !rt.llmOn {
		t.Fatal("llmOn = false with endpoint and key configured")
	}
	if rt.newOptimizer(false) == nil {
		t.Error("optimizer not built with a usable LLM")
	}
	if rt.newAdvisor() == nil {
		t.Error("advisor not built with a usable LLM")
	}
	if !rt.advisorActive() || !rt.newCollector().CollectsConfigSnapshots() {
		t.Error("advisor inactive or config snapshot off with a usable LLM")
	}
	if llmMgr.Optimizer != nil {
		t.Error("dedicated optimizer client built though optimizer_llm is off")
	}
	primary, fallback := tunerLLMClients(rt.llmManager)
	if primary != llmClient || fallback != nil {
		t.Errorf("tuner clients = %p/%p, want the general client only", primary, fallback)
	}
}

// Explicit opt-outs still win when an LLM is configured.
func TestExplicitOptOutsWinWithConfiguredLLM(t *testing.T) {
	rt := standaloneDefaultsRuntime(t, "http://127.0.0.1:1/v1", "fixture-key")
	cfg.LLM.Optimizer.Enabled = false
	cfg.Advisor.Enabled = false
	if rt.newOptimizer(false) != nil || rt.newAdvisor() != nil {
		t.Error("explicit optimizer/advisor false ignored")
	}
	if rt.newCollector().CollectsConfigSnapshots() {
		t.Error("config snapshot collected with advisor.enabled=false")
	}
	cfg.LLM.Enabled = false
	llmClient = llm.New(&cfg.LLM, nil)
	llmMgr = newStandaloneLLMManager(llmClient)
	rt.resolveLLM()
	if rt.llmOn {
		t.Error("llm.enabled=false left llmOn true")
	}
}

// captureStderr returns what fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	original := os.Stderr
	os.Stderr = writer
	done := make(chan string)
	go func() {
		data, _ := io.ReadAll(reader)
		done <- string(data)
	}()
	defer func() { os.Stderr = original }()
	fn()
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	return <-done
}

func TestLLMSetupNoticeLoggedOncePerProcess(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	configController = nil
	out := captureStderr(t, func() {
		for i := 0; i < 2; i++ {
			if err := initializeConfigController(nil); err != nil {
				t.Errorf("initializeConfigController: %v", err)
			}
		}
		if err := ensureConfigController(); err != nil {
			t.Errorf("ensureConfigController: %v", err)
		}
	})
	if got := strings.Count(out, "llm.api_key"); got != 1 {
		t.Fatalf("setup notice lines = %d, want 1; stderr:\n%s", got, out)
	}
	if !strings.Contains(out, "[INFO]") {
		t.Errorf("notice not logged at INFO: %q", out)
	}
}

func TestLLMSetupNoticeSilentWhenConfiguredOrOff(t *testing.T) {
	for _, mutate := range []func(*config.Config){
		func(c *config.Config) { c.LLM.Endpoint, c.LLM.APIKey = "http://x/v1", "k" },
		func(c *config.Config) { c.LLM.Enabled = false },
	} {
		preserveFleetRuntimeGlobals(t)
		cfg = config.DefaultConfig()
		mutate(cfg)
		configController = nil
		out := captureStderr(t, func() {
			if err := initializeConfigController(nil); err != nil {
				t.Errorf("initializeConfigController: %v", err)
			}
		})
		if strings.Contains(out, "llm.api_key") {
			t.Errorf("setup notice logged for a configured or disabled LLM: %q", out)
		}
	}
}
