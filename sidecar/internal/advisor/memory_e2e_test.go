//go:build e2e

package advisor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestMemE2E_FullPipeline(t *testing.T) {
	pool := e2ePool(t)
	mgr := e2eLLMManager(t)
	snap := e2eSnapshot(t, pool)
	cfg := &config.Config{}
	logFn := func(level, msg string, args ...any) {
		t.Logf("[%s] "+msg, append([]any{level}, args...)...)
	}

	findings, err := analyzeMemory(
		context.Background(), mgr, snap, cfg, logFn,
	)
	if err != nil {
		t.Fatalf("analyzeMemory: %v", err)
	}

	t.Logf("memory findings: %d", len(findings))
	for _, f := range findings {
		t.Logf("  %s: %s -- %s",
			f.ObjectIdentifier, f.Severity, f.Recommendation)
	}
}
