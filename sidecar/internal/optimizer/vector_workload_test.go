package optimizer

import (
	"strings"
	"testing"
)

// indexes-09: a distance operator alone (<->, <=>, <#>) is not a vector
// query. <-> is also PostGIS KNN, pg_trgm and btree_gist distance, and <#>
// is the PostGIS box distance, so the classifier asks for an operand whose
// column type, or cast, is vector, halfvec or sparsevec.
// No concurrency test: ClassifyVectorWorkload is pure and stateless.

var docsCols = []ColumnInfo{
	{Name: "id", Type: "bigint"},
	{Name: "tenant_id", Type: "integer"},
	{Name: "embedding", Type: "vector"},
	{Name: "e", Type: "halfvec"},
	{Name: "limit_x", Type: "integer"},
}

func TestClassifyVectorWorkload_ANNOrderByLimit(t *testing.T) {
	q := QueryInfo{Text: "SELECT id FROM docs ORDER BY embedding <-> $1 LIMIT 10"}

	got := ClassifyVectorWorkload(q, docsCols)

	if got.Shape != VectorShapeANN {
		t.Fatalf("Shape = %q, want %q", got.Shape, VectorShapeANN)
	}
	if len(got.Warnings) != 0 || got.PrimaryRecommendation != "" {
		t.Fatalf("Warnings = %v rec %q, want none", got.Warnings,
			got.PrimaryRecommendation)
	}
	if !containsString(got.Evidence, "embedding is vector") {
		t.Fatalf("Evidence = %v, want the vector operand named", got.Evidence)
	}
}

func TestClassifyVectorWorkload_CosineOnVectorColumn(t *testing.T) {
	q := QueryInfo{Text: "SELECT id FROM docs d ORDER BY d.embedding <=> $1 LIMIT 5"}

	got := ClassifyVectorWorkload(q, docsCols)

	if got.Shape != VectorShapeANN || len(got.Warnings) != 0 {
		t.Fatalf("got %+v, want plain ANN with no warning", got)
	}
}

func TestClassifyVectorWorkload_MissingLimitWarns(t *testing.T) {
	q := QueryInfo{Text: "SELECT id FROM docs ORDER BY embedding <=> $1"}

	got := ClassifyVectorWorkload(q, docsCols)

	if got.Shape != VectorShapeANN {
		t.Fatalf("Shape = %q, want %q", got.Shape, VectorShapeANN)
	}
	if !containsString(got.Warnings, VectorWarningMissingLimit) {
		t.Fatalf("Warnings = %v, want %q", got.Warnings,
			VectorWarningMissingLimit)
	}
	if got.PrimaryRecommendation != VectorRecommendationAddLimit {
		t.Fatalf("PrimaryRecommendation = %q, want %q",
			got.PrimaryRecommendation, VectorRecommendationAddLimit)
	}
}

func TestClassifyVectorWorkload_FilteredANN(t *testing.T) {
	q := QueryInfo{
		Text: "SELECT id FROM docs WHERE tenant_id = $2 ORDER BY embedding <=> $1 LIMIT 10",
	}

	got := ClassifyVectorWorkload(q, docsCols)

	if got.Shape != VectorShapeFilteredANN {
		t.Fatalf("Shape = %q, want %q", got.Shape, VectorShapeFilteredANN)
	}
	if got.PrimaryRecommendation != VectorRecommendationFilteredRecallCheck {
		t.Fatalf("PrimaryRecommendation = %q", got.PrimaryRecommendation)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none (the query has a LIMIT)", got.Warnings)
	}
}

// The filtered-ANN advice names a next step the operator can take.
func TestClassifyVectorWorkload_FilteredRecallAdviceLeadsSomewhere(t *testing.T) {
	rec := VectorRecommendationFilteredRecallCheck
	for _, want := range []string{"recall", "iterative scan", "ef_search",
		"pg_sage vector-lab"} {
		if !strings.Contains(rec, want) {
			t.Errorf("recommendation %q does not mention %q", rec, want)
		}
	}
}

