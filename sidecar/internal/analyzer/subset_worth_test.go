package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

func TestSubsetWorthDropping(t *testing.T) {
	sub := ParsedIndex{Columns: []string{"c1"}}
	// (c1) covered by (c1,c2) — 1 extra col, similar size -> worth dropping.
	if !subsetWorthDropping(sub, ParsedIndex{Columns: []string{"c1", "c2"}},
		collector.IndexStats{IndexBytes: 1000}, collector.IndexStats{IndexBytes: 1500}) {
		t.Error("close superset (1 extra col, 1.5x size) should be droppable")
	}
	// (c1) covered by (c1..c12) — 11 extra cols -> NOT worth dropping.
	wide := ParsedIndex{Columns: []string{"c1", "c2", "c3", "c4", "c5", "c6", "c7", "c8", "c9", "c10", "c11", "c12"}}
	if subsetWorthDropping(sub, wide, collector.IndexStats{IndexBytes: 1000}, collector.IndexStats{IndexBytes: 12000}) {
		t.Error("(c1) should NOT be replaced by (c1..c12)")
	}
	// 2 extra cols but superset is 5x the size -> size guard rejects.
	if subsetWorthDropping(sub, ParsedIndex{Columns: []string{"c1", "c2", "c3"}},
		collector.IndexStats{IndexBytes: 1000}, collector.IndexStats{IndexBytes: 5000}) {
		t.Error("5x-larger superset should be rejected by size guard")
	}
	// heavily-used narrow index, 2 extra cols -> keep it.
	if subsetWorthDropping(sub, ParsedIndex{Columns: []string{"c1", "c2", "c3"}},
		collector.IndexStats{IndexBytes: 1000, IdxScan: 500_000}, collector.IndexStats{IndexBytes: 2000}) {
		t.Error("heavily-used narrow index with >1 extra col should be kept")
	}
	// heavily-used but superset only 1 extra col + similar size -> still droppable.
	if !subsetWorthDropping(sub, ParsedIndex{Columns: []string{"c1", "c2"}},
		collector.IndexStats{IndexBytes: 1000, IdxScan: 500_000}, collector.IndexStats{IndexBytes: 1300}) {
		t.Error("near-identical superset should be droppable even if heavily used")
	}
}
