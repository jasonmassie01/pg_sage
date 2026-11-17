package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
)

// evid-02a: workload evidence pack builds by result and the last pack's
// bytes render as one series each, under their registered headers. No
// concurrency test: the writer renders a snapshot taken under workpack's lock.

func TestWriteWorkloadPackMetrics_RendersEverySeries(t *testing.T) {
	var b strings.Builder
	writeWorkloadPackMetrics(&b, []workpack.BuildCount{
		{Database: "app", Result: "built", Count: 2},
		{Database: "app", Result: "too_large", Count: 1},
	}, []workpack.PackSize{{Database: "app", Bytes: 40960}})
	out := b.String()
	for _, want := range []string{
		"# TYPE pg_sage_workload_pack_builds_total counter",
		`pg_sage_workload_pack_builds_total{database="app",result="built"} 2`,
		`pg_sage_workload_pack_builds_total{database="app",result="too_large"} 1`,
		"# TYPE pg_sage_workload_pack_bytes gauge",
		`pg_sage_workload_pack_bytes{database="app"} 40960`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}

func TestWriteWorkloadPackMetrics_NoSeriesBeforeTheFirstBuild(t *testing.T) {
	var b strings.Builder
	writeWorkloadPackMetrics(&b, nil, nil)
	if strings.Contains(b.String(), "{database=") {
		t.Fatalf("no build yet must render no series:\n%s", b.String())
	}
}
