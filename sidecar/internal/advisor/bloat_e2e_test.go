//go:build e2e

package advisor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestBloatE2E_FullPipeline(t *testing.T) {
	pool := e2ePool(t)
	mgr := e2eLLMManager(t)
	snap := e2eSnapshot(t, pool)
	cfg := &config.Config{}
	logFn := func(level, msg string, args ...any) {
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}

	findings, err := analyzeBloat(
		context.Background(), mgr, snap, nil, cfg, logFn,
	)
	if err != nil {
		t.Fatalf("analyzeBloat: %v", err)
	}

	t.Logf("bloat findings: %d", len(findings))
	for _, f := range findings {
		t.Logf("  %s: %s -- %s",
			f.ObjectIdentifier, f.Severity, f.Recommendation)
		if f.Severity != "info" {
			t.Errorf(
				"bloat finding should be info severity, got: %s",
				f.Severity,
			)
		}
		if f.RecommendedSQL != "" {
			t.Errorf(
				"bloat finding should have empty RecommendedSQL, got: %s",
				f.RecommendedSQL,
			)
		}
	}
}
