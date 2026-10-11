package agentguard

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// Kill switch and freeze (spec §6.10, §8.3): request validation,
// configuration, the report's wire shape, standby classification, prior
// attributes and the local fallback log. No database.

const validPID = "agp_aaaaaaaaaaaaaaaaaaaa"

func TestKillRequest_Validate(t *testing.T) {
	ok := []KillRequest{
		{Scope: KillScopeAll, Reason: "incident 42", Actor: "a@example.com"},
		{Scope: KillScopePrincipal, ID: validPID, Reason: "r", Actor: "a"},
		{Scope: KillScopeDatabase, ID: "orders", Reason: "r", Actor: "a"},
	}
	for _, r := range ok {
		require.NoError(t, r.Validate(), r.Scope)
	}
	bad := map[string]KillRequest{
		"no scope":         {Reason: "r", Actor: "a"},
		"unknown scope":    {Scope: "fleet", Reason: "r", Actor: "a"},
		"all with id":      {Scope: KillScopeAll, ID: "x", Reason: "r", Actor: "a"},
		"principal no id":  {Scope: KillScopePrincipal, Reason: "r", Actor: "a"},
		"principal bad id": {Scope: KillScopePrincipal, ID: "bot-1", Reason: "r", Actor: "a"},
		"database no id":   {Scope: KillScopeDatabase, Reason: "r", Actor: "a"},
		"empty reason":     {Scope: KillScopeAll, Actor: "a"},
		"long reason":      {Scope: KillScopeAll, Reason: strings.Repeat("x", 2001), Actor: "a"},
		"control reason":   {Scope: KillScopeAll, Reason: "a\x00b", Actor: "a"},
		"no actor":         {Scope: KillScopeAll, Reason: "r"},
		"database ctrl":    {Scope: KillScopeDatabase, ID: "a\nb", Reason: "r", Actor: "a"},
	}
	for name, r := range bad {
		err := r.Validate()
		require.ErrorIs(t, err, ErrInvalid, name)
	}
}

func TestFreezeRequest_Validate(t *testing.T) {
	require.NoError(t, FreezeRequest{PrincipalID: validPID, Reason: "r", Actor: "a"}.Validate())
	require.ErrorIs(t, FreezeRequest{PrincipalID: "x", Reason: "r", Actor: "a"}.Validate(),
		ErrInvalid)
	require.ErrorIs(t, FreezeRequest{PrincipalID: validPID, Actor: "a"}.Validate(), ErrInvalid)
	require.ErrorIs(t, FreezeRequest{PrincipalID: validPID, Reason: "r"}.Validate(), ErrInvalid)
}

func TestUnfreezeRequest_Validate(t *testing.T) {
	good := UnfreezeRequest{PrincipalID: validPID, Reason: "fixed", Actor: "a", ActorUserID: 3}
	require.NoError(t, good.Validate())
	noUser := good
	noUser.ActorUserID = 0
	require.ErrorIs(t, noUser.Validate(), ErrApprovalRequired)
	badID := good
	badID.PrincipalID = "agp_x"
	require.ErrorIs(t, badID.Validate(), ErrInvalid)
	noActor := good
	noActor.Actor = ""
	require.ErrorIs(t, noActor.Validate(), ErrInvalid)
	// The reason may be empty for a plain unfreeze; single-operator mode
	// requires one (checked by Unfreeze, which knows the mode).
	noReason := good
	noReason.Reason = ""
	require.NoError(t, noReason.Validate())
}

func TestReleaseRequest_Validate(t *testing.T) {
	require.NoError(t, ReleaseRequest{Scope: KillScopeAll, Reason: "r", Actor: "a",
		ActorUserID: 1}.Validate())
	require.NoError(t, ReleaseRequest{Scope: KillScopeDatabase, Database: "orders",
		Reason: "r", Actor: "a", ActorUserID: 1}.Validate())
	require.ErrorIs(t, ReleaseRequest{Scope: KillScopePrincipal, Database: validPID,
		Reason: "r", Actor: "a", ActorUserID: 1}.Validate(), ErrInvalid)
	require.ErrorIs(t, ReleaseRequest{Scope: KillScopeDatabase, Reason: "r", Actor: "a",
		ActorUserID: 1}.Validate(), ErrInvalid)
	require.ErrorIs(t, ReleaseRequest{Scope: KillScopeAll, Database: "x", Reason: "r",
		Actor: "a", ActorUserID: 1}.Validate(), ErrInvalid)
	require.ErrorIs(t, ReleaseRequest{Scope: KillScopeAll, Reason: "r", Actor: "a"}.Validate(),
		ErrApprovalRequired)
}

