package agentguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestToolAccess_Matrix(t *testing.T) {
	one := 1
	sponsored := Principal{Status: StatusActive, SponsorUserID: &one, SponsorActive: true}
	unsponsored := Principal{Status: StatusActive}
	departed := Principal{Status: StatusActive, SponsorUserID: &one}
	frozen := sponsored
	frozen.Status = StatusFrozen
	tainted := sponsored
	tainted.Tainted = true
	retired := sponsored
	retired.Status = StatusRetired
	bogus := sponsored
	bogus.Status = "killed"
	cases := []struct {
		name string
		p    Principal
		kind ToolKind
		want Access
	}{
		{"sponsored read", sponsored, ToolRead, Access{true, 3, ""}},
		{"sponsored propose capped", sponsored, ToolPropose, Access{true, 2, ""}},
		{"sponsored agent", sponsored, ToolAgent, Access{true, 3, ""}},
		{"unsponsored read", unsponsored, ToolRead, Access{true, 3, ""}},
		{"unsponsored propose queues", unsponsored, ToolPropose, Access{true, 2, ""}},
		{"unsponsored agent", unsponsored, ToolAgent, Access{false, 0, ReasonUnsponsored}},
		{"departed sponsor agent", departed, ToolAgent, Access{false, 0, ReasonUnsponsored}},
		{"frozen read", frozen, ToolRead, Access{true, 3, ""}},
		{"frozen propose", frozen, ToolPropose, Access{false, 0, ReasonFrozen}},
		{"frozen agent", frozen, ToolAgent, Access{false, 0, ReasonFrozen}},
		{"tainted propose", tainted, ToolPropose, Access{true, 2, ""}},
		{"tainted agent capped", tainted, ToolAgent, Access{true, 2, ""}},
		{"retired read", retired, ToolRead, Access{false, 0, ReasonRetired}},
		{"retired agent", retired, ToolAgent, Access{false, 0, ReasonRetired}},
		{"unknown status", bogus, ToolRead, Access{false, 0, ReasonRetired}},
		{"zero principal", Principal{}, ToolRead, Access{false, 0, ReasonRetired}},
		{"unknown kind", sponsored, ToolKind("admin"), Access{false, 0, ReasonLevel0}},
	}
	for _, c := range cases {
		require.Equal(t, c.want, ToolAccess(c.p, c.kind), c.name)
	}
}

func TestScramVerifier_KnownAnswer(t *testing.T) {
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	got, err := scramVerifier("pencil", salt, 4096)
	require.NoError(t, err)
	// Computed independently (Python hashlib/hmac, RFC 5802 definitions).
	require.Equal(t, "SCRAM-SHA-256$4096:AAECAwQFBgcICQoLDA0ODw==$"+
		"zHCdol2044/ZyWzPLi7oxApCkamKw9Z+E4U/QApd/5Y=:"+
		"dd5peBOitVnLNFu7VmwP+HiDaaw4OUCv396eVCWhYiE=", got)
	_, err = scramVerifier("pencil", nil, 4096)
	require.ErrorIs(t, err, ErrInvalid)
	_, err = scramVerifier("pencil", salt, 0)
	require.ErrorIs(t, err, ErrInvalid)
}

func TestNewScramVerifier_FreshSaltAndNoPlaintext(t *testing.T) {
	secret, err := newBrokerSecret()
	require.NoError(t, err)
	require.Len(t, secret, 43)
	a, err := newScramVerifier(secret)
	require.NoError(t, err)
	b, err := newScramVerifier(secret)
	require.NoError(t, err)
	require.NotEqual(t, a, b, "salt must be random")
	require.False(t, strings.Contains(a, secret))
	require.True(t, strings.HasPrefix(a, "SCRAM-SHA-256$4096:"))
	other, err := newBrokerSecret()
	require.NoError(t, err)
	require.NotEqual(t, secret, other)
}

