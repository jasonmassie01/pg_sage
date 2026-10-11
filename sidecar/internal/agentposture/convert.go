package agentposture

import "github.com/pg-sage/sidecar/internal/analyzer"

// AnalyzerFinding is f as a sage.findings row. It never carries
// RecommendedSQL or RollbackSQL: the analyzer turns only findings with
// RecommendedSQL into recommendations and the executor acts only on
// those, so a posture finding stays a manual script (L1, G0-05). The fix
// is kept in the detail as manual_script.
func (f Finding) AnalyzerFinding() analyzer.Finding {
	detail := map[string]any{
		"detector":       f.Detector,
		"section":        Section,
		"proposal_level": ProposalLevel,
	}
	for key, v := range map[string]string{"detail": f.Detail, "manual_script": f.FixScript,
		"caveat": f.Caveat} {
		if v != "" {
			detail[key] = v
		}
	}
	if len(f.Evidence) > 0 {
		detail["evidence"] = f.Evidence
	}
	return analyzer.Finding{
		Category:         f.Category(),
		Severity:         string(f.Severity),
		ObjectType:       f.ObjectType,
		ObjectIdentifier: f.Object,
		Title:            f.Title,
		Detail:           detail,
		Recommendation:   f.Recommendation,
		RuleID:           f.Detector,
	}
}
