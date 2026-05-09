package optimizer

import "strings"

const (
	JSONShapeContainment      = "containment"
	JSONShapeKeyExistence     = "key_existence"
	JSONShapeScalarExtraction = "scalar_extraction"
	JSONShapeSortOrGroup      = "sort_or_group_on_extracted_value"
	JSONShapePathPredicate    = "json_path_predicate"

	JSONRecommendationGINPathOps       = "gin_jsonb_path_ops"
	JSONRecommendationGINOps           = "gin_jsonb_ops"
	JSONRecommendationExpressionIndex  = "expression_index"
	JSONRecommendationPromoteField     = "promote_field"
	JSONRecommendationJSONPathAnalysis = "json_path_analysis"
)

type JSONWorkloadClassification struct {
	Shape                 string
	PrimaryRecommendation string
	Evidence              []string
}

func ClassifyJSONWorkload(q QueryInfo) JSONWorkloadClassification {
	text := strings.ToLower(q.Text)
	if !looksLikeJSONQuery(text) {
		return JSONWorkloadClassification{}
	}
	if hasSortOrGroupOnExtractedJSON(text) {
		return jsonClassification(
			JSONShapeSortOrGroup,
			JSONRecommendationPromoteField,
			"sort/group on JSON-derived scalar",
		)
	}
	if strings.Contains(text, "jsonb_path_") ||
		strings.Contains(text, "@@") ||
		strings.Contains(text, "@?") {
		return jsonClassification(
			JSONShapePathPredicate,
			JSONRecommendationJSONPathAnalysis,
			"JSON path predicate",
		)
	}
	if strings.Contains(text, "->>") {
		return jsonClassification(
			JSONShapeScalarExtraction,
			JSONRecommendationExpressionIndex,
			"scalar extraction with ->>",
		)
	}
	if strings.Contains(text, "?|") ||
		strings.Contains(text, "?&") ||
		strings.Contains(text, " ? ") {
		return jsonClassification(
			JSONShapeKeyExistence,
			JSONRecommendationGINOps,
			"key existence operator",
		)
	}
	if strings.Contains(text, "@>") {
		return jsonClassification(
			JSONShapeContainment,
			JSONRecommendationGINPathOps,
			"containment operator",
		)
	}
	return JSONWorkloadClassification{}
}

func looksLikeJSONQuery(text string) bool {
	return strings.Contains(text, "json") ||
		strings.Contains(text, "payload") ||
		strings.Contains(text, "->>") ||
		strings.Contains(text, " -> ") ||
		strings.Contains(text, "@>") ||
		strings.Contains(text, "?|") ||
		strings.Contains(text, "?&") ||
		strings.Contains(text, " ? ")
}

func hasSortOrGroupOnExtractedJSON(text string) bool {
	hasExtract := strings.Contains(text, "->>") || strings.Contains(text, " -> ")
	return hasExtract &&
		(strings.Contains(text, "order by") || strings.Contains(text, "group by"))
}

func jsonClassification(
	shape string,
	recommendation string,
	evidence string,
) JSONWorkloadClassification {
	return JSONWorkloadClassification{
		Shape:                 shape,
		PrimaryRecommendation: recommendation,
		Evidence:              []string{evidence},
	}
}
