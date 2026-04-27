package cases

import "sort"

type ShadowReport struct {
	TotalCases        int      `json:"total_cases"`
	WouldAutoResolve  int      `json:"would_auto_resolve"`
	RequiresApproval  int      `json:"requires_approval"`
	Blocked           int      `json:"blocked"`
	EstimatedToilMins int      `json:"estimated_toil_minutes"`
	BlockedReasons    []string `json:"blocked_reasons"`
}

func BuildShadowReport(cases []Case) ShadowReport {
	report := ShadowReport{TotalCases: len(cases)}
	reasons := map[string]bool{}

	for _, c := range cases {
		for _, a := range c.ActionCandidates {
			switch {
			case a.RiskTier == "safe" && a.BlockedReason == "":
				report.WouldAutoResolve++
				report.EstimatedToilMins += estimatedToilForAction(a.ActionType)
			default:
				report.RequiresApproval++
				if a.BlockedReason != "" {
					report.Blocked++
					reasons[a.BlockedReason] = true
				}
			}
		}
	}

	report.BlockedReasons = sortedReasons(reasons)
	return report
}

func estimatedToilForAction(actionType string) int {
	if actionType == "analyze_table" {
		return 15
	}
	return 30
}

func sortedReasons(reasons map[string]bool) []string {
	out := make([]string, 0, len(reasons))
	for reason := range reasons {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}
