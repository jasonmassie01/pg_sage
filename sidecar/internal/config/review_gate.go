package config

import (
	"fmt"
	"math"
	"strings"
)

// The model gate (review-05a). G-M1 widens the model's authority only on a
// person's approval; G-M2 only narrows. The defaults are decision D11's
// proposals for the owner: 50 reviews, 10 points, 2%.
const (
	DefaultReviewGateMinReviews   = 50
	DefaultReviewGateMaxGapPoints = 10.0
	DefaultReviewGateMaxRevertPct = 2.0
	maxReviewGateMinReviews       = 10000
)

// ReviewGateConfig is review.gate. Every field is read when a database's
// runtime starts (restart lifecycle).
type ReviewGateConfig struct {
	MinReviews       int      `yaml:"min_reviews" doc:"Reviews on real databases, each with a finished model pass of the running model, that G-M1 and G-M2 need before they decide. Range 1-10000. Default: 50 (decision D11, an owner proposal)."`
	MaxGapPoints     float64  `yaml:"max_gap_points" doc:"G-M1 passes when the model's verified-improved rate is at least the generators' rate minus this many percentage points. Range 0-100. Default: 10 (decision D11, an owner proposal)."`
	MaxRevertPct     float64  `yaml:"max_revert_pct" doc:"G-M1 passes when at most this percent of the model's decided steps were reverted. Range 0-100. Default: 2 (decision D11, an owner proposal)."`
	FixtureDatabases []string `yaml:"fixture_databases" doc:"Database names whose reviews never count toward G-M1 or G-M2 (bench and game-day fixtures); databases named by a clone receipt never count either. Default: none."`
}

func defaultReviewGateConfig() ReviewGateConfig {
	return ReviewGateConfig{MinReviews: DefaultReviewGateMinReviews,
		MaxGapPoints: DefaultReviewGateMaxGapPoints,
		MaxRevertPct: DefaultReviewGateMaxRevertPct}
}

// validate refuses a negative, NaN or out-of-range threshold and an empty
// fixture name.
func (g ReviewGateConfig) validate() error {
	if g.MinReviews < 1 || g.MinReviews > maxReviewGateMinReviews {
		return fmt.Errorf("review.gate.min_reviews must be 1-%d, got %d",
			maxReviewGateMinReviews, g.MinReviews)
	}
	for _, v := range []struct {
		key   string
		value float64
	}{{"review.gate.max_gap_points", g.MaxGapPoints},
		{"review.gate.max_revert_pct", g.MaxRevertPct}} {
		if math.IsNaN(v.value) || v.value < 0 || v.value > 100 {
			return fmt.Errorf("%s must be 0-100, got %v", v.key, v.value)
		}
	}
	for i, name := range g.FixtureDatabases {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("review.gate.fixture_databases[%d] is empty; name a database",
				i)
		}
	}
	return nil
}
