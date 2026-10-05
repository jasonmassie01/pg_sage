package specialist

import (
	"context"
	"fmt"
	"slices"

	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/modellift"
)

// BenchCalibrator reads a family's top-1 accuracy on the newest bench
// report that counts for the database's ledger: how often pg_sage named
// the right root for that family. With several gated arms the weakest
// (lowest Wilson lower bound) is reported. No report, no gated cell: nil
// (the result says "uncalibrated").
type BenchCalibrator struct{ Registry *earned.Registry }

// Calibrate implements Calibrator.
func (c BenchCalibrator) Calibrate(ctx context.Context, database,
	family string) (*CalibratedRate, error) {
	if c.Registry == nil {
		return nil, nil
	}
	entry, ok := c.Registry.Lookup(database)
	if !ok || entry.Service == nil || entry.Service.Store() == nil {
		return nil, nil
	}
	run, err := entry.Service.Store().LatestBench(ctx, earned.Family(family))
	if err != nil {
		return nil, fmt.Errorf("read the latest bench report: %w", err)
	}
	if run == nil {
		return nil, nil
	}
	return weakestGatedCell(run, family), nil
}

func weakestGatedCell(run *earned.EvalRun, family string) *CalibratedRate {
	var best *CalibratedRate
	for _, cell := range run.Cells {
		if cell.Family != family || !slices.Contains(run.Gated, cell.Arm) ||
			cell.Top1.N <= 0 || cell.Top1.K < 0 || cell.Top1.K > cell.Top1.N {
			continue
		}
		lo, _ := modellift.Wilson(cell.Top1.K, cell.Top1.N)
		rate := &CalibratedRate{Family: family, K: cell.Top1.K, N: cell.Top1.N,
			Rate: float64(cell.Top1.K) / float64(cell.Top1.N), WilsonLower: lo,
			Source: fmt.Sprintf("bench report %s (%s, arm %s)", run.ID, run.Provenance,
				cell.Arm)}
		if best == nil || lo < best.WilsonLower {
			best = rate
		}
	}
	return best
}
