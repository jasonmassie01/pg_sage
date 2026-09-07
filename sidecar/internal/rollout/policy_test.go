package rollout

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestPolicyInheritanceFleetCohortThenInstance(t *testing.T) {
	policies := PolicySet{
		FleetDefault: &Policy{
			CanaryInstances:             2,
			MaxAffectedInstances:        20,
			AggregateRegressionLimitPct: 15,
			RequireLocalReverification:  true,
		},
		ClassDefaults: map[string]PolicyPatch{
			"critical": {
				CanaryInstances:      intPointer(3),
				MaxAffectedInstances: intPointer(10),
			},
		},
		TagDefaults: map[string]PolicyPatch{
			"low-latency": {
				AggregateRegressionLimitPct: floatPointer(8),
			},
		},
		InstanceOverrides: map[string]PolicyPatch{
			"orders-primary": {
				MaxAffectedInstances: intPointer(5),
			},
		},
	}

	effective, err := policies.Resolve(Instance{
		ID: "orders-primary", Class: "critical", Tags: []string{"low-latency"},
	})

	require.NoError(t, err)
	require.Equal(t, 3, effective.CanaryInstances)
	require.Equal(t, 5, effective.MaxAffectedInstances)
	require.Equal(t, float64(8), effective.AggregateRegressionLimitPct)
	require.True(t, effective.RequireLocalReverification)
	require.Equal(t, []string{
		"fleet", "class:critical", "tag:low-latency", "instance:orders-primary",
	}, effective.Sources)
}

func TestPolicyInheritanceDoesNotMutateParentPolicies(t *testing.T) {
	fleetPolicy := &Policy{
		CanaryInstances:             2,
		MaxAffectedInstances:        20,
		AggregateRegressionLimitPct: 15,
		RequireLocalReverification:  true,
	}
	policies := PolicySet{
		FleetDefault: fleetPolicy,
		InstanceOverrides: map[string]PolicyPatch{
			"orders": {MaxAffectedInstances: intPointer(4)},
		},
	}

	orders, err := policies.Resolve(Instance{ID: "orders"})
	require.NoError(t, err)
	analytics, err := policies.Resolve(Instance{ID: "analytics"})
	require.NoError(t, err)

	require.Equal(t, 4, orders.MaxAffectedInstances)
	require.Equal(t, 20, analytics.MaxAffectedInstances)
	require.Equal(t, 20, fleetPolicy.MaxAffectedInstances)
}

func TestPolicyInheritanceFailsClosedWithoutFleetDefault(t *testing.T) {
	policies := PolicySet{
		InstanceOverrides: map[string]PolicyPatch{
			"orders": {MaxAffectedInstances: intPointer(1)},
		},
	}

	_, err := policies.Resolve(Instance{ID: "orders"})

	require.ErrorIs(t, err, ErrPolicyUnavailable)
}

func TestPolicyInheritanceRejectsUnsafeEffectivePolicy(t *testing.T) {
	testCases := []struct {
		name   string
		policy Policy
	}{
		{
			name: "zero canaries",
			policy: Policy{
				CanaryInstances: 0, MaxAffectedInstances: 10,
				AggregateRegressionLimitPct: 15, RequireLocalReverification: true,
			},
		},
		{
			name: "canaries exceed blast radius",
			policy: Policy{
				CanaryInstances: 3, MaxAffectedInstances: 2,
				AggregateRegressionLimitPct: 15, RequireLocalReverification: true,
			},
		},
		{
			name: "local verification disabled",
			policy: Policy{
				CanaryInstances: 2, MaxAffectedInstances: 10,
				AggregateRegressionLimitPct: 15, RequireLocalReverification: false,
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			policies := PolicySet{FleetDefault: &testCase.policy}
			_, err := policies.Resolve(Instance{ID: "orders"})
			require.ErrorIs(t, err, ErrInvalidPolicy)
		})
	}
}

func intPointer(value int) *int {
	return &value
}

func floatPointer(value float64) *float64 {
	return &value
}
