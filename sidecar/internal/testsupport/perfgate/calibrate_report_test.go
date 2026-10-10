package perfgate

import (
	"fmt"
	"strings"
	"testing"
)

// The report says what the runner is (so reference runs can be grouped by
// runner type), what it measured over how many runs, each factor, the
// clamp, and the budgets each factor scaled, already scaled above it.
func TestReportShowsCalibration(t *testing.T) {
	c := mustCalibration(t, ReferenceCPUMs*0.8, ReferenceDBMs*1.2)
	c.CPUModel, c.CPUs = "AMD EPYC 7763 64-Core Processor", 4
	b := DefaultBudgets().Calibrated(c)
	md := RenderCalibratedMarkdown(SmallScale(), b, c, []Phase{steadyPhase()}, nil)
	ceiling := DefaultBudgets().MeanExempt[clusterSizeTag].CeilingMs * 1.2
	for _, want := range []string{
		"## Runner calibration", "AMD EPYC 7763 64-Core Processor, 4 CPUs",
		fmt.Sprintf("best of %d runs", calibrationRuns), "x0.75-x1.25",
		fmt.Sprintf("median of %d reference runs", len(referenceRuns)),
		fmt.Sprintf("| CPU workload | %.1f ms | %.1f ms | x0.80 |", ReferenceCPUMs*0.8,
			ReferenceCPUMs),
		fmt.Sprintf("| SQL workload | %.1f ms | %.1f ms | x1.20 |", ReferenceDBMs*1.2,
			ReferenceDBMs),
		"| larger of the two | | | x1.20 | " + string(GateEndpoint) + " |",
		"480 ms (steady phase)", "120 ms (steady phase)", "HTTP 200 within 1200 ms",
		fmt.Sprintf("ceiling %.0f ms mean", ceiling),
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("report lacks %q:\n%s", want, md)
		}
	}
	if strings.Index(md, "## Runner calibration") > strings.Index(md, "## Offenders") {
		t.Fatalf("calibration should follow the budgets, before the offenders:\n%s", md)
	}
}

// Off Linux (or on ARM) /proc/cpuinfo names no model; the report says so
// rather than printing an empty runner.
func TestReportShowsAnUnknownCPUModel(t *testing.T) {
	c := mustCalibration(t, ReferenceCPUMs, ReferenceDBMs)
	c.CPUs = 2
	md := RenderCalibratedMarkdown(SmallScale(), DefaultBudgets().Calibrated(c), c,
		[]Phase{steadyPhase()}, nil)
	if !strings.Contains(md, "unknown CPU model, 2 CPUs") {
		t.Fatalf("report lacks the unknown model:\n%s", md)
	}
}

func TestReportShowsAnUnmeasuredCalibration(t *testing.T) {
	md := RenderCalibratedMarkdown(SmallScale(), DefaultBudgets(), Calibration{},
		[]Phase{steadyPhase()}, nil)
	if !strings.Contains(md, "## Runner calibration") || !strings.Contains(md,
		"Not measured") || strings.Contains(md, "| CPU workload |") {
		t.Fatalf("unmeasured calibration:\n%s", md)
	}
}
