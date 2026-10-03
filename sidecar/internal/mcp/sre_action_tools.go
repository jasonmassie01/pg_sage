package mcp

import (
	"context"
	"encoding/json"
	"errors"

	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
)

// Sage SRE action tools (AI-SRE-SPEC §9, CHECK-39). They need an
// operator or admin principal, take typed ids only (never SQL), and never
// execute: sre_propose_action derives a proposal with its repair
// contract and policy verdict, sre_request_execution queues exactly one
// item in the existing approval flow, where a human approves it.

var sreActionToolNames = map[string]bool{"sre_propose_action": true,
	"sre_request_execution": true}

// SREActionRequest identifies an investigation or a proposal.
type SREActionRequest struct {
	Database        string `json:"database,omitempty"`
	InvestigationID string `json:"investigation_id,omitempty"`
	ProposalID      string `json:"proposal_id,omitempty"`
}

// SREActionBackend serves the Sage SRE action tools.
type SREActionBackend interface {
	ProposeAction(context.Context, SREActionRequest) (any, error)
	RequestExecution(context.Context, SREActionRequest) (any, error)
}

func sreActionTools() []Tool {
	schema := func(properties, required string) json.RawMessage {
		return json.RawMessage(`{"type":"object","properties":` + properties +
			`,"required":["` + required + `"],"additionalProperties":false}`)
	}
	db := `"database":{"type":"string","description":"fleet database name"}`
	return []Tool{
		{Name: "sre_propose_action", Description: "Propose the evidence-matched " +
			"mitigation of a concluded investigation (cancel of the one root " +
			"backend) with its repair contract and policy verdict, or why there is " +
			"none. Never executes anything.",
			InputSchema: schema(`{`+db+`,"investigation_id":{"type":"string",`+
				`"format":"uuid"}}`, "investigation_id")},
		{Name: "sre_request_execution", Description: "Queue exactly one approval " +
			"item for a proposal in the existing approval flow. Never executes: a " +
			"human approves, then pg_sage rechecks the target and runs it.",
			InputSchema: schema(`{`+db+`,"proposal_id":{"type":"string",`+
				`"format":"uuid"}}`, "proposal_id")},
	}
}

func (s *Server) callSREActionTool(ctx context.Context, name string,
	raw json.RawMessage) (any, *rpcError) {
	backend, ok := s.backend.(SREActionBackend)
	if !ok {
		return nil, failure(-32603, "actions unavailable")
	}
	var req SREActionRequest
	if !decodeStrict(raw, &req) {
		return nil, failure(-32602, "invalid arguments")
	}
	var result any
	var err error
	switch {
	case name == "sre_propose_action" && req.InvestigationID != "" && req.ProposalID == "":
		result, err = backend.ProposeAction(ctx, req)
	case name == "sre_request_execution" && req.ProposalID != "" &&
		req.InvestigationID == "":
		result, err = backend.RequestExecution(ctx, req)
	default:
		return nil, failure(-32602, "invalid arguments")
	}
	if err != nil {
		return nil, sreActionFailure(err)
	}
	return toolSuccess(result), nil
}

func sreActionFailure(err error) *rpcError {
	switch {
	case errors.Is(err, sreaction.ErrProposalNotFound):
		return failure(-32004, "not found")
	case errors.Is(err, sreaction.ErrProposalState):
		return failure(-32009, "the proposal is not in a state that allows this")
	case errors.Is(err, sreaction.ErrPolicyBlocked):
		return failure(-32010, "policy withholds this action")
	case errors.Is(err, sreaction.ErrHandoffBlocked):
		return failure(-32603, "investigation metadata is degraded; nothing is handed off")
	}
	return sreFailure(err)
}

// ProposeAction serves sre_propose_action.
func (backend *ProductionBackend) ProposeAction(ctx context.Context,
	request SREActionRequest) (any, error) {
	if backend.dependencies.Actions == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Actions.ProposeAction(ctx, request)
}

// RequestExecution serves sre_request_execution.
func (backend *ProductionBackend) RequestExecution(ctx context.Context,
	request SREActionRequest) (any, error) {
	if backend.dependencies.Actions == nil {
		return nil, ErrProductionDependencyUnavailable
	}
	return backend.dependencies.Actions.RequestExecution(ctx, request)
}
