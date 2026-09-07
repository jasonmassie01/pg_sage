package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestAnalyzerFiltersOnlyPrivateSnapshotCopy(t *testing.T) {
	source := &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "orders"},
			{SchemaName: "sage", RelName: "findings"},
		},
		Indexes: []collector.IndexStats{
			{SchemaName: "public", IndexRelName: "orders_pkey"},
			{SchemaName: "sage", IndexRelName: "findings_pkey"},
		},
	}

	private := snapshotForAnalysis(source)
	filterSchemaExclusions(private)
	private.Tables[0].RelName = "changed"

	if len(source.Tables) != 2 || source.Tables[0].RelName != "orders" {
		t.Fatalf("source tables mutated: %#v", source.Tables)
	}
	if len(source.Indexes) != 2 {
		t.Fatalf("source indexes mutated: %#v", source.Indexes)
	}
	if len(private.Tables) != 1 || len(private.Indexes) != 1 {
		t.Fatalf("private filter result: %#v", private)
	}
}
