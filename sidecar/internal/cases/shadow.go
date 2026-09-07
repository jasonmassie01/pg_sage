package cases

import (
	"sort"
	"time"
)

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
	ActionID          string `json:"action_id,omitempty"`
	CaseID            string `json:"case_id"`
	Title             string `json:"title"`
	ActionType        string `json:"action_type"`
	PolicyDecision    string `json:"policy_decision"`
	LifecycleState    string `json:"lifecycle_state,omitempty"`
	Status            string `json:"status,omitempty"`
	Verification      string `json:"verification_status,omitempty"`
	ProposedAt        string `json:"proposed_at,omitempty"`
	EstimatedToilMins int    `json:"estimated_toil_minutes"`
	BlockedReason     string `json:"blocked_reason,omitempty"`
}

func BuildShadowReport(cases []Case) ShadowReport {
	report := ShadowReport{TotalCases: len(cases)}
	reasons := map[string]bool{}

	for _, c := range cases {
		if len(c.Actions) > 0 {
			addActionProof(&report, reasons, c)
			continue
		}
		for _, a := range c.ActionCandidates {
			toil := estimatedToilForAction(a.ActionType, 0)
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

func addActionProof(
	report *ShadowReport,
	reasons map[string]bool,
	c Case,
) {
	for _, a := range c.Actions {
		toil := estimatedToilForAction(a.Type, a.ShadowToilMinutes)
		policy := actionHistoryPolicyDecision(a)
		proof := ShadowProofItem{
			ActionID:          a.ID,
			CaseID:            c.ID,
			Title:             c.Title,
			ActionType:        a.Type,
			PolicyDecision:    policy,
			LifecycleState:    a.LifecycleState,
			Status:            a.Status,
			Verification:      a.VerificationStatus,
			ProposedAt:        shadowProofTime(a.ProposedAt),
			EstimatedToilMins: toil,
			BlockedReason:     a.BlockedReason,
		}
		switch {
		case actionHistoryWouldAutoResolve(a, policy):
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

func actionHistoryPolicyDecision(action CaseAction) string {
	if action.PolicyDecision != "" {
		return action.PolicyDecision
	}
	if action.BlockedReason != "" {
		return "blocked"
	}
	if action.Status == "success" || action.LifecycleState == "executed" {
		return "execute"
	}
	return "queue_for_approval"
}

func actionHistoryWouldAutoResolve(action CaseAction, policy string) bool {
	return policy == "execute" &&
		action.BlockedReason == "" &&
		(action.RiskTier == "safe" || action.Type == "analyze" ||
			action.Type == "analyze_table")
}

func estimatedToilForAction(actionType string, override int) int {
	if override > 0 {
		return override
	}
	if actionType == "analyze_table" || actionType == "analyze" {
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

func shadowProofTime(value *time.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}