func TestDefaultKillConfig(t *testing.T) {
	c := DefaultKillConfig()
	require.Equal(t, 10*time.Second, c.VerifyTimeout)
	require.Equal(t, 2*time.Second, c.LockTimeout)
	require.Equal(t, 3, c.Attempts)
	require.Equal(t, 15*time.Minute, c.ApprovalTTL)
	require.False(t, c.SingleOperatorMode)
	require.Positive(t, c.ReplicaConnectTimeout)
	require.Equal(t, DefaultRoleConfig(), c.Roles)
	require.NoError(t, c.Validate())
}

func TestKillConfig_ValidateRejectsZeroes(t *testing.T) {
	muts := map[string]func(*KillConfig){
		"verify":   func(c *KillConfig) { c.VerifyTimeout = 0 },
		"lock":     func(c *KillConfig) { c.LockTimeout = -time.Second },
		"attempts": func(c *KillConfig) { c.Attempts = 0 },
		"replica":  func(c *KillConfig) { c.ReplicaConnectTimeout = 0 },
		"ttl":      func(c *KillConfig) { c.ApprovalTTL = 0 },
		"roles":    func(c *KillConfig) { c.Roles.StatementTimeout = 0 },
	}
	for name, mut := range muts {
		c := DefaultKillConfig()
		mut(&c)
		require.ErrorIs(t, c.Validate(), ErrInvalid, name)
	}
}

func TestNewSwitch_RequiresTargetsAndValidConfig(t *testing.T) {
	_, err := NewSwitch(KillDeps{Store: NewStore(nil), Config: DefaultKillConfig()})
	require.ErrorIs(t, err, ErrInvalid)
	bad := DefaultKillConfig()
	bad.Attempts = 0
	_, err = NewSwitch(KillDeps{Store: NewStore(nil), Config: bad,
		Targets: func(context.Context) ([]KillTarget, error) { return nil, nil }})
	require.ErrorIs(t, err, ErrInvalid)
	s, err := NewSwitch(KillDeps{Config: DefaultKillConfig(),
		Targets: func(context.Context) ([]KillTarget, error) { return nil, nil }})
	require.NoError(t, err, "a nil store is a control database that is unavailable")
	require.NotNil(t, s)
}

func TestSessionBoundFor(t *testing.T) {
	rc := DefaultRoleConfig()
	b16 := SessionBoundFor(rc, 160004)
	require.Equal(t, int64(30000), b16.StatementTimeoutMS)
	require.Equal(t, int64(600000), b16.IdleSessionTimeoutMS)
	require.Equal(t, int64(0), b16.TransactionTimeoutMS, "no transaction_timeout before 17")
	b17 := SessionBoundFor(rc, 170000)
	require.Equal(t, int64(600000), b17.TransactionTimeoutMS)
	rc.StatementTimeout = 5 * time.Second
	require.Equal(t, int64(5000), SessionBoundFor(rc, 180000).StatementTimeoutMS)
}

func TestClassifyStandbys(t *testing.T) {
	bound := SessionBound{StatementTimeoutMS: 30000, IdleSessionTimeoutMS: 600000}
	obs := []StandbyObservation{
		{Source: "primary", ApplicationName: "replica1", ClientAddr: "10.0.0.2",
			State: "streaming"},
		{Source: "primary", ApplicationName: "standby2", ClientAddr: "10.0.0.3",
			State: "streaming"},
		{Source: "replica1", ApplicationName: "walreceiver", ClientAddr: "10.0.0.4",
			State: "catchup"},
	}
	got := classifyStandbys(obs, map[string]bool{"replica1": true}, bound)
	require.Len(t, got, 2)
	names := []string{got[0].ApplicationName, got[1].ApplicationName}
	require.Contains(t, names, "standby2")
	require.Contains(t, names, "walreceiver")
	for _, r := range got {
		require.False(t, r.Configured)
		require.NotNil(t, r.Bound)
		require.Equal(t, bound, *r.Bound)
		require.NotEmpty(t, r.Name)
		require.False(t, r.Verified, "an unconfigured standby is never verified")
	}
	require.Empty(t, classifyStandbys(nil, nil, bound))
	require.Empty(t, classifyStandbys(obs[:1], map[string]bool{"replica1": true}, bound))
}

