package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/pg-sage/sidecar/internal/ask"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Intent error codes (mcp-08): an agent tells "retry later" (-32010)
// from "fix your request" (-32602); anything else stays an internal
// error whose detail never reaches the caller.

type failingPolicy struct {
	productionFixture
	err error
}

func (p *failingPolicy) GetPolicy(context.Context, PolicyRequest) (PolicyResult, error) {
	return PolicyResult{}, p.err
}

func productionServer(t *testing.T, mutate func(*ProductionDependencies)) *Server {
	t.Helper()
	dependencies := newProductionFixture().dependencies()
	if mutate != nil {
		mutate(&dependencies)
	}
	backend, err := NewProductionBackend(dependencies)
	require.NoError(t, err)
	return NewServer(backend)
}

func policyFailing(err error) func(*ProductionDependencies) {
	return func(d *ProductionDependencies) { d.Policy = &failingPolicy{err: err} }
}

func TestIntentFailureUnavailableTargetIsUnavailable(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("MCP target database: %w", ErrIntentUnavailable),
		ErrProductionDependencyUnavailable,
		fmt.Errorf("read policy: %w", ErrProductionDependencyUnavailable),
	} {
		server := productionServer(t, policyFailing(err))
		response := invoke(t, server, operatorContext(context.Background()),
			toolCall("get_policy", `{}`))
		require.Equal(t, codeUnavailable, response.Error.Code, err.Error())
		require.Contains(t, response.Error.Message, "unavailable", err.Error())
		require.Contains(t, response.Error.Message, "retry", err.Error())
	}
	require.ErrorIs(t, ErrProductionDependencyUnavailable, ErrIntentUnavailable)
	require.Equal(t, "MCP production dependency unavailable",
		ErrProductionDependencyUnavailable.Error())
}

func TestIntentFailureGenericBackendErrorIsInternalAndSanitized(t *testing.T) {
	server := productionServer(t, policyFailing(
		errors.New("dial tcp: password=hunter2 refused")))
	response := invoke(t, server, operatorContext(context.Background()),
		toolCall("get_policy", `{}`))
	require.Equal(t, codeInternal, response.Error.Code)
	require.Equal(t, "internal error", response.Error.Message)
}

func TestIntentFailurePlannerRefusalIsInvalidArguments(t *testing.T) {
	deterministic := func(d *ProductionDependencies) {
		d.Planner = DeterministicIntentPlanner{}
	}
	cases := map[string]string{
		`{"intent":{"kind":"drop_table","table":"public.t"}}`: "unsupported mutation intent",
		`{"intent":{"query_id":1}}`:                           "kind is required",
	}
	for args, want := range cases {
		response := invoke(t, productionServer(t, deterministic),
			operatorContext(context.Background()), toolCall("request_change", args))
		require.Equal(t, codeInvalidParams, response.Error.Code, args)
		require.Contains(t, response.Error.Message, want, args)
	}
}

func TestIntentFailurePlannerErrorsAreTypedInvalid(t *testing.T) {
	_, err := DeterministicIntentPlanner{}.Plan(context.Background(), "drop_table", nil)
	require.ErrorIs(t, err, ErrIntentInvalid)
	_, err = DeterministicIntentPlanner{}.Plan(context.Background(), "request_change",
		json.RawMessage(`{"kind":" "}`))
	require.ErrorIs(t, err, ErrIntentInvalid)
	backend, err := NewProductionBackend(func() ProductionDependencies {
		d := newProductionFixture().dependencies()
		d.Planner = DeterministicIntentPlanner{}
		return d
	}())
	require.NoError(t, err)
	_, err = backend.RequestChange(context.Background(), ChangeRequest{
		Intent: json.RawMessage(`{"kind":"vacuum_full"}`)})
	require.ErrorIs(t, err, ErrIntentInvalid)
}

func TestIntentFailureMaintenancePolicyWithoutPatchIsInvalid(t *testing.T) {
	response := invoke(t, productionServer(t, nil), operatorContext(context.Background()),
		toolCall("set_maintenance_policy", `{"scope":{}}`))
	require.Equal(t, codeInvalidParams, response.Error.Code, response.Error.Message)
	require.Contains(t, response.Error.Message, "patch is required")
}

func TestIntentFailureLedgerFilterOutOfRangeIsInvalid(t *testing.T) {
	for _, raw := range []string{`{"limit":0}`, `{"limit":1001}`, `{"limit":"ten"}`} {
		_, err := parseLedgerFilter(json.RawMessage(raw))
		require.ErrorIs(t, err, ErrIntentInvalid)
	}
	for _, raw := range []string{`{"limit":1}`, `{"limit":1000}`, `null`, ``} {
		filter, err := parseLedgerFilter(json.RawMessage(raw))
		require.NoError(t, err)
		require.True(t, filter.Limit >= 1 && filter.Limit <= 1000, raw)
	}
}

func TestIntentFailureCancellationStaysCancelled(t *testing.T) {
	server := productionServer(t, policyFailing(
		fmt.Errorf("read policy: %w", context.Canceled)))
	response := invoke(t, server, operatorContext(context.Background()),
		toolCall("get_policy", `{}`))
	require.Equal(t, codeCancelled, response.Error.Code)
}

func TestAskFailureUnavailableIsUnavailable(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{Actor: "user:2", Role: "operator"})
	backend := &askRecordingBackend{err: fmt.Errorf("%w: budget store down", ask.ErrUnavailable)}
	response := invoke(t, NewServer(backend), ctx, toolCall("ask_sage", askArgs))
	require.Equal(t, codeUnavailable, response.Error.Code)
	require.NotContains(t, response.Error.Message, "budget store down")
	generic := &askRecordingBackend{err: errors.New("db down")}
	response = invoke(t, NewServer(generic), ctx, toolCall("ask_sage", askArgs))
	require.Equal(t, codeInternal, response.Error.Code)
	require.NotContains(t, response.Error.Message, "db down")
}

// Non-intent families keep their codes: fleet_findings still reports a
// missing production dependency as an internal error.
func TestIntentFailureFleetFamilyCodeUnchanged(t *testing.T) {
	backend := &fleetRecordingBackend{err: ErrProductionDependencyUnavailable}
	response := invoke(t, NewServer(backend), context.Background(),
		toolCall("fleet_findings", `{}`))
	require.Equal(t, codeInternal, response.Error.Code)
}

// No concurrent-access test: the error mapping is a pure function of the
// error value. No state transitions: the mapping has no state.
