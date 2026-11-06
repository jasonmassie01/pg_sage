package freeze

import "math"

// BloatPlanThreshold is the least bloat ratio that plans an online rebuild.
const BloatPlanThreshold = 0.50

// Where a ResponseInput's bloat ratio came from.
const (
	BloatSourceEstimate    = "estimate"
	BloatSourcePgstattuple = "pgstattuple"
)

// EstimatedBloat is the catalog bloat estimate of a heap of pages pages
// whose analyzed rows need expectedPages: 1 - expected/pages, clamped to
// [0, 1]. There is no estimate (false) without pages, or when the catalog
// has none (a nil expectedPages: never analyzed, or no column statistics).
func EstimatedBloat(pages int64, expectedPages *float64) (float64, bool) {
	if pages <= 0 || expectedPages == nil || math.IsNaN(*expectedPages) ||
		math.IsInf(*expectedPages, 0) {
		return 0, false
	}
	ratio := 1 - *expectedPages/float64(pages)
	return math.Max(0, math.Min(1, ratio)), true
}
