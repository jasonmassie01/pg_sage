package optimizer

import (
	"testing"
)

func TestClassifyJSONWorkload_Containment(t *testing.T) {
	q := QueryInfo{Text: `SELECT * FROM events WHERE payload @> '{"type":"login"}'`}

	got := ClassifyJSONWorkload(q)

	if got.Shape != JSONShapeContainment {
		t.Fatalf("Shape = %q, want %q", got.Shape, JSONShapeContainment)
	}
	if got.PrimaryRecommendation != JSONRecommendationGINPathOps {
		t.Fatalf("PrimaryRecommendation = %q", got.PrimaryRecommendation)
	}
}

func TestClassifyJSONWorkload_KeyExistence(t *testing.T) {
	q := QueryInfo{Text: `SELECT * FROM events WHERE payload ? 'tenant_id'`}

	got := ClassifyJSONWorkload(q)

	if got.Shape != JSONShapeKeyExistence {
		t.Fatalf("Shape = %q, want %q", got.Shape, JSONShapeKeyExistence)
	}
	if got.PrimaryRecommendation != JSONRecommendationGINOps {
		t.Fatalf("PrimaryRecommendation = %q", got.PrimaryRecommendation)
	}
}

func TestClassifyJSONWorkload_ScalarExtraction(t *testing.T) {
	q := QueryInfo{Text: `SELECT * FROM events WHERE payload->>'tenant_id' = $1`}

	got := ClassifyJSONWorkload(q)

	if got.Shape != JSONShapeScalarExtraction {
		t.Fatalf("Shape = %q, want %q", got.Shape, JSONShapeScalarExtraction)
	}
	if got.PrimaryRecommendation != JSONRecommendationExpressionIndex {
		t.Fatalf("PrimaryRecommendation = %q", got.PrimaryRecommendation)
	}
}

func TestClassifyJSONWorkload_SortOnExtractedValue(t *testing.T) {
	q := QueryInfo{Text: `SELECT payload->>'score' FROM events ORDER BY payload->>'score'`}

	got := ClassifyJSONWorkload(q)

	if got.Shape != JSONShapeSortOrGroup {
		t.Fatalf("Shape = %q, want %q", got.Shape, JSONShapeSortOrGroup)
	}
	if got.PrimaryRecommendation != JSONRecommendationPromoteField {
		t.Fatalf("PrimaryRecommendation = %q", got.PrimaryRecommendation)
	}
}

func TestClassifyJSONWorkload_NonJSONQuery(t *testing.T) {
	q := QueryInfo{Text: `SELECT * FROM events WHERE id = $1`}

	got := ClassifyJSONWorkload(q)

	if got.Shape != "" {
		t.Fatalf("Shape = %q, want empty", got.Shape)
	}
	if got.PrimaryRecommendation != "" {
		t.Fatalf("PrimaryRecommendation = %q, want empty",
			got.PrimaryRecommendation)
	}
}
