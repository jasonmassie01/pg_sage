package optimizer

import "strings"

const (
	RiskSafe     = "safe"
	RiskModerate = "moderate"
	RiskHigh     = "high_risk"
)

// RiskTierForRecommendation returns the executor policy risk for an index
// recommendation. ActionLevel is confidence/exposure metadata, not risk.
func RiskTierForRecommendation(rec Recommendation) string {
	if strings.TrimSpace(rec.DDL) == "" {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(rec.ActionRisk)) {
	case RiskSafe:
		return RiskSafe
	case RiskModerate:
		return RiskModerate
	case RiskHigh:
		return RiskHigh
	case "":
		return riskFromActionLevel(rec.ActionLevel)
	default:
		return RiskHigh
	}
}

func riskFromActionLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "autonomous", "advisory", "informational", "":
		return RiskModerate
	default:
		return RiskHigh
	}
}
