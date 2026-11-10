package rollout

import (
	"context"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// No concurrent access tests: validatePolicy is a pure function of its
// argument and Rollout holds no state between calls.

// An unsafe policy is refused before any instance is re-verified, applied
// or measured: the engine is the only gate on a request's policy.
func TestRolloutRefusesUnsafePolicyBeforeTouchingAnInstance(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Policy)
	}{
		{"canaries exceed blast radius", func(p *Policy) {
			p.CanaryInstances, p.MaxAffectedInstances = 3, 2
		}},
		{"zero canaries", func(p *Policy) { p.CanaryInstances = 0 }},
		{"negative canaries", func(p *Policy) { p.CanaryInstances = -1 }},
		{"zero blast radius", func(p *Policy) { p.MaxAffectedInstances = 0 }},
		{"negative blast radius", func(p *Policy) { p.MaxAffectedInstances = -1 }},
		{"negative regression limit", func(p *Policy) {
			p.AggregateRegressionLimitPct = -0.01
		}},
		{"local verification disabled", func(p *Policy) {
			p.RequireLocalReverification = false
		}},
		{"zero policy", func(p *Policy) { *p = Policy{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := newRecordingRolloutBackend()
			request := healthyRolloutRequest("a", "b", "c")
			tc.mutate(&request.Policy)

			result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

			require.ErrorIs(t, err, ErrInvalidPolicy)
			require.Zero(t, result.AppliedInstances)
			require.False(t, result.Halted)
			require.Zero(t, backend.totalCalls())
		})
	}
}

// The boundaries validatePolicy allows: canaries equal to the blast radius
// and a zero regression limit are both valid.
func TestRolloutAcceptsPolicyAtItsBoundaries(t *testing.T) {
	backend := newRecordingRolloutBackend()
	request := healthyRolloutRequest("a", "b")
	request.Policy.CanaryInstances = 2
	request.Policy.MaxAffectedInstances = 2
	request.Policy.AggregateRegressionLimitPct = 0

	result, err := NewEngine(backend, backend).Rollout(context.Background(), request)

	require.NoError(t, err)
	require.False(t, result.Halted)
	require.Equal(t, 2, result.AppliedInstances)
	require.Equal(t, []string{"a", "b"}, result.CanaryInstanceIDs)
	require.Equal(t, 2, backend.totalApplyCalls())
}
