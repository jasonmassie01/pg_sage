package tuning

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/optimizer"
)

// indexes-09: a PostGIS KNN or pg_trgm table is not sent to the model as a
// vector ANN workload, and the vector hint names a next step.
// No concurrency test: workloadHints is pure over its TableContext.

func hintsContaining(hints []string, sub string) []string {
	var out []string
	for _, h := range hints {
		if strings.Contains(h, sub) {
			out = append(out, h)
		}
	}
	return out
}

func TestWorkloadHints_PostGISKNNTableHasNoVectorHint(t *testing.T) {
	tc := optimizer.TableContext{Schema: "public", Table: "shops",
		Columns: []optimizer.ColumnInfo{{Name: "id", Type: "bigint"},
			{Name: "open", Type: "boolean"}, {Name: "geom", Type: "geometry"}},
		Queries: []optimizer.QueryInfo{{QueryID: 7,
			Text: "SELECT id FROM shops WHERE open ORDER BY geom <-> $1::geometry LIMIT 5"}}}

	hints := workloadHints(tc)

	if v := hintsContaining(hints, "vector"); len(v) != 0 {
		t.Fatalf("vector hints for a PostGIS table: %q (all %q)", v, hints)
	}
	want := "queryid 7: PostGIS " + optimizer.PostGISShapeKNN + ", consider " +
		optimizer.PostGISRecommendationKNNGiSTIndex
	if len(hints) != 1 || hints[0] != want {
		t.Fatalf("hints = %q, want exactly %q", hints, want)
	}
}

func TestWorkloadHints_TrigramTableHasNoVectorHint(t *testing.T) {
	tc := optimizer.TableContext{Schema: "public", Table: "people",
		Columns: []optimizer.ColumnInfo{{Name: "name", Type: "text"}},
		Queries: []optimizer.QueryInfo{{QueryID: 8,
			Text: "SELECT name FROM people ORDER BY name <-> 'jon' LIMIT 10"}}}

	if hints := workloadHints(tc); len(hintsContaining(hints, "vector")) != 0 {
		t.Fatalf("vector hint for a pg_trgm query: %q", hints)
	}
}

func TestWorkloadHints_FilteredANNHintNamesVectorLab(t *testing.T) {
	tc := optimizer.TableContext{Schema: "public", Table: "docs",
		Columns: []optimizer.ColumnInfo{{Name: "tenant_id", Type: "integer"},
			{Name: "embedding", Type: "vector"}},
		Queries: []optimizer.QueryInfo{{QueryID: 9, Text: "SELECT id FROM docs\n" +
			"WHERE tenant_id = $2\nORDER BY embedding <=> $1\nLIMIT 10"}}}

	hints := workloadHints(tc)

	want := "queryid 9: vector " + optimizer.VectorShapeFilteredANN + ", consider " +
		optimizer.VectorRecommendationFilteredRecallCheck
	if len(hints) != 1 || hints[0] != want {
		t.Fatalf("hints = %q, want exactly %q", hints, want)
	}
	if !strings.Contains(hints[0], "pg_sage vector-lab") {
		t.Fatalf("hint %q does not point at vector-lab", hints[0])
	}
}

// A table whose column types were not read gives no vector hint.
func TestWorkloadHints_UnknownColumnsGiveNoVectorHint(t *testing.T) {
	tc := optimizer.TableContext{Schema: "public", Table: "docs",
		Queries: []optimizer.QueryInfo{{QueryID: 10,
			Text: "SELECT id FROM docs ORDER BY embedding <-> $1 LIMIT 10"}}}

	if hints := workloadHints(tc); len(hints) != 0 {
		t.Fatalf("hints = %q, want none", hints)
	}
}
