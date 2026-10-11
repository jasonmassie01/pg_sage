package agentguard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// The kill switch on one primary (spec §6.10 steps 1-8): every
// step is checked against the catalog and the control tables, not only
// against the report.

func killPrincipal(p Principal) KillRequest {
	return KillRequest{Scope: KillScopePrincipal, ID: p.ID, Reason: "incident 42",
		Actor: "admin@example.com"}
}

// principalKill is the setup of TestKill_PrincipalContainsEverything: p
// is killed while other, with the same kinds of state, must be untouched.
type principalKill struct {
	f                                         *killFixture
	p, other                                  Principal
	password                                  string
	pending, approved, executed, otherPending int64
	tok, otherTok                             string
	stmt, otherStmt                           *brokerStatement
	since                                     time.Time
	rep                                       KillReport
}

func newPrincipalKill(t *testing.T) *principalKill {
	t.Helper()
	k := &principalKill{f: newKillFixture(t)}
	f := k.f
	k.p, k.password = f.ensured(t)
	var otherPassword string
	k.other, otherPassword = f.ensured(t)
	k.pending = f.pendingApproval(t, k.p, "pending")
	k.approved = f.pendingApproval(t, k.p, "approved")
	k.executed = f.pendingApproval(t, k.p, "executed")
	k.otherPending = f.pendingApproval(t, k.other, "pending")
	k.tok, k.otherTok = f.agentToken(t, k.p).ID, f.agentToken(t, k.other).ID
	k.stmt = f.sleepAs(t, f.super, f.dsn, k.p.BrokerRole(), k.password, 30)
	k.otherStmt = f.sleepAs(t, f.super, f.dsn, k.other.BrokerRole(), otherPassword, 3)
	k.since = time.Now().Add(-time.Second)
	return k
}

func TestKill_PrincipalContainsEverything(t *testing.T) {
	k := newPrincipalKill(t)
	begin := time.Now()
	rep, err := k.f.sw.Kill(context.Background(), killPrincipal(k.p))
	require.NoError(t, err)
	require.Less(t, time.Since(begin), 10*time.Second)
	k.rep = rep
	k.checkSessions(t)
	k.checkControl(t)
	k.checkApprovals(t)
	k.checkRoles(t)
	k.checkReport(t)
	k.checkAudit(t)
}

// Step 6/8: the brokered statement is cancelled, no backend remains; the
// other principal's statement runs to its end.
func (k *principalKill) checkSessions(t *testing.T) {
	require.True(t, terminatedOrCancelled(waitDone(t, k.stmt, 10*time.Second)))
	require.Less(t, k.stmt.elapsed, 10*time.Second)
	require.Equal(t, 0, countBackends(t, k.f.super, k.p.BrokerRole(), k.p.LoginRole()))
	require.NoError(t, waitDone(t, k.otherStmt, 10*time.Second))
}

// Steps 1-2: frozen with the reason; tokens and in-memory pools revoked.
func (k *principalKill) checkControl(t *testing.T) {
	got := mustGet(t, k.f.store, k.p.ID)
	require.Equal(t, StatusFrozen, got.Status)
	require.Contains(t, got.FrozenReason, "incident 42")
	require.Equal(t, StatusActive, mustGet(t, k.f.store, k.other.ID).Status)
	require.True(t, k.f.tokenRevoked(t, k.tok))
	require.False(t, k.f.tokenRevoked(t, k.otherTok))
	require.Equal(t, int64(1), k.rep.TokensRevoked)
	require.Len(t, k.f.memory.calls, 1)
	require.Equal(t, []string{k.p.ID}, k.f.memory.calls[0])
	require.False(t, k.f.memory.all[0])
}

// Step 3: open approvals of p are cancelled_kill; executed ones and other
// principals' stay.
func (k *principalKill) checkApprovals(t *testing.T) {
	f := k.f
	require.Equal(t, "cancelled_kill", f.queueStatus(t, k.pending))
	require.Equal(t, "cancelled_kill", f.queueStatus(t, k.approved))
	require.Equal(t, "executed", f.queueStatus(t, k.executed))
	require.Equal(t, "pending", f.queueStatus(t, k.otherPending))
	require.Equal(t, 2, k.rep.ApprovalsCancelled)
}

