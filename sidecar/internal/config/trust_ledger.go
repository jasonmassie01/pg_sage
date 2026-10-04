package config

import "fmt"

// SREClassPromotionConfig is the promotion bar of self-initiated classes
// (roadmap 1.2, one trust system): index create/drop, GUC, reloption,
// vacuum, analyze, query hints and retention earn their level per
// database from verified outcomes. The time ramp (trust.ramp_*_hours) is
// the minimum observation before a promotion is proposed; an admin still
// approves every promotion, and irreversible classes never exceed L1.
type SREClassPromotionConfig struct {
	MinSuccessesL2    int     `yaml:"min_successes_l2" doc:"Verified successes since the class's last demerit to propose L2 for a self-initiated class (tuning: improved; hygiene: improved or held neutral), 1-1000. Default: 3." warning:"Lower values let pg_sage propose promotions after fewer verified outcomes."`
	MinSuccessesL3    int     `yaml:"min_successes_l3" doc:"Verified successes since the last demerit to propose L3 (unattended) for a self-initiated class, 1-10000, at least min_successes_l2. Default: 10." warning:"Lower values let pg_sage auto-execute after fewer verified outcomes."`
	MinSuccessRatePct float64 `yaml:"min_success_rate_pct" doc:"Share of decided outcomes since the last demerit that must be successes for L3, 50-100. Insufficient and unverifiable verdicts count neither way. Default: 80."`
}

func defaultSREClassPromotionConfig() SREClassPromotionConfig {
	return SREClassPromotionConfig{MinSuccessesL2: 3, MinSuccessesL3: 10,
		MinSuccessRatePct: 80}
}

func (p SREClassPromotionConfig) validate() error {
	const pre = "sre.autonomy.class_promotion."
	if p.MinSuccessesL2 < 1 || p.MinSuccessesL2 > 1000 {
		return fmt.Errorf(pre+"min_successes_l2 must be 1-1000, got %d", p.MinSuccessesL2)
	}
	if p.MinSuccessesL3 < 1 || p.MinSuccessesL3 > 10000 {
		return fmt.Errorf(pre+"min_successes_l3 must be 1-10000, got %d", p.MinSuccessesL3)
	}
	if p.MinSuccessesL3 < p.MinSuccessesL2 {
		return fmt.Errorf(pre+"min_successes_l3 (%d) must be at least min_successes_l2 (%d)",
			p.MinSuccessesL3, p.MinSuccessesL2)
	}
	// The negated form also rejects NaN.
	if !(p.MinSuccessRatePct >= minAccuracyPct && p.MinSuccessRatePct <= 100) {
		return fmt.Errorf(pre+"min_success_rate_pct must be %d-100, got %v",
			minAccuracyPct, p.MinSuccessRatePct)
	}
	return nil
}

// loweredClassPromotion lists the class promotion settings below the
// spec (fast elevation).
func loweredClassPromotion(p SREClassPromotionConfig,
	add func(key string, value, def float64, unit string)) {
	def := defaultSREClassPromotionConfig()
	const pre = "sre.autonomy.class_promotion."
	add(pre+"min_successes_l2", float64(p.MinSuccessesL2), float64(def.MinSuccessesL2),
		"actions")
	add(pre+"min_successes_l3", float64(p.MinSuccessesL3), float64(def.MinSuccessesL3),
		"actions")
	add(pre+"min_success_rate_pct", p.MinSuccessRatePct, def.MinSuccessRatePct, "percent")
}