func TestClassifyVectorWorkload_CastMakesVector(t *testing.T) {
	cases := []struct {
		name, text string
	}{
		{"param halfvec on the left", "SELECT 1 FROM t ORDER BY $1::halfvec <-> e LIMIT 3"},
		{"param vector on the right", "SELECT 1 FROM t ORDER BY x <-> $1::vector LIMIT 3"},
		{"literal with typmod", "SELECT 1 FROM t ORDER BY x <#> '[1,2,3]'::vector(3) LIMIT 3"},
		{"schema-qualified type", "SELECT 1 FROM t ORDER BY x <=> $1 :: public.sparsevec LIMIT 3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyVectorWorkload(QueryInfo{Text: c.text}, nil)
			if got.Shape != VectorShapeANN {
				t.Fatalf("Shape = %q, want %q (%+v)", got.Shape, VectorShapeANN, got)
			}
		})
	}
}

// Invalid input: the same operators on non-vector types give no shape.
func TestClassifyVectorWorkload_NonVectorDistanceOperators(t *testing.T) {
	cols := []ColumnInfo{{Name: "geom", Type: "geometry"}, {Name: "name", Type: "text"},
		{Name: "ts", Type: "timestamp with time zone"}, {Name: "open", Type: "boolean"},
		{Name: "box", Type: "box2d"}}
	cases := []struct {
		name, text string
	}{
		{"postgis knn", "SELECT id FROM shops WHERE open ORDER BY geom <-> $1 LIMIT 5"},
		{"postgis box distance", "SELECT id FROM shops ORDER BY geom <#> $1 LIMIT 5"},
		{"pg_trgm distance", "SELECT id FROM shops ORDER BY name <-> 'abc' LIMIT 5"},
		{"btree_gist distance", "SELECT id FROM ev ORDER BY ts <-> now() LIMIT 5"},
		{"function on the left", "SELECT id FROM ev ORDER BY now() <-> ts LIMIT 5"},
		{"geography cast", "SELECT id FROM s ORDER BY geom::geography <-> $1 LIMIT 5"},
		{"operator inside a literal", "SELECT id FROM s WHERE name = 'x <-> y' LIMIT 5"},
		{"no distance operator", "SELECT id FROM docs WHERE tenant_id = $1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyVectorWorkload(QueryInfo{Text: c.text}, cols)
			if got.Shape != "" || got.PrimaryRecommendation != "" ||
				got.Warnings != nil || got.Evidence != nil {
				t.Fatalf("got %+v, want an empty classification", got)
			}
		})
	}
}

// An unknown column type is not vector: no hint rather than a wrong one.
func TestClassifyVectorWorkload_UnknownColumnTypeIsNotVector(t *testing.T) {
	text := "SELECT id FROM docs ORDER BY embedding <-> $1 LIMIT 10"
	for name, cols := range map[string][]ColumnInfo{
		"nil columns":      nil,
		"empty columns":    {},
		"user-defined":     {{Name: "embedding", Type: "USER-DEFINED"}},
		"column not found": {{Name: "other", Type: "vector"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := ClassifyVectorWorkload(QueryInfo{Text: text}, cols)
			if got.Shape != "" {
				t.Fatalf("Shape = %q, want empty", got.Shape)
			}
		})
	}
}

func TestClassifyVectorWorkload_EmptyText(t *testing.T) {
	got := ClassifyVectorWorkload(QueryInfo{}, nil)
	if got.Shape != "" || got.Warnings != nil {
		t.Fatalf("got %+v, want an empty classification", got)
	}
}

// Boundary: LIMIT and WHERE are words, wherever the whitespace falls.
func TestClassifyVectorWorkload_LimitAndWhereAreWords(t *testing.T) {
	cases := []struct {
		name, text   string
		missingLimit bool
		filtered     bool
	}{
		{"limit on a new line", "SELECT id FROM t\nORDER BY e <-> $1\nLIMIT 10", false, false},
		{"limit after a tab", "SELECT id FROM t ORDER BY e <-> $1\tLIMIT 10", false, false},
		{"where on a new line", "SELECT id FROM t\nWHERE tenant_id = $2\n" +
			"ORDER BY e <-> $1 LIMIT 10", false, true},
		{"limit_x column is not a limit", "SELECT limit_x FROM t ORDER BY e <-> $1",
			true, false},
		{"no limit at all", "SELECT id FROM t ORDER BY e <-> $1", true, false},
		{"fetch first rows", "SELECT id FROM t ORDER BY e <-> $1 FETCH FIRST 5 ROWS ONLY",
			false, false},
		{"limit parameter glued", "SELECT id FROM t ORDER BY e <-> $1 LIMIT($2)",
			false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ClassifyVectorWorkload(QueryInfo{Text: c.text}, docsCols)
			if got.Shape == "" {
				t.Fatalf("not classified: %+v", got)
			}
			if has := containsString(got.Warnings, VectorWarningMissingLimit); has !=
				c.missingLimit {
				t.Fatalf("missing_limit = %t, want %t (%+v)", has, c.missingLimit, got)
			}
			if f := got.Shape == VectorShapeFilteredANN; f != c.filtered {
				t.Fatalf("filtered = %t, want %t (%+v)", f, c.filtered, got)
			}
		})
	}
}

