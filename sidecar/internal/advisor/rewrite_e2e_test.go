//go:build e2e

package advisor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestRewriteE2E_FullPipeline(t *testing.T) {
	pool := e2ePool(t)
	mgr := e2eLLMManager(t)
	snap := e2eSnapshot(t, pool)
	cfg := &config.Config{}
	logFn := func(level, msg string, args ...any) {
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}

	findings, err := analyzeQueryRewrites(
		context.Background(), pool, mgr, snap, cfg, logFn,
	)
	if err != nil {
		t.Fatalf("analyzeQueryRewrites: %v", err)
	}

	t.Logf("rewrite findings: %d", len(findings))
	for _, f := range findings {
		t.Logf("  %s: %s -- %s",
			f.ObjectIdentifier, f.Severity, f.Recommendation)
		if f.RecommendedSQL != "" {
			t.Errorf(
				"rewrite finding should have empty RecommendedSQL, got: %s",
				f.RecommendedSQL,
			)
		}
		if f.Severity != "info" {
			t.Errorf(
				"rewrite finding should be info severity, got: %s",
				f.Severity,
			)
		}
	}
}
