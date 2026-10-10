package grants

import (
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/agentguard/decide"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Pure parts: object names, the contracts' shape, verdict mapping and
// configuration bounds. No concurrency tests here: these functions are
// stateless.

func TestParseObject(t *testing.T) {
	for in, want := range map[string][2]string{
		"app.orders":      {"app", "orders"},
		`"My App".Orders`: {"My App", "Orders"},
		`app."a.b"`:       {"app", "a.b"},
		`"we""ird".t`:     {`we"ird`, "t"},
	} {
		s, r, err := parseObject(in)
		require.NoError(t, err, in)
		require.Equal(t, want, [2]string{s, r}, in)
	}
	for _, bad := range []string{"", "orders", "a.b.c", ".t", "s.", `"open.t`,
		"s.t; DROP", "s.t\x00", string(make([]byte, 300)) + ".t", `"".t`} {
		_, _, err := parseObject(bad)
		require.ErrorIs(t, err, agentguard.ErrInvalid, bad)
	}
}

func TestContracts_GrantWidensRevokeNarrows(t *testing.T) {
	g, ok := executor.PolicyContractFor(executor.ActionTypeGuardGrant)
	require.True(t, ok)
	r, ok := executor.PolicyContractFor(executor.ActionTypeGuardRevoke)
	require.True(t, ok)
	require.False(t, g.Narrowing, "guard_grant widens: L2, approved")
	require.True(t, r.Narrowing, "guard_revoke narrows (§6.2.4)")
	require.Equal(t, "moderate", string(g.RiskTier))
	require.Equal(t, "reversible", string(g.RollbackClass))
	require.Equal(t, "safe", string(r.RiskTier))
	require.Equal(t, "no_rollback_needed", string(r.RollbackClass))
	for _, at := range []string{executor.ActionTypeGuardGrant, executor.ActionTypeGuardRevoke} {
		c, ok := executor.ContractForActionType(at)
		require.True(t, ok)
		require.NoError(t, c.Validate())
		require.Len(t, c.ProviderSupport, 8)
	}
}

func TestVerdictOf(t *testing.T) {
	cases := []struct {
		in   decide.Verdict
		want string
		code string
	}{
		{decide.Verdict{Allowed: true, MaxLevel: 3}, VerdictQueueApproval, "approval_required"},
		{decide.Verdict{Allowed: true, MaxLevel: 2}, VerdictQueueApproval, "approval_required"},
		{decide.Verdict{Allowed: true, MaxLevel: 1}, VerdictObserveOnly,
			"agent_proposal_recorded"},
		{decide.Verdict{Allowed: true, MaxLevel: 0}, VerdictBlocked, "agent_level0"},
		{decide.Verdict{Reason: decide.ReasonEnvCeiling}, VerdictBlocked, "agent_env_ceiling"},
		{decide.Verdict{}, VerdictBlocked, "agent_governance_unavailable"},
		{decide.Verdict{Park: true, Reason: decide.ReasonRate}, VerdictPark, "agent_rate"},
	}
	for _, c := range cases {
		v, code := verdictOf(c.in)
		require.Equal(t, c.want, v, c.in)
		require.Equal(t, c.code, code, c.in)
	}
}

func TestNewManager_Bounds(t *testing.T) {
	_, err := NewManager(nil, Config{MaxDuration: time.Hour})
	require.ErrorIs(t, err, agentguard.ErrInvalid)
	for _, d := range []time.Duration{0, -time.Hour, 30 * time.Second} {
		_, err := NewManager(&agentguard.Store{}, Config{MaxDuration: d})
		require.ErrorIs(t, err, agentguard.ErrInvalid, d)
	}
	m, err := NewManager(&agentguard.Store{}, Config{MaxDuration: time.Minute})
	require.NoError(t, err)
	require.NotNil(t, m)
}

func TestFixStatements(t *testing.T) {
	require.Equal(t, `GRANT SELECT ("a", "b c") ON TABLE "s"."t" TO "me" WITH GRANT OPTION;`,
		grantOptionFix("s", "t", []string{"a", "b c"}, "me"))
	require.Equal(t, `GRANT USAGE ON SCHEMA "s" TO "me" WITH GRANT OPTION;`,
		schemaOptionFix("s", "me"))
}

func TestErrorsAreDistinct(t *testing.T) {
	all := []error{ErrFenced, ErrNotActive, ErrRequestNotPending}
	for i, a := range all {
		for j, b := range all {
			require.Equal(t, i == j, errors.Is(a, b))
		}
	}
}

// guard_grant and guard_revoke requests carry the agent they act for, so
// the gate runs the D-steps on them (§6.2.2) and decides the grant as the
// requested capability class; the change class stays agent_access.
func TestGateRequest_CarriesPrincipalAndCapability(t *testing.T) {
	tgt := Target{ID: "00000000-0000-4000-8000-000000000001"}
	req, err := gateRequest(executor.ActionTypeGuardGrant, "agp_aaaaaaaaaaaaaaaaaaaa", tgt,
		[]string{"table:app.t"}, nil, true)
	require.NoError(t, err)
	require.NotNil(t, req.Principal)
	require.Equal(t, "agp_aaaaaaaaaaaaaaaaaaaa", req.Principal.ID)
	require.Equal(t, ToolRequestCapability, req.Principal.Tool)
	require.Equal(t, CapabilityRead, req.CapabilityClass)
	require.Equal(t, "agent_access", req.Feature)
	require.True(t, req.OperatorApproved && req.InternalControl)
	require.False(t, req.Contract.Narrowing)
	rv, err := gateRequest(executor.ActionTypeGuardRevoke, "agp_aaaaaaaaaaaaaaaaaaaa", tgt,
		nil, nil, false)
	require.NoError(t, err)
	require.True(t, rv.Contract.Narrowing)
	require.Equal(t, "agp_aaaaaaaaaaaaaaaaaaaa", rv.Principal.ID)
}
