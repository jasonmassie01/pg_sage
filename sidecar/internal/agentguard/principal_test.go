package agentguard

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestNewID_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := NewID()
		require.NoError(t, err)
		require.True(t, ValidID(id), "id %q", id)
		require.Len(t, id, 24)
		require.False(t, seen[id], "duplicate id %q", id)
		seen[id] = true
	}
}

func TestValidID_Rejects(t *testing.T) {
	for _, id := range []string{"", "agp_", "agp_abcdefghijklmnopqrs", // 19
		"agp_abcdefghijklmnopqrstu", // 21
		"AGP_abcdefghijklmnopqrst", "agp_ABCDEFGHIJKLMNOPQRST",
		"agp_abcdefghijklmnopqrs1", // 1 is not base32
		"agx_abcdefghijklmnopqrst", " agp_abcdefghijklmnopqrst"} {
		require.False(t, ValidID(id), "accepted %q", id)
	}
	require.True(t, ValidID("agp_abcdefghijklmnopqrst"))
	require.True(t, IsAgentRoleName(BrokerRoleName("agp_abcdefghijklmnopqrst")))
	require.True(t, IsAgentRoleName(LoginRoleName("agp_abcdefghijklmnopqrst")))
	require.False(t, IsAgentRoleName("sage_agent_k2m4q7x9a1"), "1 is not base32")
	require.True(t, ValidID("agp_234567abcdefghijklmn"))
}

func TestValidName_Boundaries(t *testing.T) {
	good := []string{"ab", "a1", "a-", "billing-bot", "a" + strings.Repeat("b", 62)}
	for _, n := range good {
		require.True(t, ValidName(n), "rejected %q", n)
	}
	bad := []string{"", "a", "1a", "-a", "Ab", "a_b", "a b", "a.b", "é-bot",
		"a" + strings.Repeat("b", 63), "drop table;"}
	for _, n := range bad {
		require.False(t, ValidName(n), "accepted %q", n)
	}
}

func TestRoleNames_DeterministicAndDistinct(t *testing.T) {
	a, b := "agp_abcdefghijklmnopqrst", "agp_abcdefghijklmnopqrsu"
	require.Equal(t, LoginRoleName(a), LoginRoleName(a))
	require.True(t, strings.HasPrefix(LoginRoleName(a), "sage_agent_"))
	require.True(t, strings.HasPrefix(BrokerRoleName(a), "sage_agentb_"))
	require.Equal(t, strings.TrimPrefix(LoginRoleName(a), "sage_agent_"),
		strings.TrimPrefix(BrokerRoleName(a), "sage_agentb_"))
	require.NotEqual(t, LoginRoleName(a), LoginRoleName(b))
	for _, r := range []string{LoginRoleName(a), BrokerRoleName(a), BrokerRoleName(b)} {
		require.True(t, RolePattern.MatchString(r), "role %q", r)
	}
	p := Principal{ID: a}
	require.Equal(t, LoginRoleName(a), p.LoginRole())
	require.Equal(t, BrokerRoleName(a), p.BrokerRole())
	// Golden: sha256("agp_abcdefghijklmnopqrst") in lower base32, first 10.
	require.Equal(t, "sage_agentb_"+roleSuffix(a), BrokerRoleName(a))
	require.Len(t, roleSuffix(a), 10)
}

func TestRolePattern_RejectsLookalikes(t *testing.T) {
	for _, r := range []string{"sage_agent_abc", "sage_agentx_abcdefghij",
		"sage_agent_abcdefghij1", "sage_agent_ABCDEFGHIJ", "xsage_agent_abcdefghij",
		"sage_agent_abcdefghi1"} {
		require.False(t, RolePattern.MatchString(r), "matched %q", r)
		require.False(t, IsAgentRoleName(r), "matched %q", r)
	}
}

func TestEnv_RankAndMin(t *testing.T) {
	require.True(t, EnvBranch.Rank() < EnvDev.Rank())
	require.True(t, EnvDev.Rank() < EnvStage.Rank())
	require.True(t, EnvStage.Rank() < EnvProd.Rank())
	require.Equal(t, 0, Env("qa").Rank())
	require.False(t, Env("").Valid())
	require.Equal(t, EnvDev, MinEnv(EnvProd, EnvDev))
	require.Equal(t, EnvDev, MinEnv(EnvDev, EnvProd))
	require.Equal(t, EnvStage, MinEnv(EnvStage, EnvStage))
	// An invalid ceiling never widens: it collapses to the narrowest.
	require.Equal(t, EnvBranch, MinEnv(Env("qa"), EnvProd))
	require.Equal(t, EnvBranch, MinEnv(EnvProd, ""))
}

func TestStatusAndSponsorship(t *testing.T) {
	require.True(t, StatusActive.Valid())
	require.True(t, StatusFrozen.Valid())
	require.True(t, StatusRetired.Valid())
	require.False(t, Status("killed").Valid())
	one := 1
	require.True(t, Principal{SponsorUserID: &one, SponsorActive: true}.Sponsored())
	require.False(t, Principal{SponsorUserID: &one}.Sponsored(), "departed sponsor")
	require.False(t, Principal{SponsorActive: true}.Sponsored(), "no sponsor id")
	require.True(t, Principal{Status: StatusFrozen}.Frozen())
	require.False(t, Principal{Status: StatusActive}.Frozen())
	require.True(t, Principal{Status: StatusRetired}.Retired())
}
