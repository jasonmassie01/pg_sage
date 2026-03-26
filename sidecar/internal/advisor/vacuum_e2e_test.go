//go:build e2e

package advisor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestVacuumE2E_FullPipeline(t *testing.T) {
	pool := e2ePool(t)
	mgr := e2eLLMManager(t)
	snap := e2eSnapshot(t, pool)

	if len(snap.Tables) == 0 {
		t.Skip("no tables found")
	}

	cfg := &config.Config{}
	logFn := func(level, msg string, args ...any) {
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}

	findings, err := analyzeVacuum(
		context.Background(), mgr, snap, nil, cfg, logFn,
	)
	if err != nil {
		t.Fatalf("analyzeVacuum: %v", err)
	}

	t.Logf("vacuum findings: %d", len(findings))
	for _, f := range findings {
		t.Logf("  %s: %s -- %s",
			f.ObjectIdentifier, f.Severity, f.Recommendation)
		if f.RecommendedSQL != "" {
			t.Logf("    SQL: %s", f.RecommendedSQL)
		}
	}
}

func TestVacuumE2E_FindingsAreInfo(t *testing.T) {
	pool := e2ePool(t)
	mgr := e2eLLMManager(t)
	snap := e2eSnapshot(t, pool)
	cfg := &config.Config{}
	logFn := func(string, string, ...any) {}

	findings, err := analyzeVacuum(
		context.Background(), mgr, snap, nil, cfg, logFn,
	)
	if err != nil {
		t.Fatalf("analyzeVacuum: %v", err)
	}
	for _, f := range findings {
		if f.Category != "vacuum_tuning" {
			t.Errorf("expected category vacuum_tuning, got %s", f.Category)
		}
	}
}