func TestPriorAttrs_RoundTripAndMalformed(t *testing.T) {
	p := PriorAttrs{"sage_agentb_abcdefghij": {Login: true, ConnectionLimit: 2},
		"sage_agent_abcdefghij": {Login: false, ConnectionLimit: 5}}
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"connection_limit":2`)
	got, err := ParsePriorAttrs(raw)
	require.NoError(t, err)
	require.Equal(t, p, got)
	empty, err := ParsePriorAttrs(nil)
	require.NoError(t, err)
	require.Empty(t, empty)
	_, err = ParsePriorAttrs(json.RawMessage(`{"x": 1`))
	require.Error(t, err)
	_, err = ParsePriorAttrs(json.RawMessage(`{"not_an_agent_role": {"login": true}}`))
	require.ErrorIs(t, err, ErrInvalid)
	_, err = ParsePriorAttrs(json.RawMessage(
		`{"sage_agentb_abcdefghij": {"login": true, "connection_limit": -5}}`))
	require.ErrorIs(t, err, ErrInvalid)
}

func TestKillReport_WireShape(t *testing.T) {
	rep := KillReport{KillID: 7, Scope: KillScopeAll, Reason: "r",
		Databases: []DatabaseReport{{Name: "orders", RolesDisabled: 2, BackendsTerminated: 1,
			Verified: true, Replicas: []ReplicaReport{{Name: "replica1", Configured: true,
				Verified: true, LoginsBlocked: true}}}}}
	raw, err := json.Marshal(rep)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	dbs := m["databases"].([]any)
	db := dbs[0].(map[string]any)
	for _, k := range []string{"name", "roles_disabled", "backends_terminated", "replicas",
		"verified"} {
		require.Contains(t, db, k)
	}
	require.NotContains(t, db, "error", "error is omitted when empty")
	require.Equal(t, float64(7), m["kill_id"])
	rep.Databases[0].Error = "boom"
	raw, _ = json.Marshal(rep)
	require.Contains(t, string(raw), `"error":"boom"`)
}

func TestFallbackLog_AppendAndRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kill.log")
	l := NewFallbackLog(path)
	require.NotNil(t, l)
	e1 := FallbackEntry{ActionType: "guard_kill", Database: "orders", Scope: "all",
		Reason: "r", Actor: "a", Statements: []string{"ALTER ROLE x NOLOGIN"},
		Outcome: "success"}
	require.NoError(t, l.Append(e1))
	require.NoError(t, l.Append(FallbackEntry{ActionType: "guard_kill", Database: "b",
		Outcome: "failed", Error: "connection refused"}))
	got, err := l.Entries()
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "orders", got[0].Database)
	require.False(t, got[0].At.IsZero(), "Append stamps the time")
	require.Equal(t, "connection refused", got[1].Error)
}

func TestFallbackLog_NilAndMissing(t *testing.T) {
	require.Nil(t, NewFallbackLog(""))
	var l *FallbackLog
	require.ErrorIs(t, l.Append(FallbackEntry{}), ErrNoFallbackLog)
	got, err := NewFallbackLog(filepath.Join(t.TempDir(), "absent.log")).Entries()
	require.NoError(t, err, "a log never written to has no entries")
	require.Empty(t, got)
	bad := NewFallbackLog(filepath.Join(t.TempDir(), "no", "such", "dir", "kill.log"))
	require.Error(t, bad.Append(FallbackEntry{ActionType: "guard_kill"}))
}

// Concurrent appends never interleave: every line is one whole entry.
func TestFallbackLog_ConcurrentAppends(t *testing.T) {
	l := NewFallbackLog(filepath.Join(t.TempDir(), "kill.log"))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			require.NoError(t, l.Append(FallbackEntry{ActionType: "guard_kill",
				Database: strings.Repeat("d", i+1), Statements: []string{
					strings.Repeat("x", 4096)}}))
		}(i)
	}
	wg.Wait()
	got, err := l.Entries()
	require.NoError(t, err)
	require.Len(t, got, 20)
	seen := map[int]bool{}
	for _, e := range got {
		seen[len(e.Database)] = true
	}
	require.Len(t, seen, 20)
}

func TestKillErrorsAreDistinct(t *testing.T) {
	all := []error{ErrNotFrozen, ErrSponsorCannotApprove, ErrNoFallbackLog, ErrInvalid,
		ErrNotFound, ErrUnavailable, ErrApprovalRequired}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Fatalf("%v is %v", a, b)
			}
		}
	}
}

// A memory revoker is told which principals to drop (all=true for the
// whole fleet) before the database steps run.
type fakeMemory struct {
	mu    sync.Mutex
	calls [][]string
	all   []bool
}

func (f *fakeMemory) RevokePrincipals(_ context.Context, ids []string, all bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), ids...))
	f.all = append(f.all, all)
}
