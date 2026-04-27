package cases

import "sort"

type ShadowReport struct {
	TotalCases        int               `json:"total_cases"`
	WouldAutoResolve  int               `json:"would_auto_resolve"`
	RequiresApproval  int               `json:"requires_approval"`
	Blocked           int               `json:"blocked"`
	EstimatedToilMins int               `json:"estimated_toil_minutes"`
	BlockedReasons    []string          `json:"blocked_reasons"`
	Proof             []ShadowProofItem `json:"proof"`
}

type ShadowProofItem struct {
	CaseID            string `json:"case_id"`
	Title             string `json:"title"`
	ActionType        string `json:"action_type"`
	PolicyDecision    string `json:"policy_decision"`
	EstimatedToilMins int    `json:"estimated_toil_minutes"`
	BlockedReason     string `json:"blocked_reason,omitempty"`
}

func BuildShadowReport(cases []Case) ShadowReport {
	report := ShadowReport{TotalCases: len(cases)}
	reasons := map[string]bool{}

	for _, c := range cases {
		for _, a := range c.ActionCandidates {
			toil := estimatedToilForAction(a.ActionType)
			proof := ShadowProofItem{
				CaseID:            c.ID,
				Title:             c.Title,
				ActionType:        a.ActionType,
				PolicyDecision:    actionPolicyDecision(a),
				EstimatedToilMins: toil,
				BlockedReason:     a.BlockedReason,
			}
			switch {
			case a.RiskTier == "safe" && a.BlockedReason == "":
				report.WouldAutoResolve++
				report.EstimatedToilMins += toil
			default:
				report.RequiresApproval++
				if a.BlockedReason != "" {
					report.Blocked++
					reasons[a.BlockedReason] = true
				}
			}
			report.Proof = append(report.Proof, proof)
		}
	}

	report.BlockedReasons = sortedReasons(reasons)
	return report
}

func actionPolicyDecision(action ActionCandidate) string {
	if action.PolicyDecision != nil && action.PolicyDecision.Decision != "" {
		return action.PolicyDecision.Decision
	}
	if action.BlockedReason != "" {
		return "blocked"
	}
	if action.RiskTier == "safe" {
		return "execute"
	}
	return "queue_for_approval"
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