// Step 5: NOLOGIN CONNECTION LIMIT 0 with prior attributes recorded.
func (k *principalKill) checkRoles(t *testing.T) {
	f, p := k.f, k.p
	for _, role := range []string{p.BrokerRole(), p.LoginRole()} {
		login, limit := f.attrs(t, role)
		require.False(t, login, role)
		require.Equal(t, 0, limit, role)
	}
	login, _ := f.attrs(t, k.other.BrokerRole())
	require.True(t, login)
	cr, err := f.store.ClusterRolesOf(context.Background(), p.ID, f.cluster.Key)
	require.NoError(t, err)
	require.Equal(t, RoleStatusKilled, cr.Status)
	prior, err := ParsePriorAttrs(cr.PriorAttrs)
	require.NoError(t, err)
	require.Equal(t, RoleAttrs{Login: true, ConnectionLimit: 2}, prior[p.BrokerRole()])
	require.Equal(t, RoleAttrs{Login: false, ConnectionLimit: 5}, prior[p.LoginRole()])
	require.True(t, f.loginFails(t, f.dsn, p.BrokerRole(), k.password))
}

// The report (§8.3) and its durable copy.
func (k *principalKill) checkReport(t *testing.T) {
	rep := k.rep
	require.Positive(t, rep.KillID)
	require.Equal(t, []string{k.p.ID}, rep.Principals)
	require.True(t, rep.Verified)
	require.Empty(t, rep.ControlError)
	require.Len(t, rep.Databases, 1)
	db := rep.Databases[0]
	require.Equal(t, k.f.db, db.Name)
	require.Equal(t, 2, db.RolesDisabled)
	require.GreaterOrEqual(t, db.BackendsTerminated, 1)
	require.True(t, db.Verified)
	require.False(t, db.Direct)
	require.Positive(t, db.ActionID)
	require.Empty(t, db.Error)
	var stored []byte
	var finished bool
	require.NoError(t, k.f.super.QueryRow(context.Background(), `SELECT
		report::text::bytea, finished_at IS NOT NULL FROM sage.guard_kills WHERE id = $1`,
		rep.KillID).Scan(&stored, &finished))
	require.True(t, finished)
	var back KillReport
	require.NoError(t, json.Unmarshal(stored, &back))
	require.Equal(t, db.RolesDisabled, back.Databases[0].RolesDisabled)
}

// The kill flag on the principal and one audited guard_kill action with
// its statements; the gated path writes no fallback entry.
func (k *principalKill) checkAudit(t *testing.T) {
	ctx := context.Background()
	var killID *int64
	require.NoError(t, k.f.super.QueryRow(ctx, `SELECT kill_id FROM sage.guard_freezes
		WHERE scope = 'principal' AND target = $1 AND cleared_at IS NULL`, k.p.ID).
		Scan(&killID))
	require.NotNil(t, killID)
	require.Equal(t, k.rep.KillID, *killID)
	require.Equal(t, 1, k.f.actionCount(t, "guard_kill", k.p, k.since))
	var sqlText string
	require.NoError(t, k.f.super.QueryRow(ctx, `SELECT sql_executed FROM sage.action_log
		WHERE id = $1`, k.rep.Databases[0].ActionID).Scan(&sqlText))
	require.Contains(t, sqlText, "NOLOGIN CONNECTION LIMIT 0")
	require.Contains(t, sqlText, k.p.BrokerRole())
	entries, err := k.f.fallback.Entries()
	require.NoError(t, err)
	require.Empty(t, entries, "the gated path writes no fallback entry")
}

func mustGet(t *testing.T, s *Store, id string) Principal {
	t.Helper()
	p, err := s.Get(context.Background(), id)
	require.NoError(t, err)
	return p
}

