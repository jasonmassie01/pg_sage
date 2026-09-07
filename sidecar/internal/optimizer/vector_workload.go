package optimizer

import "strings"

const (
	VectorShapeANN         = "ann_order_by_limit"
	VectorShapeFilteredANN = "filtered_ann"

	VectorWarningMissingLimit = "missing_limit"

	VectorRecommendationFilteredRecallCheck = "filtered_recall_check"
	VectorRecommendationAddLimit            = "add_limit"
)

type VectorWorkloadClassification struct {
	Shape                 string
	PrimaryRecommendation string
	Warnings              []string
	Evidence              []string
}

func ClassifyVectorWorkload(q QueryInfo) VectorWorkloadClassification {
	text := strings.ToLower(q.Text)
	if !looksLikeVectorQuery(text) {
		return VectorWorkloadClassification{}
	}

	out := VectorWorkloadClassification{
		Shape:    VectorShapeANN,
		Evidence: []string{"vector distance operator"},
	}
	hasLimit := strings.Contains(text, " limit ")
	hasFilter := strings.Contains(text, " where ")
	if !hasLimit {
		out.Warnings = append(out.Warnings, VectorWarningMissingLimit)
		out.PrimaryRecommendation = VectorRecommendationAddLimit
	}
	if hasFilter {
		out.Shape = VectorShapeFilteredANN
		out.PrimaryRecommendation = VectorRecommendationFilteredRecallCheck
		out.Evidence = append(out.Evidence, "scalar filter before ANN ordering")
	}
	return out
}

func looksLikeVectorQuery(text string) bool {
	return strings.Contains(text, "<->") ||
		strings.Contains(text, "<=>") ||
		strings.Contains(text, "<#>")
}
