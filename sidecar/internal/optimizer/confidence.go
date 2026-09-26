package optimizer

// ConfidenceInput holds normalized 0.0–1.0 signals for confidence scoring.
type ConfidenceInput struct {
	QueryVolume      float64 // 0.0–1.0 based on calls/day
	PlanClarity      float64 // 0.0–1.0 based on plan data availability
	WriteRateKnown   float64 // 1.0 if write rate computed, 0.0 if cold start
	HypoPGValidated  float64 // 1.0 if HypoPG measured an accepted gain; 0.0 if unavailable (a measured rejection drops the rec)
	SelectivityKnown float64 // 0.0–1.0 based on pg_stats availability
	TableCallVolume  float64 // 0.0–1.0 based on total queries hitting this table
}

// Weights for confidence scoring (must sum to 1.0). HypoPG is a hard
// gate when available (G3-B06), so its weight is modest; without HypoPG
// the 0.5 advisory threshold is reached only with measured evidence
// (query volume, write statistics, pg_stats selectivity).
const (
	weightQueryVolume     = 0.25
	weightPlanClarity     = 0.25
	weightWriteRateKnown  = 0.15
	weightHypoPGValidated = 0.10
	weightSelectivity     = 0.15
	weightTableCallVolume = 0.10
)

// ComputeConfidence returns a 0.0–1.0 confidence score using a weighted sum.
func ComputeConfidence(input ConfidenceInput) float64 {
	score := weightQueryVolume*input.QueryVolume +
		weightPlanClarity*input.PlanClarity +
		weightWriteRateKnown*input.WriteRateKnown +
		weightHypoPGValidated*input.HypoPGValidated +
		weightSelectivity*input.SelectivityKnown +
		weightTableCallVolume*input.TableCallVolume

	if score > 1.0 {
		score = 1.0
	}
	if score < 0.0 {
		score = 0.0
	}
	return score
}

// ActionLevel maps a confidence score to an action risk tier
// compatible with the executor trust system (safe/moderate/high_risk).
func ActionLevel(confidence float64) string {
	if confidence >= 0.7 {
		return "safe"
	}
	if confidence >= 0.4 {
		return "moderate"
	}
	return "high_risk"
}
