//go:build e2e

package advisor

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

func TestConnE2E_FullPipeline(t *testing.T) {
	mgr := e2eLLMManager(t)
	pool := e2ePool(t)
	snap := e2eSnapshot(t, pool)
	cfg := &config.Config{}
	logFn := e2eAdvisorLog(t)

	findings, err := analyzeConnections(
		context.Background(), mgr, snap, cfg, logFn,
	)
	if err != nil {
		t.Fatalf("analyzeConnections: %v", err)
	}

	t.Logf("connection findings: %d", len(findings))
	for _, f := range findings {
		t.Logf("  %s: %s -- %s",
			f.ObjectIdentifier, f.Severity, f.Recommendation)
	}
}
