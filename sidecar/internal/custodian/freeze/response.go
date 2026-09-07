package freeze

import (
	"fmt"
	"strings"
)

type ResponseKind string

const (
	ResponseCancelBlocker    ResponseKind = "cancel_xmin_blocker"
	ResponseTerminateBlocker ResponseKind = "terminate_xmin_blocker"
	ResponseVacuumFreeze     ResponseKind = "vacuum_freeze"
	ResponseTuneAutovacuum   ResponseKind = "tune_autovacuum"
	ResponsePlanRepack       ResponseKind = "plan_pg_repack"
	ResponseParkBloat        ResponseKind = "park_bloat"
)

type XminBlocker struct {
	PID     int
	XminAge int64
	User    string
	State   string
	Query   string
}

type ResponseInput struct {
	Schema            string
	Table             string
	Urgency           Urgency
	Blocker           *XminBlocker
	DeadTupleRatio    float64
	BloatRatio        float64
	PGRepackAvailable bool
}

type Response struct {
	Kind     ResponseKind
	SQL      string
	Plan     string
	Park     bool
	Evidence map[string]any
}

func PlanResponse(input ResponseInput) (Response, error) {
	if err := validateResponseInput(input); err != nil {
		return Response{}, err
	}
	if input.Blocker != nil {
		return blockerResponse(*input.Blocker), nil
	}
	if input.Urgency == UrgencyRed {
		return Response{Kind: ResponseVacuumFreeze,
			SQL:      "VACUUM (FREEZE) " + quoteIdent(input.Schema) + "." + quoteIdent(input.Table),
			Evidence: responseEvidence(input)}, nil
	}
	if input.Urgency == UrgencyAmber && input.DeadTupleRatio >= 0.20 {
		return Response{Kind: ResponseTuneAutovacuum,
			SQL: "ALTER TABLE " + quoteIdent(input.Schema) + "." + quoteIdent(input.Table) +
				" SET (autovacuum_vacuum_scale_factor = 0.02)",
			Evidence: responseEvidence(input)}, nil
	}
	if input.BloatRatio >= 0.50 {
		return bloatResponse(input), nil
	}
	return Response{}, nil
}

func validateResponseInput(input ResponseInput) error {
	if strings.TrimSpace(input.Schema) == "" || strings.TrimSpace(input.Table) == "" {
		return fmt.Errorf("schema-qualified freeze target is required")
	}
	if input.Blocker != nil && input.Blocker.PID <= 0 {
		return fmt.Errorf("xmin blocker PID must be positive")
	}
	if input.DeadTupleRatio < 0 || input.DeadTupleRatio > 1 ||
		input.BloatRatio < 0 || input.BloatRatio > 1 {
		return fmt.Errorf("freeze ratios must be within zero and one")
	}
	return nil
}

func blockerResponse(blocker XminBlocker) Response {
	kind := ResponseCancelBlocker
	function := "pg_cancel_backend"
	if strings.EqualFold(strings.TrimSpace(blocker.State), "idle in transaction") {
		kind = ResponseTerminateBlocker
		function = "pg_terminate_backend"
	}
	return Response{Kind: kind,
		SQL: fmt.Sprintf("SELECT %s(%d)", function, blocker.PID),
		Evidence: map[string]any{"pid": blocker.PID, "xmin_age": blocker.XminAge,
			"user": blocker.User, "state": blocker.State, "query": blocker.Query}}
}

func bloatResponse(input ResponseInput) Response {
	response := Response{Park: true, Evidence: responseEvidence(input)}
	if input.PGRepackAvailable {
		response.Kind = ResponsePlanRepack
		response.Plan = "Plan pg_repack --table=" + input.Schema + "." + input.Table +
			" after disk and lock-window validation"
		return response
	}
	response.Kind = ResponseParkBloat
	response.Plan = "Park bloat remediation until an online rebuild provider is available"
	return response
}

func responseEvidence(input ResponseInput) map[string]any {
	return map[string]any{"dead_tuple_ratio": input.DeadTupleRatio,
		"bloat_ratio": input.BloatRatio, "pg_repack_available": input.PGRepackAvailable}
}