func TestRoleConfig_ValidateAndSettings(t *testing.T) {
	cfg := DefaultRoleConfig()
	require.NoError(t, cfg.Validate())
	breakers := map[string]func(*RoleConfig){
		"conn":     func(c *RoleConfig) { c.ConnectionLimit = 0 },
		"broker":   func(c *RoleConfig) { c.BrokerConnectionLimit = -1 },
		"stmt":     func(c *RoleConfig) { c.StatementTimeout = 0 },
		"lock":     func(c *RoleConfig) { c.LockTimeout = -time.Second },
		"idle_tx":  func(c *RoleConfig) { c.IdleInTransactionTimeout = 0 },
		"idle":     func(c *RoleConfig) { c.IdleSessionTimeout = 0 },
		"tx":       func(c *RoleConfig) { c.TransactionTimeout = 0 },
		"temp":     func(c *RoleConfig) { c.TempFileLimitMB = -1 },
		"rolelock": func(c *RoleConfig) { c.RoleChangeLockTimeout = 0 },
		"rotation": func(c *RoleConfig) { c.BrokerCredentialRotation = 0 },
	}
	for name, mutate := range breakers {
		c := DefaultRoleConfig()
		mutate(&c)
		require.ErrorIs(t, c.Validate(), ErrInvalid, name)
	}
	pg16 := names(cfg.Settings(160000))
	require.Equal(t, []string{"statement_timeout=30000ms", "lock_timeout=2000ms",
		"idle_in_transaction_session_timeout=60000ms", "idle_session_timeout=600000ms",
		"temp_file_limit=1024MB?"}, pg16)
	pg17 := names(cfg.Settings(170000))
	require.Contains(t, pg17, "transaction_timeout=600000ms")
	cfg.TempFileLimitMB = 0
	require.NotContains(t, names(cfg.Settings(170000)), "temp_file_limit=0MB?")
	require.Len(t, cfg.Settings(170000), 5)
}

func names(ss []Setting) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name + "=" + s.Value
		if s.Optional {
			out[i] += "?"
		}
	}
	return out
}

func TestRoleAttributesAndQuoting(t *testing.T) {
	require.Equal(t, "LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS "+
		"CONNECTION LIMIT 2", roleAttributes(true, 2))
	require.True(t, strings.HasPrefix(roleAttributes(false, 5), "NOLOGIN "))
	require.Equal(t, `'it''s'`, literal("it's"))
	require.Equal(t, `"we""ird"`, ident(`we"ird`))
	require.Equal(t, `CREATE ROLE "r" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE `+
		`NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 2`, roleStatement(false, "r", true, 2))
	// ALTER never names attributes a CREATEROLE role may not change.
	require.Equal(t, `ALTER ROLE "r" WITH NOLOGIN NOCREATEROLE CONNECTION LIMIT 5`,
		roleStatement(true, "r", false, 5))
}

func TestCluster_Validate(t *testing.T) {
	require.ErrorIs(t, Cluster{}.validate(), ErrInvalid)
	require.ErrorIs(t, Cluster{Key: "k"}.validate(), ErrInvalid, "no admin pool")
	require.ErrorIs(t, Cluster{Key: strings.Repeat("k", 501)}.validate(), ErrInvalid)
}

func TestRoleRequest_Validate(t *testing.T) {
	ok := "agp_abcdefghijklmnopqrst"
	require.ErrorIs(t, RoleRequest{PrincipalID: "nope"}.validate(), ErrNotFound)
	require.ErrorIs(t, RoleRequest{PrincipalID: ok}.validate(), ErrApprovalRequired)
	require.ErrorIs(t, RoleRequest{PrincipalID: ok, Approval: Approval{ApprovedBy: -1}}.
		validate(), ErrApprovalRequired)
	require.ErrorIs(t, RoleRequest{PrincipalID: ok, Approval: Approval{ApprovedBy: 1}}.
		validate(), ErrInvalid, "no executor")
}

func TestDeniedError(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &DeniedError{Reason: ReasonFrozen, Detail: "kill",
		Fix: "unfreeze"})
	d, ok := IsDenied(err)
	require.True(t, ok)
	require.Equal(t, ReasonFrozen, d.Reason)
	require.Equal(t, "unfreeze", d.Fix)
	require.Equal(t, "agentguard: denied: agent_frozen: kill", d.Error())
	_, ok = IsDenied(errors.New("plain"))
	require.False(t, ok)
	require.Equal(t, "agentguard: denied: agent_level0", (&DeniedError{Reason: ReasonLevel0}).
		Error())
}

func TestIdentityContext(t *testing.T) {
	ctx := context.Background()
	_, err := PrincipalFromContext(ctx)
	require.ErrorIs(t, err, ErrNoPrincipal)
	_, ok := IdentityFromContext(WithIdentity(ctx, Identity{}))
	require.False(t, ok, "an identity without a principal is no identity")
	id := Identity{Principal: Principal{ID: "agp_abcdefghijklmnopqrst", Name: "bot"},
		TokenID: "t1", Databases: []string{"orders"}, TaskID: "task-9"}
	ctx = WithIdentity(ctx, id)
	got, ok := IdentityFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, "t1", got.TokenID)
	require.Equal(t, "task-9", got.TaskID)
	p, err := PrincipalFromContext(ctx)
	require.NoError(t, err)
	require.Equal(t, "bot", p.Name)
	require.True(t, got.MayUseDatabase("orders"))
	require.False(t, got.MayUseDatabase("billing"))
	require.False(t, got.MayUseDatabase(""))
	all := Identity{Principal: p}
	require.True(t, all.MayUseDatabase("billing"))
	require.False(t, all.MayUseDatabase(""))
}

