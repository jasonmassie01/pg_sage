package main

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/migration"
)

// migration-10: /metrics exposes the model work each database's enrichment
// queue dropped as pg_sage_migration_enrich_dropped_total.

func TestWriteMigrationEnrichMetrics(t *testing.T) {
	var b strings.Builder
	writeMigrationEnrichMetrics(&b, []migration.EnrichDropCount{
		{Database: "orders", Count: 24}, {Database: "billing", Count: 0},
	})
	out := b.String()
	for _, want := range []string{
		"# HELP pg_sage_migration_enrich_dropped_total ",
		"# TYPE pg_sage_migration_enrich_dropped_total counter\n",
		"pg_sage_migration_enrich_dropped_total{database=\"orders\"} 24\n",
		"pg_sage_migration_enrich_dropped_total{database=\"billing\"} 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}

func TestWriteMigrationEnrichMetricsEmpty(t *testing.T) {
	var b strings.Builder
	writeMigrationEnrichMetrics(&b, nil)
	out := b.String()
	if !strings.Contains(out, "# TYPE pg_sage_migration_enrich_dropped_total counter") {
		t.Fatalf("empty metrics lack the TYPE line:\n%s", out)
	}
	if strings.Contains(out, "{database=") {
		t.Fatalf("empty metrics have a series:\n%s", out)
	}
}

// A database name with a quote is escaped, not split into labels.
func TestWriteMigrationEnrichMetricsEscapes(t *testing.T) {
	var b strings.Builder
	writeMigrationEnrichMetrics(&b, []migration.EnrichDropCount{{Database: `a"b`, Count: 1}})
	if !strings.Contains(b.String(), `{database="a\"b"} 1`) {
		t.Fatalf("label not escaped:\n%s", b.String())
	}
}
