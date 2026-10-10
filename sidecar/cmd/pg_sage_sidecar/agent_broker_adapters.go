package main

import (
	"context"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/broker"
	"github.com/pg-sage/sidecar/internal/crypto"
)

// agentLogins opens broker credentials with the core's store.
type agentLogins struct {
	store *agentguard.Store
	kr    *crypto.Keyring
}

func (a agentLogins) BrokerLogin(ctx context.Context, principalID, clusterKey string) (
	string, string, error) {
	return a.store.BrokerCredential(ctx, a.kr, principalID, clusterKey)
}

// agentBrokerMCP serves agent_query and agent_whoami to the MCP backend.
type agentBrokerMCP struct{}

func (agentBrokerMCP) AgentQuery(ctx context.Context, req broker.Request) (broker.Result,
	error) {
	rt, err := processAgentBroker()
	if err != nil {
		return broker.Result{}, err
	}
	return rt.broker.Query(ctx, req)
}

func (agentBrokerMCP) AgentWhoAmI(ctx context.Context) (broker.WhoAmI, error) {
	rt, err := processAgentBroker()
	if err != nil {
		return broker.WhoAmI{}, err
	}
	return rt.broker.WhoAmI(ctx)
}

// agentActivityAPI serves GET /api/v1/agents/{id}/activity.
type agentActivityAPI struct{}

func (agentActivityAPI) AgentActivity(ctx context.Context, principalID, database string,
	req broker.ActivityRequest) ([]broker.Activity, error) {
	rt, err := processAgentBroker()
	if err != nil {
		return nil, err
	}
	if _, err := rt.store.Get(ctx, principalID); err != nil {
		return nil, err
	}
	names := []string{database}
	if database == "" {
		names = fleetNames(fleetMgr)
	}
	targets := agentTargets{svc: agentEnvSvc}
	out := []broker.Activity{}
	for _, name := range names {
		t, err := targets.Target(ctx, name)
		if err != nil {
			return nil, err
		}
		if t.DatabaseID == "" {
			continue // unbound: nothing can be attributed to it yet
		}
		a, err := broker.ReadActivity(ctx, t, req)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