// Narrowing (§6.2.4): the kill works under the emergency stop, with the
// executor disabled, and at trust level observation.
func TestKill_WorksUnderEmergencyStopAndObservation(t *testing.T) {
	f := newKillFixture(t)
	p, password := f.ensured(t)
	f.setRuntime(func(r *policy.RuntimeState) {
		r.EmergencyStop, r.ExecutorEnabled = true, false
		r.TrustLevel, r.ExecutionMode = policy.TrustObservation, "manual"
	})
	stmt := f.sleepAs(t, f.super, f.dsn, p.BrokerRole(), password, 30)
	rep, err := f.sw.Kill(context.Background(), killPrincipal(p))
	require.NoError(t, err)
	require.True(t, terminatedOrCancelled(waitDone(t, stmt, 10*time.Second)))
	require.True(t, rep.Verified)
	require.False(t, rep.Databases[0].Direct, "narrowing passes the gate; no fallback")
	require.Positive(t, rep.Databases[0].ActionID)
	login, _ := f.attrs(t, p.BrokerRole())
	require.False(t, login)
}

// Kill is idempotent: a second kill keeps the first prior attributes, so
// unfreeze restores what the roles had before any kill.
func TestKill_IdempotentKeepsFirstPriorAttrs(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	rep, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	require.True(t, rep.Verified)
	cr, err := f.store.ClusterRolesOf(ctx, p.ID, f.cluster.Key)
	require.NoError(t, err)
	prior, err := ParsePriorAttrs(cr.PriorAttrs)
	require.NoError(t, err)
	require.Equal(t, RoleAttrs{Login: true, ConnectionLimit: 2}, prior[p.BrokerRole()])
	var open int
	require.NoError(t, f.super.QueryRow(ctx, `SELECT count(*)::int FROM sage.guard_freezes
		WHERE scope = 'principal' AND target = $1 AND cleared_at IS NULL`, p.ID).Scan(&open))
	require.Equal(t, 1, open)
}

// The direct fallback (§6.2.5): when the gate refuses or no executor is
// wired, containment still happens and a local log records it.
func TestKill_DirectWhenGateUnavailable(t *testing.T) {
	f := newKillFixture(t)
	p, password := f.ensured(t)
	f.exec.WithPolicyGate(policy.NewGate(policy.GateConfig{}))
	stmt := f.sleepAs(t, f.super, f.dsn, p.BrokerRole(), password, 30)
	rep, err := f.sw.Kill(context.Background(), killPrincipal(p))
	require.NoError(t, err)
	require.True(t, terminatedOrCancelled(waitDone(t, stmt, 10*time.Second)))
	require.True(t, rep.Databases[0].Direct)
	require.True(t, rep.Databases[0].Verified)
	login, _ := f.attrs(t, p.BrokerRole())
	require.False(t, login)
	entries, err := f.fallback.Entries()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "guard_kill", entries[0].ActionType)
	require.Equal(t, f.db, entries[0].Database)
	require.Equal(t, "success", entries[0].Outcome)
}

