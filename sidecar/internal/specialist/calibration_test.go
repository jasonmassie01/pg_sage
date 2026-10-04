package specialist

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/earned"
)

// Bench calibration: the weakest gated arm's top-1 for the family, never an
// ungated arm, another family or an invalid cell.

func TestWeakestGatedCell(t *testing.T) {
	run := &earned.EvalRun{ID: "r1", Provenance: "signed", Gated: []string{"graph", "model"},
		Cells: []earned.Cell{
			{Arm: "graph", Family: "lock_blocking", Top1: earned.Metric{K: 30, N: 30}},
			{Arm: "model", Family: "lock_blocking", Top1: earned.Metric{K: 18, N: 20}},
			{Arm: "rules-only", Family: "lock_blocking", Top1: earned.Metric{K: 1, N: 20}},
			{Arm: "graph", Family: "wal_retention", Top1: earned.Metric{K: 1, N: 9}},
			{Arm: "model", Family: "lock_blocking", Top1: earned.Metric{K: 5, N: 0}},
		}}
	got := weakestGatedCell(run, "lock_blocking")
	if got == nil || got.K != 18 || got.N != 20 || got.Rate != 0.9 ||
		got.WilsonLower <= 0 || got.WilsonLower >= 0.9 || got.Family != "lock_blocking" {
		t.Fatalf("calibration %+v", got)
	}
	if weakestGatedCell(run, "plan_regression") != nil {
		t.Fatal("no cell for the family is no calibration")
	}
}

func TestBenchCalibrator_WithoutALedger(t *testing.T) {
	ctx := context.Background()
	if c, err := (BenchCalibrator{}).Calibrate(ctx, "orders", "lock_blocking"); c != nil ||
		err != nil {
		t.Fatalf("no registry: %+v %v", c, err)
	}
	reg := earned.NewRegistry(true)
	if c, err := (BenchCalibrator{Registry: reg}).Calibrate(ctx, "orders",
		"lock_blocking"); c != nil || err != nil {
		t.Fatalf("unknown database: %+v %v", c, err)
	}
}
