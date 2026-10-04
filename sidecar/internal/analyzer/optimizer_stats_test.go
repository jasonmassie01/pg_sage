package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/optimizer"
)

// The Prometheus exporter reads the optimizer's rejection-memory counters
// through the analyzer; a database without an optimizer reports nothing.

// No concurrent access tests here: the counters are atomics owned and
// tested by the optimizer package; this accessor only forwards them.

func TestOptimizerMemoryStats_WithoutOptimizer(t *testing.T) {
	var nilAnalyzer *Analyzer
	if _, ok := nilAnalyzer.OptimizerMemoryStats(); ok {
		t.Fatal("a nil analyzer reported optimizer stats")
	}
	if _, ok := (&Analyzer{}).OptimizerMemoryStats(); ok {
		t.Fatal("an analyzer without an optimizer reported stats")
	}
}

func TestOptimizerMemoryStats_ForwardsTheOptimizer(t *testing.T) {
	opt := optimizer.New(nil, nil, nil, &config.OptimizerConfig{}, 170000, 8192,
		func(string, string, ...any) {})
	stats, ok := (&Analyzer{optimizer: opt}).OptimizerMemoryStats()
	if !ok || stats != opt.MemoryStats() || stats != (optimizer.MemoryStats{}) {
		t.Fatalf("stats = %+v ok=%t, want the optimizer's zero counters", stats, ok)
	}
}