// No concurrent-access test for Identity: it is an immutable value carried
// on a context, which is safe for concurrent use by construction.

func TestSelfResult(t *testing.T) {
	good := SelfResult{Role: "sage", CreateRole: true}
	require.NoError(t, good.Err())
	require.Empty(t, good.Problems())
	bad := SelfResult{Role: "sage", Superuser: true, BypassRLS: true,
		Inherits: []string{"sage_agentb_abcdefghij"}}
	require.Equal(t, 4, len(bad.Problems()))
	require.ErrorIs(t, bad.Err(), ErrSelfCheck)
	require.ErrorContains(t, bad.Err(), "role sage is superuser")
	require.ErrorContains(t, bad.Err(), "lacks CREATEROLE")
	require.ErrorContains(t, bad.Err(), "inherits agent roles sage_agentb_abcdefghij")
}

func TestRoleState_AgentViolations(t *testing.T) {
	require.Empty(t, RoleState{CanLogin: true, ConnLimit: 2}.AgentViolations())
	st := RoleState{Superuser: true, BypassRLS: true, CreateRole: true, CreateDB: true,
		Replication: true, Dangerous: []string{"pg_write_server_files"}, Owned: 3}
	v := st.AgentViolations()
	require.Equal(t, []string{"is superuser", "has BYPASSRLS", "has CREATEROLE",
		"has CREATEDB", "has REPLICATION", "is a member of pg_write_server_files",
		"owns 3 objects"}, v)
}

func TestPatch_Widens(t *testing.T) {
	p := Principal{EnvCeiling: EnvStage, Profile: "app-writer"}
	dev, prod := EnvDev, EnvProd
	same, other := "app-writer", "coding-agent"
	two := 2
	require.False(t, Patch{EnvCeiling: &dev}.Widens(p), "narrowing")
	require.True(t, Patch{EnvCeiling: &prod}.Widens(p), "raising the ceiling")
	require.False(t, Patch{Profile: &same}.Widens(p))
	require.True(t, Patch{Profile: &other}.Widens(p), "a profile change")
	require.False(t, Patch{SponsorUserID: &two}.Widens(p), "a sponsor change")
	require.False(t, Patch{}.Widens(p))
}

func TestNilStore_Unavailable(t *testing.T) {
	var s *Store
	ctx := context.Background()
	_, err := s.Get(ctx, "agp_abcdefghijklmnopqrst")
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = NewStore(nil).Create(ctx, CreateRequest{Name: "ok-bot", Profile: "legacy",
		CreatedBy: "admin"})
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = NewStore(nil).List(ctx, ListOptions{})
	require.ErrorIs(t, err, ErrUnavailable)
	_, err = NewStore(nil).ClusterRoles(ctx, "agp_abcdefghijklmnopqrst")
	require.ErrorIs(t, err, ErrUnavailable)
	require.Nil(t, s.Pool())
}

func TestCreateRequest_InvalidNeverTouchesStorage(t *testing.T) {
	s := NewStore(nil) // a storage call would be ErrUnavailable, not ErrInvalid
	zero := 0
	bad := []CreateRequest{
		{Name: "B", Profile: "legacy", CreatedBy: "a"},
		{Name: "ok-bot", Profile: "", CreatedBy: "a"},
		{Name: "ok-bot", Profile: "Legacy", CreatedBy: "a"},
		{Name: "ok-bot", Profile: "legacy", CreatedBy: ""},
		{Name: "ok-bot", Profile: "legacy", CreatedBy: "a\x00b"},
		{Name: "ok-bot", Profile: "legacy", CreatedBy: "a", EnvCeiling: "qa"},
		{Name: "ok-bot", Profile: "legacy", CreatedBy: "a", SponsorUserID: &zero},
		{Name: "ok-bot", Profile: "legacy", CreatedBy: "a", Tenant: strings.Repeat("t", 201)},
		{Name: "ok-bot", Profile: "legacy", CreatedBy: strings.Repeat("a", 201)},
	}
	for i, req := range bad {
		_, err := s.Create(context.Background(), req)
		require.ErrorIs(t, err, ErrInvalid, "case %d", i)
	}
	_, err := s.List(context.Background(), ListOptions{Limit: 201})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = s.List(context.Background(), ListOptions{Status: "gone"})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = s.List(context.Background(), ListOptions{Cursor: "Bad Cursor"})
	require.ErrorIs(t, err, ErrInvalid)
	_, err = s.SetStatus(context.Background(), "agp_abcdefghijklmnopqrst", "gone", "")
	require.ErrorIs(t, err, ErrInvalid)
	bad2 := "Bad"
	_, err = s.Update(context.Background(), "agp_abcdefghijklmnopqrst", Patch{Profile: &bad2})
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorIs(t, s.Taint(context.Background(), "agp_abcdefghijklmnopqrst", "", ""),
		ErrInvalid)
}