func TestKill_DirectWithoutExecutor(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	f.targets[0].Executor = nil
	f.rebuild(t)
	rep, err := f.sw.Kill(context.Background(), killPrincipal(p))
	require.NoError(t, err)
	require.True(t, rep.Databases[0].Direct)
	require.Equal(t, 2, rep.Databases[0].RolesDisabled)
	entries, err := f.fallback.Entries()
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

// With the control database down the kill continues per database
// (§6.10 step 1): role names follow from the principal id.
func TestKill_ControlDatabaseDown(t *testing.T) {
	f := newKillFixture(t)
	p, password := f.ensured(t)
	f.store = NewStore(nil)
	f.rebuild(t)
	stmt := f.sleepAs(t, f.super, f.dsn, p.BrokerRole(), password, 30)
	rep, err := f.sw.Kill(context.Background(), killPrincipal(p))
	require.NoError(t, err)
	require.True(t, terminatedOrCancelled(waitDone(t, stmt, 10*time.Second)))
	require.NotEmpty(t, rep.ControlError)
	require.Zero(t, rep.KillID)
	require.Equal(t, 2, rep.Databases[0].RolesDisabled)
	require.True(t, rep.Verified)
	entries, err := f.fallback.Entries()
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the audit is queued locally")
}

// Kill all: every principal, every agent role on the cluster (even one
// pg_sage never registered), and the fleet flag.
func TestKill_AllFreezesFleet(t *testing.T) {
	requireDedicatedServer(t)
	f := newKillFixture(t)
	a, pa := f.ensured(t)
	b, _ := f.ensured(t)
	ctx := context.Background()
	stray := "sage_agentb_" + strings.Repeat("z", 10)
	_, err := f.admin.Exec(ctx, "CREATE ROLE "+stray+" LOGIN PASSWORD 'stray-pw'")
	require.NoError(t, err)
	t.Cleanup(func() { f.dropRole(t, stray) })
	stmt := f.sleepAs(t, f.super, f.dsn, a.BrokerRole(), pa, 30)
	rep, err := f.sw.Kill(ctx, KillRequest{Scope: KillScopeAll, Reason: "fleet incident",
		Actor: "admin@example.com"})
	require.NoError(t, err)
	require.True(t, terminatedOrCancelled(waitDone(t, stmt, 10*time.Second)))
	for _, role := range []string{a.BrokerRole(), b.BrokerRole(), stray} {
		login, limit := f.attrs(t, role)
		require.False(t, login, role)
		require.Equal(t, 0, limit, role)
	}
	require.Equal(t, StatusFrozen, mustGet(t, f.store, a.ID).Status)
	require.Equal(t, StatusFrozen, mustGet(t, f.store, b.ID).Status)
	require.Contains(t, rep.Principals, a.ID)
	require.Contains(t, rep.Principals, b.ID)
	require.GreaterOrEqual(t, rep.Databases[0].RolesDisabled, 5)
	frozen, reason, err := f.sw.Frozen(ctx, validPID, "any-database")
	require.NoError(t, err)
	require.True(t, frozen, "the fleet flag binds every principal and database")
	require.Contains(t, reason, "fleet incident")
	require.True(t, f.memory.all[len(f.memory.all)-1])
}

// A fleet kill also ends sessions of lookalike roles (^sage_agentb?_ but
// not an agent role name, as CheckBackends flags them) and names them in
// the report; it leaves their attributes alone (pg_sage does not own them).
func TestKill_AllEndsAndReportsLookalikes(t *testing.T) {
	requireDedicatedServer(t)
	f := newKillFixture(t)
	ctx := context.Background()
	look := "sage_agent_lookalike"
	_, err := f.super.Exec(ctx, "CREATE ROLE "+look+" LOGIN PASSWORD 'look-pw'")
	require.NoError(t, err)
	_, err = f.super.Exec(ctx, "GRANT CONNECT ON DATABASE "+ident(f.db)+" TO "+look)
	require.NoError(t, err)
	t.Cleanup(func() { f.dropRole(t, look) })
	require.False(t, IsAgentRoleName(look))
	stmt := f.sleepAs(t, f.super, f.dsn, look, "look-pw", 30)
	rep, err := f.sw.Kill(ctx, KillRequest{Scope: KillScopeAll, Reason: "lookalike",
		Actor: "admin@example.com"})
	require.NoError(t, err)
	require.True(t, terminatedOrCancelled(waitDone(t, stmt, 10*time.Second)))
	require.Contains(t, rep.Databases[0].Lookalikes, look)
	login, _ := f.attrs(t, look)
	require.True(t, login, "a lookalike role is reported, not altered")
	// Roles other tests left on the shared server may not be pg_sage's to
	// disable; the lookalike itself never adds an error.
	require.NotContains(t, rep.Databases[0].Error, look)
}

// Kill one database: its flag, its sessions and its approvals; roles are
// cluster-wide, so they keep their login for the cluster's other
// databases.
func TestKill_DatabaseScope(t *testing.T) {
	f := newKillFixture(t)
	p, password := f.ensured(t)
	ctx := context.Background()
	pending := f.pendingApproval(t, p, "pending")
	stmt := f.sleepAs(t, f.super, f.dsn, p.BrokerRole(), password, 30)
	rep, err := f.sw.Kill(ctx, KillRequest{Scope: KillScopeDatabase, ID: f.db,
		Reason: "db incident", Actor: "admin@example.com"})
	require.NoError(t, err)
	require.True(t, terminatedOrCancelled(waitDone(t, stmt, 10*time.Second)))
	require.Equal(t, "cancelled_kill", f.queueStatus(t, pending))
	require.Equal(t, StatusActive, mustGet(t, f.store, p.ID).Status)
	login, _ := f.attrs(t, p.BrokerRole())
	require.True(t, login)
	require.Equal(t, 0, rep.Databases[0].RolesDisabled)
	frozen, reason, err := f.sw.Frozen(ctx, p.ID, f.db)
	require.NoError(t, err)
	require.True(t, frozen)
	require.Contains(t, reason, "db incident")
	frozen, _, err = f.sw.Frozen(ctx, p.ID, "another-db")
	require.NoError(t, err)
	require.False(t, frozen)
}

func TestKill_UnknownTargets(t *testing.T) {
	f := newKillFixture(t)
	ctx := context.Background()
	_, err := f.sw.Kill(ctx, KillRequest{Scope: KillScopePrincipal,
		ID: "agp_" + strings.Repeat("q", 20), Reason: "r", Actor: "a"})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.sw.Kill(ctx, KillRequest{Scope: KillScopeDatabase, ID: "no-such-db",
		Reason: "r", Actor: "a"})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = f.sw.Kill(ctx, KillRequest{Scope: "fleet", Reason: "r", Actor: "a"})
	require.ErrorIs(t, err, ErrInvalid)
}

// pg_sage's role needs pg_signal_backend to end agent sessions it does not
// inherit; without it the roles are still disabled and the report names
// the grant.
func TestKill_WithoutSignalPrivilege(t *testing.T) {
	f := newKillFixture(t)
	f.grantSignal(t, false)
	p, password := f.ensured(t)
	stmt := f.sleepAs(t, f.super, f.dsn, p.BrokerRole(), password, 5)
	rep, err := f.sw.Kill(context.Background(), killPrincipal(p))
	require.NoError(t, err)
	_ = waitDone(t, stmt, 15*time.Second)
	login, _ := f.attrs(t, p.BrokerRole())
	require.False(t, login, "NOLOGIN does not need the signal privilege")
	require.False(t, rep.Databases[0].Verified)
	require.False(t, rep.Verified)
	require.Contains(t, rep.Databases[0].Error, "pg_signal_backend")
}

// Concurrent kills of overlapping principals finish without a deadlock.
func TestKill_ConcurrentNoDeadlock(t *testing.T) {
	f := newKillFixture(t)
	a, _ := f.ensured(t)
	b, _ := f.ensured(t)
	reqs := []KillRequest{killPrincipal(a), killPrincipal(b), killPrincipal(a),
		killPrincipal(b), killPrincipal(a)}
	errs := make(chan error, len(reqs))
	for _, r := range reqs {
		go func(r KillRequest) {
			_, err := f.sw.Kill(context.Background(), r)
			errs <- err
		}(r)
	}
	deadline := time.After(30 * time.Second)
	for range reqs {
		select {
		case err := <-errs:
			require.NoError(t, err)
		case <-deadline:
			t.Fatal("concurrent kills did not finish")
		}
	}
	require.Equal(t, StatusFrozen, mustGet(t, f.store, a.ID).Status)
	require.Equal(t, StatusFrozen, mustGet(t, f.store, b.ID).Status)
}

// An operator freeze (§8.3 POST /agents/{id}/freeze) contains like a
// principal kill but keeps the tokens and is not a kill: its unfreeze
// needs one admin.
func TestFreeze_Operator(t *testing.T) {
	f := newKillFixture(t)
	p, password := f.ensured(t)
	ctx := context.Background()
	pending := f.pendingApproval(t, p, "pending")
	tok := f.agentToken(t, p)
	stmt := f.sleepAs(t, f.super, f.dsn, p.BrokerRole(), password, 30)
	since := time.Now().Add(-time.Second)
	rep, err := f.sw.Freeze(ctx, FreezeRequest{PrincipalID: p.ID, Reason: "odd reads",
		Actor: "op@example.com"})
	require.NoError(t, err)
	require.True(t, terminatedOrCancelled(waitDone(t, stmt, 10*time.Second)))
	require.True(t, rep.Verified)
	require.Zero(t, rep.KillID)
	require.Equal(t, StatusFrozen, mustGet(t, f.store, p.ID).Status)
	require.Equal(t, "cancelled_kill", f.queueStatus(t, pending))
	require.False(t, f.tokenRevoked(t, tok.ID), "a freeze keeps the tokens")
	login, _ := f.attrs(t, p.BrokerRole())
	require.False(t, login)
	var killID *int64
	require.NoError(t, f.super.QueryRow(ctx, `SELECT kill_id FROM sage.guard_freezes
		WHERE scope = 'principal' AND target = $1 AND cleared_at IS NULL`, p.ID).Scan(&killID))
	require.Nil(t, killID)
	require.Equal(t, 1, f.actionCount(t, "guard_freeze", p, since))
	require.Equal(t, 0, f.actionCount(t, "guard_kill", p, since))
	_, err = f.sw.Freeze(ctx, FreezeRequest{PrincipalID: "agp_" + strings.Repeat("q", 20),
		Reason: "r", Actor: "a"})
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFrozen_NoFlags(t *testing.T) {
	f := newKillFixture(t)
	frozen, reason, err := f.sw.Frozen(context.Background(), validPID, f.db)
	require.NoError(t, err)
	require.False(t, frozen)
	require.Empty(t, reason)
	down, err := NewSwitch(KillDeps{Store: NewStore(nil), Config: DefaultKillConfig(),
		Targets: func(context.Context) ([]KillTarget, error) { return nil, nil }})
	require.NoError(t, err)
	_, _, err = down.Frozen(context.Background(), validPID, f.db)
	require.ErrorIs(t, err, ErrUnavailable, "D1 fails closed without the control database")
}

// The fallback log is reconciled into action_log once the control
// database is back, and not twice.
func TestFallback_Reconcile(t *testing.T) {
	f := newKillFixture(t)
	require.NoError(t, f.fallback.Append(FallbackEntry{ActionType: "guard_kill",
		Database: f.db, Scope: "principal", PrincipalID: validPID, Reason: "r", Actor: "a",
		Statements: []string{"ALTER ROLE x NOLOGIN CONNECTION LIMIT 0"},
		Outcome:    "success"}))
	since := time.Now().Add(-time.Second)
	n, err := f.sw.ReconcileFallback(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, 1, f.actionCount(t, "guard_kill", Principal{ID: validPID}, since))
	entries, err := f.fallback.Entries()
	require.NoError(t, err)
	require.Empty(t, entries)
	n, err = f.sw.ReconcileFallback(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)
}

func TestInflight_RegisterAndKillCancels(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	const dbID = "00000000-0000-4000-8000-00000000c111"
	f.targets[0].DatabaseID = dbID
	f.rebuild(t)
	require.ErrorIs(t, RegisterInflight(ctx, f.super, p.ID, "not-a-uuid", 1, 0), ErrInvalid)
	require.ErrorIs(t, RegisterInflight(ctx, f.super, "bad", dbID, 1, 0), ErrInvalid)
	require.ErrorIs(t, RegisterInflight(ctx, f.super, p.ID, dbID, 0, 0), ErrInvalid)
	// An executor apply for p runs on pg_sage's own connection.
	conn, err := f.admin.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var pid int
	require.NoError(t, conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid))
	require.NoError(t, RegisterInflight(ctx, f.super, p.ID, dbID, pid, 9))
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, "SELECT pg_sleep(30)")
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	rep, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	select {
	case err := <-done:
		require.True(t, terminatedOrCancelled(err))
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight apply was not cancelled")
	}
	require.Equal(t, 1, rep.Databases[0].StatementsCancelled)
	require.NoError(t, UnregisterInflight(ctx, f.super, dbID, pid))
	var left int
	require.NoError(t, f.super.QueryRow(ctx, `SELECT count(*)::int FROM sage.guard_inflight
		WHERE database_id = $1`, dbID).Scan(&left))
	require.Zero(t, left)
}
