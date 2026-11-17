package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/pg-sage/sidecar/internal/changeplan"
)

// propose_plan's input (plan-03b). The plan is a JSON object, or a string
// holding exactly one JSON object (models often send their JSON as text).
// The input is strict: markdown fences, text around the JSON, unknown
// fields and the fields pg_sage sets (status, origin, the hash, ids) are
// refused, each with its plan_code, and nothing is stored.

// planProposal is what a proposer may set on a plan.
type planProposal struct {
	Goal     string            `json:"goal"`
	Scope    changeplan.Scope  `json:"scope"`
	PackHash string            `json:"pack_hash"`
	Steps    []changeplan.Step `json:"steps"`
	Edges    []changeplan.Edge `json:"edges"`
}

// planInputError is a refused plan argument with its plan_code.
type planInputError struct{ code, message string }

func (e *planInputError) Error() string { return e.message }

func refusePlanInput(code, message string) *planInputError {
	return &planInputError{code: code, message: "invalid plan: " + message}
}

// decodeProposal reads the plan argument.
func decodeProposal(raw json.RawMessage) (planProposal, *planInputError) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return planProposal{}, refusePlanInput("plan_empty", "the plan is required")
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return planProposal{}, refusePlanInput("plan_malformed", "the plan string is "+
				"not valid JSON text")
		}
		text = strings.TrimSpace(text)
		if strings.HasPrefix(text, "```") {
			return planProposal{}, refusePlanInput("plan_fenced", "send the plan JSON "+
				"itself, without markdown fences")
		}
		raw = json.RawMessage(text)
		if len(raw) == 0 {
			return planProposal{}, refusePlanInput("plan_empty", "the plan is empty")
		}
	}
	if raw[0] != '{' {
		return planProposal{}, refusePlanInput("plan_malformed", "the plan must be one "+
			"JSON object")
	}
	if string(bytes.Join(bytes.Fields(raw), nil)) == "{}" {
		return planProposal{}, refusePlanInput("plan_empty", "the plan has no content")
	}
	var p planProposal
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return planProposal{}, refusePlanInput("plan_malformed", "the plan is not one "+
			"JSON object of goal, scope, pack_hash, steps and edges")
	}
	if len(bytes.TrimSpace(raw[dec.InputOffset():])) > 0 {
		return planProposal{}, refusePlanInput("plan_malformed", "text follows the plan "+
			"JSON")
	}
	return p, nil
}

// proposer is who proposes over MCP: an agent (its principal id, else
// its token or actor) or a person.
func proposer(ctx context.Context) (changeplan.Origin, string) {
	p, _ := PrincipalFromContext(ctx)
	if p.Kind == KindAgent {
		for _, ref := range []string{p.PrincipalID, p.TokenID, p.Actor} {
			if ref = strings.TrimSpace(ref); ref != "" {
				return changeplan.OriginAgent, ref
			}
		}
	}
	return changeplan.OriginPerson, strings.TrimSpace(p.Actor)
}

func proposePlan(ctx context.Context, backend PlanBackend, args planArgs) (any, *rpcError) {
	in, refused := decodeProposal(args.Plan)
	if refused != nil {
		return planRefusal(codeInvalidParams, refused.code, refused.message, nil), nil
	}
	origin, ref := proposer(ctx)
	p := changeplan.Plan{Goal: in.Goal, Scope: in.Scope, PackHash: in.PackHash,
		Steps: in.Steps, Edges: in.Edges, Origin: origin, OriginRef: ref,
		Status: changeplan.StatusProposed, CreatedBy: ActorFromContext(ctx)}
	stored, err := backend.ProposePlan(ctx, args.Database, p,
		changeplan.DefaultOpenPlansPerPrincipal)
	if err != nil {
		return planFailure(ctx, "propose_plan", err)
	}
	return toolSuccess(map[string]any{"database": args.Database, "plan_id": stored.ID,
		"status": stored.Status, "plan_hash": stored.PlanHash, "origin": stored.Origin,
		"origin_ref": stored.OriginRef, "steps": len(stored.Steps),
		"note": "proposed: nothing runs until a person approves it in pg_sage (the app, " +
			"the CLI or a chat card); agents can never approve a plan"}), nil
}
