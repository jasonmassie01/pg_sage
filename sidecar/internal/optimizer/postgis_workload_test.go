package optimizer

import (
	"testing"
)

func TestClassifyPostGISWorkload_DWithin(t *testing.T) {
	q := QueryInfo{
		Text: "SELECT id FROM places WHERE ST_DWithin(geom, $1, 1000)",
	}

	got := ClassifyPostGISWorkload(q)

	if got.Shape != PostGISShapeDistanceFilter {
		t.Fatalf("Shape = %q, want %q", got.Shape, PostGISShapeDistanceFilter)
	}
	if got.PrimaryRecommendation != PostGISRecommendationGiSTIndex {
		t.Fatalf("PrimaryRecommendation = %q, want %q",
			got.PrimaryRecommendation, PostGISRecommendationGiSTIndex)
	}
}

func TestClassifyPostGISWorkload_DistancePredicate(t *testing.T) {
	q := QueryInfo{
		Text: "SELECT id FROM places WHERE ST_Distance(geom, $1) < 1000",
	}

	got := ClassifyPostGISWorkload(q)

	if got.Shape != PostGISShapeDistanceFilter {
		t.Fatalf("Shape = %q, want %q", got.Shape, PostGISShapeDistanceFilter)
	}
	if got.PrimaryRecommendation != PostGISRecommendationDWithinRewrite {
		t.Fatalf("PrimaryRecommendation = %q, want %q",
			got.PrimaryRecommendation, PostGISRecommendationDWithinRewrite)
	}
}

func TestClassifyPostGISWorkload_TransformWarns(t *testing.T) {
	q := QueryInfo{
		Text: "SELECT id FROM places WHERE ST_Intersects(ST_Transform(geom, 3857), $1)",
	}

	got := ClassifyPostGISWorkload(q)

	if got.Shape != PostGISShapeTransformFilter {
		t.Fatalf("Shape = %q, want %q", got.Shape, PostGISShapeTransformFilter)
	}
	if got.PrimaryRecommendation != PostGISRecommendationTransformIndex {
		t.Fatalf("PrimaryRecommendation = %q, want %q",
			got.PrimaryRecommendation, PostGISRecommendationTransformIndex)
	}
	if !containsString(got.Warnings, PostGISRecommendationSRIDConsistency) {
		t.Fatalf("Warnings = %v, want %q",
			got.Warnings, PostGISRecommendationSRIDConsistency)
	}
}

func TestClassifyPostGISWorkload_NonSpatialQuery(t *testing.T) {
	q := QueryInfo{Text: "SELECT id FROM places WHERE city_id = $1"}

	got := ClassifyPostGISWorkload(q)

	if got.Shape != "" {
		t.Fatalf("Shape = %q, want empty", got.Shape)
	}
}
