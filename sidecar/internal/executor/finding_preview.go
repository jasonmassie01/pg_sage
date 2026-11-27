package executor

import (
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
)

// ContractTierNone is the contract tier of SQL the executor has no action
// contract for (or no SQL at all): the finding is advisory only.
const ContractTierNone = "none"

// FindingPreview is, for display, how the executor would treat a finding's
// recommended SQL. It never gates execution: the executor re-derives all of
// it, under policy, when an action is requested.
type FindingPreview struct {
	ActionType       string // "" when the SQL has no contract
	ContractTier     string // the contract's base risk tier, or ContractTierNone
	ApprovalRequired bool
	ApprovalReason   string
}

// FindingActionPreview classifies sql the way the executor does, names the
// action contract's base risk tier and says why the finding may run only
// with an operator's approval (a producer mark or a guarded GUC change).
func FindingActionPreview(sql string, detail map[string]any) FindingPreview {
	p := FindingPreview{ContractTier: ContractTierNone}
	if strings.TrimSpace(sql) == "" {
		return p
	}
	if actionType := ActionTypeForSQL(sql); actionType != "" {
		if c, ok := ContractForActionType(actionType); ok {
			p.ActionType, p.ContractTier = actionType, c.BaseRiskTier
		}
	}
	p.ApprovalReason = approvalRequiredReason(
		analyzer.Finding{RecommendedSQL: sql, Detail: detail})
	p.ApprovalRequired = p.ApprovalReason != ""
	return p
}
