package optimizer

import "strings"

const (
	PostGISShapeSpatialPredicate = "spatial_predicate"
	PostGISShapeDistanceFilter   = "distance_filter"
	PostGISShapeTransformFilter  = "transform_filter"
	PostGISShapeKNN              = "knn_spatial_ordering"

	PostGISRecommendationGiSTIndex       = "gist_spatial_index"
	PostGISRecommendationDWithinRewrite  = "st_dwithin_rewrite"
	PostGISRecommendationTransformIndex  = "transform_expression_index"
	PostGISRecommendationKNNGiSTIndex    = "knn_gist_index"
	PostGISRecommendationSRIDConsistency = "srid_consistency_check"
)

type PostGISWorkloadClassification struct {
	Shape                 string
	PrimaryRecommendation string
	Warnings              []string
	Evidence              []string
}

func ClassifyPostGISWorkload(q QueryInfo) PostGISWorkloadClassification {
	text := strings.ToLower(q.Text)
	if !looksLikePostGISQuery(text) {
		return PostGISWorkloadClassification{}
	}
	if strings.Contains(text, "order by") && strings.Contains(text, "<->") {
		return postGISClassification(
			PostGISShapeKNN,
			PostGISRecommendationKNNGiSTIndex,
			"spatial nearest-neighbor ordering",
		)
	}
	if strings.Contains(text, "st_transform(") {
		out := postGISClassification(
			PostGISShapeTransformFilter,
			PostGISRecommendationTransformIndex,
			"ST_Transform used inside predicate/order expression",
		)
		out.Warnings = append(out.Warnings, PostGISRecommendationSRIDConsistency)
		return out
	}
	if strings.Contains(text, "st_distance(") {
		return postGISClassification(
			PostGISShapeDistanceFilter,
			PostGISRecommendationDWithinRewrite,
			"ST_Distance predicate may be non-sargable",
		)
	}
	if strings.Contains(text, "st_dwithin(") {
		return postGISClassification(
			PostGISShapeDistanceFilter,
			PostGISRecommendationGiSTIndex,
			"ST_DWithin distance predicate",
		)
	}
	return postGISClassification(
		PostGISShapeSpatialPredicate,
		PostGISRecommendationGiSTIndex,
		"PostGIS spatial predicate",
	)
}

func looksLikePostGISQuery(text string) bool {
	return strings.Contains(text, "st_dwithin(") ||
		strings.Contains(text, "st_intersects(") ||
		strings.Contains(text, "st_contains(") ||
		strings.Contains(text, "st_within(") ||
		strings.Contains(text, "st_distance(") ||
		strings.Contains(text, "st_transform(") ||
		strings.Contains(text, "geography") ||
		strings.Contains(text, "geometry")
}

func postGISClassification(
	shape string,
	recommendation string,
	evidence string,
) PostGISWorkloadClassification {
	return PostGISWorkloadClassification{
		Shape:                 shape,
		PrimaryRecommendation: recommendation,
		Evidence:              []string{evidence},
	}
}
