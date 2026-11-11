package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/logwatch"
)

// rca-04: records dropped for their size are a third drop reason.
func TestWriteLogwatchMetrics_OversizedRecords(t *testing.T) {
	var b strings.Builder
	writeLogwatchMetrics(&b, []clusterLogDrops{
		{Cluster: "db1:5432", Counts: logwatch.DropCounts{Oversized: 4}},
	})
	out := b.String()
	if !strings.Contains(out, logwatchSeries("db1:5432", "oversized_record", 4)) ||
		!strings.Contains(out, "oversized_record") {
		t.Fatalf("metrics lack the oversized_record series:\n%s", out)
	}
	if !strings.Contains(out, logwatchSeries("db1:5432", "queue_overflow", 0)) {
		t.Fatalf("the other reasons must still be written:\n%s", out)
	}
}
