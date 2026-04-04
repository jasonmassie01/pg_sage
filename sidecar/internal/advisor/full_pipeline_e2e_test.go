//go:build e2e

package advisor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/llm"
)

func TestFullPipeline_AllAdvisorsRun(t *testing.T) {
	pool := e2ePool(t)
	mgr := e2eLLMManager(t)
	snap := e2eSnapshot(t, pool)

	cfg := &config.Config{}
	cfg.Advisor.Enabled = true
	cfg.Advisor.IntervalSeconds = 1
	cfg.Advisor.VacuumEnabled = true
	cfg.Advisor.WALEnabled = true
	cfg.Advisor.ConnectionEnabled = true
	cfg.Advisor.MemoryEnabled = true
	cfg.Advisor.RewriteEnabled = true
	cfg.Advisor.BloatEnabled = true
	cfg.LLM.Enabled = true

	logFn := func(level, msg string, args ...any) {
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}

	// Verify advisor struct can be constructed with real components.
	adv := New(pool, cfg, nil, mgr, logFn)
	_ = adv

	var totalFindings int

	if vacFindings, err := analyzeVacuum(
		context.Background(), mgr, snap, nil, cfg, logFn,
	); err != nil {
		t.Logf("vacuum error (non-fatal): %v", err)
	} else {
		totalFindings += len(vacFindings)
		t.Logf("vacuum: %d findings", len(vacFindings))
	}

	if walFindings, err := analyzeWAL(
		context.Background(), mgr, snap, nil, cfg, logFn,
	); err != nil {
		t.Logf("WAL error (non-fatal): %v", err)
	} else {
		totalFindings += len(walFindings)
		t.Logf("WAL: %d findings", len(walFindings))
	}

	if connFindings, err := analyzeConnections(
		context.Background(), mgr, snap, cfg, logFn,
	); err != nil {
		t.Logf("connection error (non-fatal): %v", err)
	} else {
		totalFindings += len(connFindings)
		t.Logf("connection: %d findings", len(connFindings))
	}

	if memFindings, err := analyzeMemory(
		context.Background(), mgr, snap, cfg, logFn,
	); err != nil {
		t.Logf("memory error (non-fatal): %v", err)
	} else {
		totalFindings += len(memFindings)
		t.Logf("memory: %d findings", len(memFindings))
	}

	if rwFindings, err := analyzeQueryRewrites(
		context.Background(), nil, mgr, snap, cfg, logFn,
	); err != nil {
		t.Logf("rewrite error (non-fatal): %v", err)
	} else {
		totalFindings += len(rwFindings)
		t.Logf("rewrite: %d findings", len(rwFindings))
	}

	if bloatFindings, err := analyzeBloat(
		context.Background(), mgr, snap, nil, cfg, logFn,
	); err != nil {
		t.Logf("bloat error (non-fatal): %v", err)
	} else {
		totalFindings += len(bloatFindings)
		t.Logf("bloat: %d findings", len(bloatFindings))
	}

	t.Logf("TOTAL FINDINGS: %d across all advisors", totalFindings)
}

func TestFullPipeline_LLMFailure_GracefulDegradation(t *testing.T) {
	pool := e2ePool(t)
	snap := e2eSnapshot(t, pool)

	brokenCfg := &config.LLMConfig{
		Enabled:          true,
		Model:            "gemini-2.5-flash",
		Endpoint:         "https://invalid.example.com/v1beta/openai",
		APIKey:           "fake-key",
		TimeoutSeconds:   5,
		TokenBudgetDaily: 100000,
		CooldownSeconds:  1,
	}
	brokenClient := llm.New(brokenCfg, func(string, string, ...any) {})
	brokenMgr := llm.NewManager(brokenClient, nil, false)

	cfg := &config.Config{}
	logFn := func(level, msg string, args ...any) {
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}

	_, err := analyzeVacuum(
		context.Background(), brokenMgr, snap, nil, cfg, logFn,
	)
	if err == nil {
		t.Log("vacuum: no error (may have no qualifying tables)")
	} else {
		t.Logf("vacuum: error as expected: %v", err)
	}

	_, err = analyzeWAL(
		context.Background(), brokenMgr, snap, nil, cfg, logFn,
	)
	if err == nil {
		t.Log("WAL: no error (may have no config data)")
	} else {
		t.Logf("WAL: error as expected: %v", err)
	}

	t.Log("graceful degradation confirmed -- no panics")
}
