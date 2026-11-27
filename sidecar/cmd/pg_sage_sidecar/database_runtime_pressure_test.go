package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/pressure"
)

// guard-01a: check_pressure's limits come from the standing policy's
// pressure block over defaults halving the configured alert thresholds;
// a policy that cannot be read or parsed is an error (the checker stops).
// No concurrent access tests: each call reads the policy afresh and keeps
// no state.

func pressurePolicyWith(t *testing.T, block string) policy.Policy {
	t.Helper()
	raw, err := policy.MarshalDocument(policy.StaffedProfile())
	if err != nil {
		t.Fatalf("marshal staffed: %v", err)
	}
	if block == "" {
		return policy.Policy{Version: 3, Document: raw}
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode: %v", err)
	}
	wire["pressure"] = json.RawMessage(block)
	out, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return policy.Policy{Version: 4, Document: out}
}

func TestPressureLimitsFromThePolicyBlock(t *testing.T) {
	stored := pressurePolicyWith(t, `{"limits": {"replica_lag_seconds": {"stop": 8}},
		"required": ["free_storage_pct"]}`)
	read := func(context.Context) (policy.Policy, error) { return stored, nil }
	alerts := func() pressure.Alerts { return pressure.Alerts{ReplicaLagSeconds: 60} }
	l, err := pressureLimitsFrom(read, alerts)(context.Background())
	if err != nil {
		t.Fatalf("limits: %v", err)
	}
	lag := l.Inputs[policy.PressureReplicaLag]
	if *lag.Stop != 8 || *lag.Slow != 4 {
		t.Fatalf("lag = stop %v slow %v, want the policy's 8 and a slow of 4 (the "+
			"default 15 would sit past the stop)", *lag.Stop, *lag.Slow)
	}
	if len(l.Required) != 1 || !l.Required[policy.PressureFreeStorage] {
		t.Fatalf("required = %v, want only free storage", l.Required)
	}
	defaults, err := pressureLimitsFrom(func(context.Context) (policy.Policy, error) {
		return pressurePolicyWith(t, ""), nil
	}, alerts)(context.Background())
	if err != nil || *defaults.Inputs[policy.PressureReplicaLag].Stop != 30 {
		t.Fatalf("no block = %v, %v; want the alert-derived stop of 30 s", defaults, err)
	}
}

func TestPressureLimitsFailuresAreErrors(t *testing.T) {
	alerts := func() pressure.Alerts { return pressure.Alerts{} }
	_, err := pressureLimitsFrom(func(context.Context) (policy.Policy, error) {
		return policy.Policy{}, errors.New("connection refused")
	}, alerts)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read the standing policy") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("read failure err = %v, want it named", err)
	}
	_, err = pressureLimitsFrom(func(context.Context) (policy.Policy, error) {
		return policy.Policy{Version: 9, Document: []byte(`{"pressure": 1}`)}, nil
	}, alerts)(context.Background())
	if err == nil || !strings.Contains(err.Error(), "parse the standing policy") {
		t.Fatalf("parse failure err = %v, want it named", err)
	}
}

func TestPressureAlertsFollowTheConfig(t *testing.T) {
	if got := pressureAlerts(nil); got != (pressure.Alerts{}) {
		t.Fatalf("nil config = %+v, want the zero alerts (package defaults)", got)
	}
	cfg := config.DefaultConfig()
	cfg.RCA.ReplicationLagThresholdS = 44
	cfg.Analyzer.XIDWraparoundWarning = 600_000_000
	got := pressureAlerts(cfg)
	if got.ReplicaLagSeconds != 44 || got.XIDWarning != 600_000_000 ||
		got.MinFreeStoragePct <= 0 {
		t.Fatalf("alerts = %+v, want 44 s, 600M and the cloud free storage floor", got)
	}
}

func TestPressureCheckerNeedsAPool(t *testing.T) {
	rt := &databaseRuntime{}
	if c := rt.pressureChecker(); c != nil {
		t.Fatalf("checker without a pool = %v, want nil (the route answers unavailable)", c)
	}
}