// A query mixing a PostGIS KNN and a vector distance is vector because of
// the vector operand, not because of the geometry one.
func TestClassifyVectorWorkload_MixedOperatorsUseVectorOperand(t *testing.T) {
	cols := []ColumnInfo{{Name: "geom", Type: "geometry"}, {Name: "embedding",
		Type: "vector"}}
	q := QueryInfo{Text: "SELECT id FROM s WHERE geom <-> $2 < 10 " +
		"ORDER BY embedding <=> $1 LIMIT 5"}

	got := ClassifyVectorWorkload(q, cols)

	if got.Shape != VectorShapeFilteredANN {
		t.Fatalf("Shape = %q, want %q", got.Shape, VectorShapeFilteredANN)
	}
	if containsString(got.Evidence, "geom is vector") ||
		!containsString(got.Evidence, "embedding is vector") {
		t.Fatalf("Evidence = %v", got.Evidence)
	}
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// Text inside string literals and comments is not SQL: an operator or a
// LIMIT/WHERE there says nothing about the statement, even when it names a
// vector column. A function named like a vector column is not that column.
func TestClassifyVectorWorkload_LiteralsCommentsAndCallsAreNotOperands(t *testing.T) {
	cols := []ColumnInfo{{Name: "embedding", Type: "vector"}, {Name: "name", Type: "text"}}
	for name, text := range map[string]string{
		"operator in a literal": "SELECT id FROM d WHERE note = 'embedding <-> $1' LIMIT 5",
		"operator in a block comment": "/* ORDER BY embedding <=> $1 */ " +
			"SELECT id FROM d ORDER BY name <-> $1 LIMIT 5",
		"operator in a line comment": "SELECT id FROM d -- embedding <=> $1\n" +
			"ORDER BY name <-> $1 LIMIT 5",
		"call named like the column": "SELECT id FROM d ORDER BY name <-> embedding($1) LIMIT 5",
	} {
		t.Run(name, func(t *testing.T) {
			got := ClassifyVectorWorkload(QueryInfo{Text: text}, cols)
			if got.Shape != "" || got.Evidence != nil {
				t.Fatalf("got %+v, want an empty classification", got)
			}
		})
	}
}

func TestClassifyVectorWorkload_LimitOrWhereOnlyInLiteralOrComment(t *testing.T) {
	text := "SELECT id /* where tenant_id = $3 limit 1 */ FROM d " +
		"ORDER BY embedding <=> $1 -- limit 10\n, note = 'where x limit 2'"
	got := ClassifyVectorWorkload(QueryInfo{Text: text}, docsCols)

	if got.Shape != VectorShapeANN {
		t.Fatalf("Shape = %q, want %q (%+v)", got.Shape, VectorShapeANN, got)
	}
	if !containsString(got.Warnings, VectorWarningMissingLimit) {
		t.Fatalf("Warnings = %v, want %q", got.Warnings, VectorWarningMissingLimit)
	}
}

// A literal keeps its cast after its text is blanked.
func TestClassifyVectorWorkload_CastLiteralWithQuoteInside(t *testing.T) {
	text := "SELECT 1 FROM t ORDER BY x <-> 'it''s <-> [1]'::sparsevec LIMIT 3"
	got := ClassifyVectorWorkload(QueryInfo{Text: text}, nil)

	if got.Shape != VectorShapeANN || !containsString(got.Evidence, "cast to sparsevec") {
		t.Fatalf("got %+v, want ANN by the sparsevec cast", got)
	}
}
