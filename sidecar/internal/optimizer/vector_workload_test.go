package optimizer

import (
	"strings"
	"testing"
)

func TestClassifyVectorWorkload_ANNOrderByLimit(t *testing.T) {
	q := QueryInfo{Text: "SELECT id FROM docs ORDER BY embedding <-> $1 LIMIT 10"}

	got := ClassifyVectorWorkload(q)

	if got.Shape != VectorShapeANN {
		t.Fatalf("Shape = %q, want %q", got.Shape, VectorShapeANN)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("Warnings = %v, want none", got.Warnings)
	}
}

func TestClassifyVectorWorkload_MissingLimitWarns(t *testing.T) {
	q := QueryInfo{Text: "SELECT id FROM docs ORDER BY embedding <=> $1"}

	got := ClassifyVectorWorkload(q)

	if got.Shape != VectorShapeANN {
		t.Fatalf("Shape = %q, want %q", got.Shape, VectorShapeANN)
	}
	if !containsString(got.Warnings, VectorWarningMissingLimit) {
		t.Fatalf("Warnings = %v, want %q", got.Warnings,
			VectorWarningMissingLimit)
	}
}

func TestClassifyVectorWorkload_FilteredANN(t *testing.T) {
	q := QueryInfo{
		Text: "SELECT id FROM docs WHERE tenant_id = $2 ORDER BY embedding <#> $1 LIMIT 10",
	}

	got := ClassifyVectorWorkload(q)

	if got.Shape != VectorShapeFilteredANN {
		t.Fatalf("Shape = %q, want %q", got.Shape, VectorShapeFilteredANN)
	}
	if got.PrimaryRecommendation != VectorRecommendationFilteredRecallCheck {
		t.Fatalf("PrimaryRecommendation = %q", got.PrimaryRecommendation)
	}
}

func TestClassifyVectorWorkload_NonVectorQuery(t *testing.T) {
	q := QueryInfo{Text: "SELECT id FROM docs WHERE tenant_id = $1"}

	got := ClassifyVectorWorkload(q)

	if got.Shape != "" {
		t.Fatalf("Shape = %q, want empty", got.Shape)
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

func TestFormatPrompt_IncludesVectorWorkloadHints(t *testing.T) {
	tc := sampleTableContext()
	tc.Columns = append(tc.Columns, ColumnInfo{
		Name: "embedding", Type: "vector(1536)", IsNullable: false,
	})
	tc.Queries = []QueryInfo{{
		QueryID: 123,
		Text: "SELECT id FROM orders WHERE tenant_id = $2 " +
			"ORDER BY embedding <-> $1 LIMIT 10",
		Calls: 250,
	}}

	prompt := FormatPrompt(tc)

	if !strings.Contains(prompt, "### Vector Workload Hints") {
		t.Fatalf("prompt missing vector workload hints: %s", prompt)
	}
	if !strings.Contains(prompt, VectorShapeFilteredANN) {
		t.Fatalf("prompt missing shape %q", VectorShapeFilteredANN)
	}
	if !strings.Contains(prompt, VectorRecommendationFilteredRecallCheck) {
		t.Fatalf("prompt missing recommendation %q",
			VectorRecommendationFilteredRecallCheck)
	}
}
