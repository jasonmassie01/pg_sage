package config

import "fmt"

// DefaultRetentionDecisionsDays keeps non-execute decisions a month after
// they were last seen (perf audit F1: at 365 days the ledger would hold
// hundreds of millions of withheld verdicts).
const DefaultRetentionDecisionsDays = 30

const maxRetentionDecisionsDays = 3650

// validateDecisionsDays refuses a window outside 0-3650 (0 disables).
func (r RetentionConfig) validateDecisionsDays() error {
	if r.DecisionsDays < 0 || r.DecisionsDays > maxRetentionDecisionsDays {
		return fmt.Errorf("retention.decisions_days must be 0-%d, got %d",
			maxRetentionDecisionsDays, r.DecisionsDays)
	}
	return nil
}
