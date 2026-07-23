package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pg-sage/sidecar/internal/policy"
)

type DeterministicIntentPlanner struct{}

func (DeterministicIntentPlanner) Plan(
	_ context.Context, tool string, arguments json.RawMessage,
) (policy.ActionRequest, error) {
	feature, risk, err := intentContract(tool, arguments)
	if err != nil {
		return policy.ActionRequest{}, err
	}
	return policy.ActionRequest{
		Contract:        &policy.ActionContract{ActionType: tool, RiskTier: risk},
		Feature:         feature,
		Arguments:       append(json.RawMessage(nil), arguments...),
		TargetObjs:      intentTargets(arguments),
		InternalControl: tool == "declare_table_contract" || tool == "register_consumer",
	}, nil
}

type FailClosedIntentExecutor struct{}

func (FailClosedIntentExecutor) Execute(
	context.Context, policy.ActionRequest, policy.Decision,
) (any, error) {
	return nil, errors.New("intent has no verified executable plan")
}

func intentContract(
	tool string, arguments json.RawMessage,
) (string, policy.RiskTier, error) {
	if tool == "request_change" {
		var request struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(arguments, &request) != nil || strings.TrimSpace(request.Kind) == "" {
			return "", "", errors.New("change intent kind is required")
		}
		tool = request.Kind
	}
	switch tool {
	case "optimize_query":
		return "index", policy.RiskSafe, nil
	case "ensure_fk_indexes":
		return "fk_index", policy.RiskSafe, nil
	case "apply_migration":
		return "online_migration", policy.RiskModerate, nil
	case "declare_table_contract":
		return "retention", policy.RiskSafe, nil
	case "register_consumer":
		return "config_guc", policy.RiskSafe, nil
	default:
		return "", "", errors.New("unsupported mutation intent")
	}
}

func intentTargets(arguments json.RawMessage) []string {
	var request struct {
		Table    string `json:"table"`
		Schema   string `json:"schema"`
		SlotName string `json:"slot_name"`
	}
	if json.Unmarshal(arguments, &request) != nil {
		return nil
	}
	for _, target := range []string{request.Table, request.Schema, request.SlotName} {
		if strings.TrimSpace(target) != "" {
			return []string{target}
		}
	}
	return nil
}
