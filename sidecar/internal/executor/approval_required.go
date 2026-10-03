package executor

import (
	"fmt"
	"slices"
	"strings"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/policy"
)

// approvalRequiredReason is why a background finding may run only with an
// operator's approval ("" = it may run unattended when policy allows):
// the producer marked it (e.g. an index drop while standby index usage is
// unknown), or it changes a GUC that needs a restart or is not on the
// autonomous allowlist (G-P0-1, G-P0-12).
func approvalRequiredReason(f analyzer.Finding) string {
	switch v := f.Detail[analyzer.DetailApprovalRequired].(type) {
	case nil:
	case string:
		if strings.TrimSpace(v) != "" {
			return v
		}
	case bool:
		if v {
			return "the finding is marked approval_required"
		}
	default:
		return fmt.Sprintf("the finding is marked approval_required (%v)", v)
	}
	stmt, ok := pgconf.ParseAlterSystem(f.RecommendedSQL)
	switch {
	case !ok:
		return ""
	case pgconf.RequiresRestart(stmt.Name):
		return stmt.Name + " takes effect only after a restart"
	case !pgconf.AutonomousGUC(stmt.Name):
		return stmt.Name + " is not on the autonomous configuration allowlist"
	}
	return ""
}

// requireApproval adds the approval guardrail to a finding's request when
// the finding may not run unattended.
func requireApproval(request *policy.ActionRequest, f analyzer.Finding) {
	if request.Contract == nil || approvalRequiredReason(f) == "" ||
		slices.Contains(request.Contract.Guardrails, policy.GuardrailApprovalRequired) {
		return
	}
	request.Contract.Guardrails = append(request.Contract.Guardrails,
		policy.GuardrailApprovalRequired)
}
