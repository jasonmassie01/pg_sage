package api

import (
	"fmt"
	"maps"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/findingcat"
)

// reservedSource is a source filter value that matches no category yet.
const reservedSource = "migration_advisor"

// buildSourceClause converts a subsystem filter value into a WHERE
// fragment + args from the finding category registry: a LIKE per prefix
// family of the subsystem and one ANY list of its other categories.
// Returns ok=false when the value is empty or unrecognised (caller must
// then emit nothing).
func buildSourceClause(
	source string, startN int,
) (string, []any, bool) {
	if source == reservedSource {
		return " AND 1=0", nil, true // reserved, emits nothing
	}
	entries := findingcat.BySubsystem(source)
	if len(entries) == 0 {
		return "", nil, false
	}
	var prefixes, exact []string
	for _, c := range entries {
		if c.Prefix {
			prefixes = append(prefixes, c.Name)
		}
	}
	for _, c := range entries {
		if !c.Prefix && !hasAnyPrefix(c.Name, prefixes) {
			exact = append(exact, c.Name)
		}
	}
	var parts []string
	var args []any
	for _, p := range prefixes {
		parts = append(parts, fmt.Sprintf("category LIKE $%d", startN+len(args)))
		args = append(args, p+"%")
	}
	if len(exact) > 0 {
		parts = append(parts, fmt.Sprintf("category = ANY($%d::text[])", startN+len(args)))
		args = append(args, exact)
	}
	if len(parts) == 1 {
		return " AND " + parts[0], args, true
	}
	return " AND (" + strings.Join(parts, " OR ") + ")", args, true
}

func hasAnyPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// subsystemFromCategory maps a category value to the subsystem the
// source filter shows it under, from the finding category registry; an
// unregistered category maps to "".
func subsystemFromCategory(category string) string {
	if c, ok := findingcat.Lookup(category); ok {
		return c.Subsystem
	}
	return ""
}

// riskRank orders the risk tiers a finding row can report.
var riskRank = map[string]int{"none": 0, "read_only": 1, "safe": 2, "moderate": 3, "high": 4}

// highRiskApproval is the approval reason of a high-risk row that has no
// more specific one: the policy gate always queues high risk for approval.
const highRiskApproval = "high-risk actions always need an operator's approval"

// normalizeActionRisk maps a producer's ActionRisk to a riskRank tier:
// "" is none, high_risk is high, and an unrecognised label fails closed
// to high, as the policy gate treats an unknown tier.
func normalizeActionRisk(risk string) string {
	risk = strings.ToLower(strings.TrimSpace(risk))
	switch risk {
	case "":
		return "none"
	case "high_risk":
		return "high"
	}
	if _, ok := riskRank[risk]; ok {
		return risk
	}
	return "high"
}

// withFindingRisk adds findingRiskFields to a finding row and returns it.
func withFindingRisk(row map[string]any, recommendedSQL string, detail any) map[string]any {
	maps.Copy(row, findingRiskFields(recommendedSQL, detail))
	return row
}

// findingRiskFields are a finding row's display-only action fields: the
// executor's action type and contract tier for its SQL, the stricter of
// that tier and the producer's detail.action_risk, and whether (and why)
// it needs an operator's approval. Empty SQL is advisory: tier none.
func findingRiskFields(recommendedSQL string, detail any) map[string]any {
	detailMap, _ := detail.(map[string]any)
	p := executor.FindingActionPreview(recommendedSQL, detailMap)
	risk := "none"
	if strings.TrimSpace(recommendedSQL) != "" {
		producer, _ := detailMap[analyzer.DetailActionRisk].(string)
		risk = normalizeActionRisk(producer)
		if contract := normalizeActionRisk(p.ContractTier); riskRank[contract] > riskRank[risk] {
			risk = contract
		}
	}
	if risk == "high" && !p.ApprovalRequired {
		p.ApprovalRequired, p.ApprovalReason = true, highRiskApproval
	}
	return map[string]any{
		"action_type":       p.ActionType,
		"contract_tier":     p.ContractTier,
		"action_risk":       risk,
		"approval_required": p.ApprovalRequired,
		"approval_reason":   p.ApprovalReason,
	}
}
