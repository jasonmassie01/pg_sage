package main

import (
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// static.md F3 / D_catalog P3: /metrics walked the whole database
// directory (pg_database_size) on every scrape, with no statement
// timeout. The gauge now reports the collector's cached size.

func TestDatabaseSizeMetric_FromCollectorSnapshot(t *testing.T) {
	snap := &collector.Snapshot{System: collector.SystemStats{DBSizeBytes: 123456789}}
	got := databaseSizeMetric(snap)
	if !strings.Contains(got, "pg_sage_database_size_bytes 123456789\n") ||
		!strings.Contains(got, "# TYPE pg_sage_database_size_bytes gauge") {
		t.Fatalf("metric = %q", got)
	}
}

func TestDatabaseSizeMetric_UnknownSizeOmitsGauge(t *testing.T) {
	for name, snap := range map[string]*collector.Snapshot{
		"nil snapshot": nil,
		"never sized":  {System: collector.SystemStats{DBSizeBytes: 0}},
		"negative":     {System: collector.SystemStats{DBSizeBytes: -1}},
	} {
		if got := databaseSizeMetric(snap); got != "" {
			t.Errorf("%s: metric = %q, want no gauge (unknown is not zero)", name, got)
		}
	}
}

func TestPrometheusHandler_DoesNotWalkTheDatabase(t *testing.T) {
	src, err := os.ReadFile("prometheus.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	if strings.Contains(string(src), "pg_database_size(") {
		t.Fatal("prometheus.go calls pg_database_size on every scrape")
	}
}
