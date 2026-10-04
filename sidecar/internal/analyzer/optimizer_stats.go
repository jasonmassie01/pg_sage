package analyzer

import "github.com/pg-sage/sidecar/internal/optimizer"

// OptimizerMemoryStats returns the index optimizer's rejection-memory
// counters; ok is false when this database runs no optimizer.
func (a *Analyzer) OptimizerMemoryStats() (optimizer.MemoryStats, bool) {
	if a == nil || a.optimizer == nil {
		return optimizer.MemoryStats{}, false
	}
	return a.optimizer.MemoryStats(), true
}
