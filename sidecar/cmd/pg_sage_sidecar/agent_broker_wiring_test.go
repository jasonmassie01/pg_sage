package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard/broker"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/config"
)

// Default value masking: the broker takes its bounds from agents.query,
// agents.roles and agents.broker, never from zero values.
func TestAgentBrokerConfigMapsSettings(t *testing.T) {
	c := config.DefaultConfig()
	got := agentBrokerConfig(c)
	if got.MaxRows != 200 || got.MaxRowsCeiling != 1000 || got.MaxBytes != 1<<20 {
		t.Errorf("row/byte bounds = %+v", got)
	}
	if got.StatementTimeout != 30*time.Second || got.PoolMaxConns != 2 ||
		got.MaxTotalConns != 20 || got.PoolIdle != time.Minute {
		t.Errorf("timeouts/pool = %+v", got)
	}
	c.Agents.Query.MaxRows, c.Agents.Roles.StatementTimeoutMS = 7, 1500
	got = agentBrokerConfig(c)
	if got.MaxRows != 7 || got.StatementTimeout != 1500*time.Millisecond {
		t.Errorf("overrides not applied: %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("mapped config invalid: %v", err)
	}
}

// Before governance starts (no control database) every agent read is
// refused, never allowed.
func TestSharedDeciderFailsClosedWithoutGovernance(t *testing.T) {
	prev := agentGate.Load()
	agentGate.Store(nil)
	t.Cleanup(func() { agentGate.Store(prev) })
	v := sharedDecider{}.Decide(context.Background(), decide.Request{PrincipalID: "agp_x",
		Tool: "agent_query"})
	if v.Allowed || v.Reason != decide.ReasonUnavailable {
		t.Errorf("verdict = %+v, want refused agent_governance_unavailable", v)
	}
}

func TestBuildAgentBrokerNeedsAControlDatabase(t *testing.T) {
	_, err := buildAgentBroker(context.Background(), nil, nil)
	if !errors.Is(err, broker.ErrUnavailable) {
		t.Errorf("error = %v, want ErrUnavailable", err)
	}
}
