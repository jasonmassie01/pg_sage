package main

import (
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// static.md F3 / D_catalog P3: /metrics walked the whole database
// directory (pg_database_size) on every scrape, with no statement
// timeout. The gauge reports the collector's cached size, which the
// fleet status carries per database (observability-02).

func TestDatabaseSizeMetric_PerDatabaseFromStatus(t *testing.T) {
	var b strings.Builder
	writeDatabaseSize(&b, "pg_sage_database_size_bytes", fleet.DatabaseStatus{
		Name: "orders", Status: &fleet.InstanceStatus{DatabaseSize: 123456789}})
	if got := b.String(); got != "pg_sage_database_size_bytes{database=\"orders\"} 123456789\n" {
		t.Fatalf("metric = %q", got)
	}
}

func TestDatabaseSizeMetric_UnknownSizeOmitsGauge(t *testing.T) {
	for name, size := range map[string]int64{"never sized": 0, "negative": -1} {
		var b strings.Builder
		writeDatabaseSize(&b, "pg_sage_database_size_bytes", fleet.DatabaseStatus{
			Name: "orders", Status: &fleet.InstanceStatus{DatabaseSize: size}})
		if got := b.String(); got != "" {
			t.Errorf("%s: metric = %q, want no sample (unknown is not zero)", name, got)
		}
	}
}

func TestPrometheusHandler_DoesNotWalkTheDatabase(t *testing.T) {
	for _, file := range []string{"prometheus.go", "database_health_metrics.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read source: %v", err)
		}
		if strings.Contains(string(src), "pg_database_size(") {
			t.Fatalf("%s calls pg_database_size on every scrape", file)
		}
	}
}
