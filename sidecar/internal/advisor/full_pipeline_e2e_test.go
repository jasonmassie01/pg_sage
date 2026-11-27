//go:build e2e

package advisor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestFullPipeline_AllAdvisorsRun(t *testing.T) {
	mgr := e2eLLMManager(t)
	pool := e2ePool(t)
	snap := e2eSnapshot(t, pool)

	cfg := &config.Config{}
	cfg.Advisor.Enabled = true
	cfg.Advisor.IntervalSeconds = 1
	cfg.Advisor.WALEnabled = true
	cfg.Advisor.ConnectionEnabled = true
	cfg.Advisor.RewriteEnabled = true
	cfg.Advisor.BloatEnabled = true
	cfg.LLM.Enabled = true

	logFn := e2eAdvisorLog(t)

	// Verify advisor struct can be constructed with real components.
	adv := New(pool, cfg, nil, mgr, logFn)
	_ = adv

	var totalFindings int
	before := mgr.General.TokensUsedToday()

	if walFindings, err := analyzeWAL(
		context.Background(), mgr, snap, cfg, logFn,
	); err != nil {
		t.Errorf("WAL error: %v", err)
	} else {
		totalFindings += len(walFindings)
		t.Logf("WAL: %d findings", len(walFindings))
	}
	requireAdvisorModelCall(t, mgr, before)
	before = mgr.General.TokensUsedToday()

	if connFindings, err := analyzeConnections(
		context.Background(), mgr, snap, cfg, logFn,
	); err != nil {
		t.Errorf("connection error: %v", err)
	} else {
		totalFindings += len(connFindings)
		t.Logf("connection: %d findings", len(connFindings))
	}
	requireAdvisorModelCall(t, mgr, before)
	before = mgr.General.TokensUsedToday()

	if rwFindings, err := analyzeQueryRewrites(
		context.Background(), mgr, snap, cfg, logFn,
	); err != nil {
		t.Errorf("rewrite error: %v", err)
	} else {
		totalFindings += len(rwFindings)
		t.Logf("rewrite: %d findings", len(rwFindings))
	}
	requireAdvisorModelCall(t, mgr, before)
	before = mgr.General.TokensUsedToday()

	if bloatFindings, err := analyzeBloat(
		context.Background(), mgr, snap, cfg, logFn,
	); err != nil {
		t.Errorf("bloat error: %v", err)
	} else {
		totalFindings += len(bloatFindings)
		t.Logf("bloat: %d findings", len(bloatFindings))
	}
	requireAdvisorModelCall(t, mgr, before)

	t.Logf("TOTAL FINDINGS: %d across all advisors", totalFindings)
}
