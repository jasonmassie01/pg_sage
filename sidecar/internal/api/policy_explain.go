package api

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/cases"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
)

// GET /api/v1/policy/explain?database=&finding_id= projects one finding
// into its case candidates and asks the standing gate how it would decide
// each, through the cases view's explain path (enrichCasesActionPolicies,
// the executor's ExplainActions since cases-04). Explaining records no
// decision and takes no budget slot (policy-07).

// Explain statuses.
const (
	explainExplained   = "explained"
	explainNoCandidate = "no_candidate"
)

// explainedCandidate is one candidate's verdict as served; no SQL text.
type explainedCandidate struct {
	ActionType                string   `json:"action_type"`
	RiskTier                  string   `json:"risk_tier"`
	Decision                  string   `json:"decision"`
	BlockedReason             string   `json:"blocked_reason,omitempty"`
	Detail                    string   `json:"detail,omitempty"`
	HumanReason               string   `json:"human_reason"`
	RequiresApproval          bool     `json:"requires_approval"`
	RequiresMaintenanceWindow bool     `json:"requires_maintenance_window"`
	Guardrails                []string `json:"guardrails"`
	Provider                  string   `json:"provider,omitempty"`
}

type explainResponse struct {
	Database   string               `json:"database"`
	FindingID  string               `json:"finding_id"`
	Status     string               `json:"status"`
	Candidates []explainedCandidate `json:"candidates"`
}

func policyExplainHandler(mgr *fleet.DatabaseManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		dbName, ok := readDatabaseParam(w, r)
		if !ok {
			return
		}
		findingID, err := strconv.ParseInt(r.URL.Query().Get("finding_id"), 10, 64)
		if err != nil || findingID <= 0 {
			jsonError(w, "finding_id must be a positive integer", http.StatusBadRequest)
			return
		}
		selected, ok := resolveSingleDatabaseRequestPool(mgr, dbName)
		if !ok {
			jsonError(w, "database is required", http.StatusBadRequest)
			return
		}
		if selected.pool == nil {
			jsonError(w, "database not found", http.StatusNotFound)
			return
		}
		id := strconv.FormatInt(findingID, 10)
		row, err := queryFindingByID(r.Context(), selected.pool, id)
		if errors.Is(err, pgx.ErrNoRows) {
			jsonError(w, "finding not found", http.StatusNotFound)
			return
		}
		if err != nil {
			slog.Warn("policy explain: finding read failed", "database", selected.name,
				"finding_id", id, "error", err)
			jsonError(w, "finding could not be read", http.StatusServiceUnavailable)
			return
		}
		row["database_name"] = selected.name
		out := explainResponse{Database: selected.name, FindingID: id,
			Candidates: explainFinding(r, row, mgr.GetInstance(selected.name))}
		out.Status = explainExplained
		if len(out.Candidates) == 0 {
			out.Status = explainNoCandidate
		}
		writeJSONStatus(w, http.StatusOK, out)
	}
}

// explainFinding projects the finding's case and explains its candidates;
// none when the finding has no candidate (or is pg_sage's own).
func explainFinding(
	r *http.Request, row map[string]any, inst *fleet.DatabaseInstance,
) []explainedCandidate {
	projected, _ := projectFindingRows([]map[string]any{row})
	out := []explainedCandidate{}
	if len(projected) == 0 || len(projected[0].ActionCandidates) == 0 {
		return out
	}
	details := enrichCasesActionPolicies(r.Context(), projected, inst)
	for j, candidate := range projected[0].ActionCandidates {
		out = append(out, explainedFrom(candidate, details[0][j]))
	}
	return out
}

func explainedFrom(candidate cases.ActionCandidate, detail string) explainedCandidate {
	out := explainedCandidate{ActionType: candidate.ActionType, RiskTier: candidate.RiskTier,
		BlockedReason: candidate.BlockedReason, Guardrails: []string{}}
	if decision := candidate.PolicyDecision; decision != nil {
		out.Decision, out.RiskTier = decision.Decision, decision.RiskTier
		out.RequiresApproval = decision.RequiresApproval
		out.RequiresMaintenanceWindow = decision.RequiresMaintenanceWindow
		out.Provider = decision.Provider
		out.Guardrails = append(out.Guardrails, decision.Guardrails...)
		if out.BlockedReason == "" { // an unknown type or provider refusal wins
			out.BlockedReason = decision.BlockedReason
		}
		if out.BlockedReason == decision.BlockedReason { // the detail is the gate's
			out.Detail = detail
		}
	}
	if out.Decision == "" {
		out.Decision = executor.PolicyDecisionBlocked
	}
	out.HumanReason = executor.HumanDecisionReason(out.BlockedReason, out.Detail)
	return out
}
