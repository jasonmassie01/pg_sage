package agentguard

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

func TestEnsure_CreatesRolesToSpec(t *testing.T) {
	f := newRoleFixture(t)
	p := f.principal(t)
	ctx := context.Background()
	res, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	require.True(t, res.Created)
	require.True(t, res.Rotated)
	require.Positive(t, res.ActionID)
	require.Equal(t, p.BrokerRole(), res.BrokerRole)
	for role, login := range map[string]bool{p.BrokerRole(): true, p.LoginRole(): false} {
		st, err := ReadRoleState(ctx, f.super, role)
		require.NoError(t, err)
		require.Empty(t, st.AgentViolations(), role)
		require.Equal(t, login, st.CanLogin, role)
		require.Empty(t, st.MemberOf, role)
	}
	broker, err := ReadRoleState(ctx, f.super, p.BrokerRole())
	require.NoError(t, err)
	require.Equal(t, 2, broker.ConnLimit)
	login, err := ReadRoleState(ctx, f.super, p.LoginRole())
	require.NoError(t, err)
	require.Equal(t, 5, login.ConnLimit)
	who, _, err := f.brokerLogin(t, p)
	require.NoError(t, err, "the broker logs in with the stored credential")
	require.Equal(t, p.BrokerRole(), who)
	_, err = tryLogin(ctx, withUser(t, f.dsn, p.LoginRole(), "anything"))
	require.Error(t, err, "the direct-lane role is NOLOGIN in G1")
}

func TestEnsure_SettingsPerDatabase(t *testing.T) {
	f := newRoleFixture(t)
	p := f.principal(t)
	ctx := context.Background()
	res, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	var conf []string
	require.NoError(t, f.super.QueryRow(ctx, `SELECT s.setconfig FROM pg_db_role_setting s
		JOIN pg_roles r ON r.oid = s.setrole JOIN pg_database d ON d.oid = s.setdatabase
		WHERE r.rolname = $1 AND d.datname = $2`, p.BrokerRole(), f.db).Scan(&conf))
	for _, want := range []string{"statement_timeout=30000ms", "lock_timeout=2000ms",
		"idle_in_transaction_session_timeout=60000ms", "idle_session_timeout=600000ms"} {
		require.Contains(t, conf, want)
	}
	if serverVersionNum(t, f.super) >= 170000 {
		require.Contains(t, conf, "transaction_timeout=600000ms")
	}
	// temp_file_limit is superuser-only: pg_sage's CREATEROLE role cannot
	// set it, so it is skipped and recorded (G1-13), not fatal.
	require.Contains(t, res.SkippedSettings, "temp_file_limit")
	require.NotContains(t, conf, "temp_file_limit=1024MB")
	var readOnly []string
	require.NoError(t, f.super.QueryRow(ctx, `SELECT s.setconfig FROM pg_db_role_setting s
		JOIN pg_roles r ON r.oid = s.setrole WHERE r.rolname = $1`, p.LoginRole()).
		Scan(&readOnly))
	require.Contains(t, readOnly, "default_transaction_read_only=on")
	// The broker session really runs under the timeouts.
	_, password, err := f.brokerLogin(t, p)
	require.NoError(t, err)
	conn, err := tryShow(ctx, withUser(t, f.dsn, p.BrokerRole(), password),
		"statement_timeout")
	require.NoError(t, err)
	require.Equal(t, "30s", conn)
}

func TestEnsure_RecordsActionWithProvenanceAndNoSecret(t *testing.T) {
	f := newRoleFixture(t)
	p := f.principal(t)
	ctx := context.Background()
	res, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	var actionType, sql, principalID, outcome string
	var approvedBy int
	var approvalID int64
	require.NoError(t, f.super.QueryRow(ctx, `SELECT action_type, sql_executed, principal_id,
		outcome, approved_by, approval_id FROM sage.action_log WHERE id = $1`, res.ActionID).
		Scan(&actionType, &sql, &principalID, &outcome, &approvedBy, &approvalID))
	require.Equal(t, "guard_role_ensure", actionType)
	require.Equal(t, p.ID, principalID)
	require.Equal(t, "success", outcome)
	require.Equal(t, 1, approvedBy)
	require.Equal(t, int64(77), approvalID)
	require.True(t, containsAll(sql, "CREATE ROLE", p.BrokerRole(), "NOBYPASSRLS",
		"PASSWORD '<scram verifier>'", "GRANT CONNECT"), sql)
	require.False(t, strings.Contains(sql, "SCRAM-SHA-256$"), "no verifier in the log")
	_, password, err := f.brokerLogin(t, p)
	require.NoError(t, err)
	require.False(t, strings.Contains(sql, password))
	var ct []byte
	var keyID string
	require.NoError(t, f.super.QueryRow(ctx, `SELECT broker_secret_ct, key_id
		FROM sage.guard_cluster_roles WHERE principal_id = $1`, p.ID).Scan(&ct, &keyID))
	require.False(t, strings.Contains(string(ct), password), "sealed at rest")
	require.Equal(t, f.manager.keyring.ActiveKeyID(), keyID)
}

func TestEnsure_IdempotentKeepsCredentialRotateReplacesIt(t *testing.T) {
	f := newRoleFixture(t)
	p := f.principal(t)
	ctx := context.Background()
	_, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	_, first, err := f.brokerLogin(t, p)
	require.NoError(t, err)
	again, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	require.False(t, again.Created)
	require.False(t, again.Rotated)
	_, err = tryLogin(ctx, withUser(t, f.dsn, p.BrokerRole(), first))
	require.NoError(t, err, "a plain re-ensure keeps the password")
	req := f.request(p)
	req.RotateCredential = true
	rot, err := f.manager.Ensure(ctx, req)
	require.NoError(t, err)
	require.True(t, rot.Rotated)
	_, err = tryLogin(ctx, withUser(t, f.dsn, p.BrokerRole(), first))
	require.Error(t, err, "the old password fails after rotation (G1-06)")
	_, second, err := f.brokerLogin(t, p)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
}

func TestEnsure_RepairsAttributeDrift(t *testing.T) {
	f := newRoleFixture(t)
	p := f.principal(t)
	ctx := context.Background()
	_, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	_, err = f.super.Exec(ctx, "ALTER ROLE "+ident(p.BrokerRole())+" CONNECTION LIMIT 50")
	require.NoError(t, err)
	_, err = f.super.Exec(ctx, "ALTER ROLE "+ident(p.LoginRole())+" LOGIN")
	require.NoError(t, err)
	_, err = f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	st, err := ReadRoleState(ctx, f.super, p.BrokerRole())
	require.NoError(t, err)
	require.Equal(t, 2, st.ConnLimit)
	login, err := ReadRoleState(ctx, f.super, p.LoginRole())
	require.NoError(t, err)
	require.False(t, login.CanLogin)
	// CREATEDB (like SUPERUSER, REPLICATION, BYPASSRLS) can be removed only
	// by a role holding it; pg_sage's CREATEROLE role cannot, so the
	// post-check refuses and names it for a superuser to fix.
	_, err = f.super.Exec(ctx, "ALTER ROLE "+ident(p.BrokerRole())+" CREATEDB")
	require.NoError(t, err)
	_, err = f.manager.Ensure(ctx, f.request(p))
	require.ErrorIs(t, err, ErrPostCheck)
	require.ErrorContains(t, err, "has CREATEDB")
}

func TestEnsure_PostCheckRefusesDangerousMembershipAndOwnership(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	p := f.principal(t)
	_, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	_, err = f.super.Exec(ctx, "GRANT pg_write_server_files TO "+ident(p.BrokerRole()))
	require.NoError(t, err)
	_, err = f.manager.Ensure(ctx, f.request(p))
	require.ErrorIs(t, err, ErrPostCheck)
	require.ErrorContains(t, err, "pg_write_server_files")
	_, err = f.super.Exec(ctx, "REVOKE pg_write_server_files FROM "+ident(p.BrokerRole()))
	require.NoError(t, err)
	q := f.principal(t)
	_, err = f.manager.Ensure(ctx, f.request(q))
	require.NoError(t, err)
	_, err = f.super.Exec(ctx, "CREATE TABLE public.g1core_owned_"+q.ID[4:12]+
		" (id int); ALTER TABLE public.g1core_owned_"+q.ID[4:12]+" OWNER TO "+
		ident(q.LoginRole()))
	require.NoError(t, err)
	_, err = f.manager.Ensure(ctx, f.request(q))
	require.ErrorIs(t, err, ErrPostCheck)
	require.ErrorContains(t, err, "owns 1 objects")
}

func TestEnsure_Refusals(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	p := f.principal(t)
	noApproval := f.request(p)
	noApproval.Approval = Approval{}
	_, err := f.manager.Ensure(ctx, noApproval)
	require.ErrorIs(t, err, ErrApprovalRequired)
	m, err := NewRoleManager(f.store, nil, DefaultRoleConfig())
	require.NoError(t, err)
	_, err = m.Ensure(ctx, f.request(p))
	require.ErrorIs(t, err, ErrEncryptionKeyRequired)
	superReq := f.request(p)
	superReq.Cluster.Admin = f.super
	_, err = f.manager.Ensure(ctx, superReq)
	require.ErrorIs(t, err, ErrSelfCheck, "pg_sage must not run Guard as superuser")
	require.False(t, f.roleExists(t, p.BrokerRole()), "nothing was created")
	r := f.principal(t)
	_, err = f.store.SetStatus(ctx, r.ID, StatusRetired, "")
	require.NoError(t, err)
	_, err = f.manager.Ensure(ctx, f.request(r))
	require.ErrorIs(t, err, ErrRetired)
	ghost := f.request(p)
	ghost.PrincipalID = "agp_aaaaaaaaaaaaaaaaaaaa"
	_, err = f.manager.Ensure(ctx, ghost)
	require.ErrorIs(t, err, ErrNotFound)
}

func withheldReason(t *testing.T, err error) string {
	t.Helper()
	var w *executor.WithheldError
	require.True(t, errors.As(err, &w), "want a withheld verdict, got %v", err)
	return w.Decision.BlockedReason
}

func TestEnsure_GateBinds(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	p := f.principal(t)
	f.setRuntime(func(rt *policy.RuntimeState) { rt.TrustLevel = policy.TrustObservation })
	_, err := f.manager.Ensure(ctx, f.request(p))
	require.Equal(t, "observe_only", withheldReason(t, err),
		"G1: acting needs trust.level advisory or higher")
	f.setRuntime(func(rt *policy.RuntimeState) {
		rt.TrustLevel = policy.TrustAdvisory
		rt.EmergencyStop = true
	})
	_, err = f.manager.Ensure(ctx, f.request(p))
	require.Equal(t, "emergency_stop", withheldReason(t, err))
	f.setRuntime(func(rt *policy.RuntimeState) { rt.EmergencyStop = false })
	doc := policy.UnattendedProfile()
	doc.AllowedChangeClasses = []policy.ChangeClass{policy.ChangeIndex}
	f.document.Store(doc)
	_, err = f.manager.Ensure(ctx, f.request(p))
	require.Equal(t, "change_class_not_allowed", withheldReason(t, err))
	require.False(t, f.roleExists(t, p.BrokerRole()), "a withheld ensure creates nothing")
	f.document.Store(policy.UnattendedProfile())
	_, err = f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
}

func TestEnsure_ConcurrentSamePrincipal(t *testing.T) {
	f := newRoleFixture(t)
	p := f.principal(t)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.manager.Ensure(context.Background(), f.request(p))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "ensure %d", i)
	}
	who, _, err := f.brokerLogin(t, p)
	require.NoError(t, err, "the stored credential matches the role after the race")
	require.Equal(t, p.BrokerRole(), who)
}

func TestRetire_DropsRolesEverywhere(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	p := f.principal(t)
	_, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	// A grant pg_sage made (it holds the grant option), as Guard grants are.
	table := "public.g1core_retire_" + p.ID[4:12]
	var admin string
	require.NoError(t, f.admin.QueryRow(ctx, "SELECT current_user::text").Scan(&admin))
	_, err = f.super.Exec(ctx, "CREATE TABLE "+table+" (id int); GRANT SELECT ON "+table+
		" TO "+ident(admin)+" WITH GRANT OPTION")
	require.NoError(t, err)
	_, err = f.admin.Exec(ctx, "GRANT SELECT ON "+table+" TO "+ident(p.BrokerRole()))
	require.NoError(t, err)
	res, err := f.manager.Retire(ctx, f.request(p))
	require.NoError(t, err)
	require.Positive(t, res.ActionID)
	require.False(t, f.roleExists(t, p.BrokerRole()))
	require.False(t, f.roleExists(t, p.LoginRole()))
	self, err := SelfCheck(ctx, f.admin)
	require.NoError(t, err)
	require.Empty(t, self.Inherits, "pg_sage keeps no inherited agent role")
	roles, err := f.store.ClusterRoles(ctx, p.ID)
	require.NoError(t, err)
	require.Len(t, roles, 1)
	require.Equal(t, RoleStatusRetired, roles[0].Status)
	var actionType string
	require.NoError(t, f.super.QueryRow(ctx, `SELECT action_type FROM sage.action_log
		WHERE id = $1 AND principal_id = $2`, res.ActionID, p.ID).Scan(&actionType))
	require.Equal(t, "guard_role_retire", actionType)
	again, err := f.manager.Retire(ctx, f.request(p))
	require.NoError(t, err, "retire is idempotent")
	require.Positive(t, again.ActionID)
	_, _, err = f.store.BrokerCredential(ctx, f.manager.keyring, p.ID, f.cluster.Key)
	d, ok := IsDenied(err)
	require.True(t, ok, "a retired login has no usable credential: %v", err)
	require.Equal(t, ReasonFrozen, d.Reason)
}

func TestRetire_ForeignGrantorResidueRefuses(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	p := f.principal(t)
	_, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	table := "public.g1core_residue_" + p.ID[4:12]
	_, err = f.super.Exec(ctx, "CREATE TABLE "+table+" (id int); GRANT SELECT ON "+table+
		" TO "+ident(p.BrokerRole()))
	require.NoError(t, err)
	_, err = f.manager.Retire(ctx, f.request(p))
	require.ErrorIs(t, err, ErrPostCheck)
	require.ErrorContains(t, err, "another grantor")
	require.True(t, f.roleExists(t, p.BrokerRole()), "nothing dropped half-way")
}

func TestRetire_GateBinds(t *testing.T) {
	f := newRoleFixture(t)
	ctx := context.Background()
	p := f.principal(t)
	_, err := f.manager.Ensure(ctx, f.request(p))
	require.NoError(t, err)
	f.setRuntime(func(rt *policy.RuntimeState) { rt.TrustLevel = policy.TrustObservation })
	_, err = f.manager.Retire(ctx, f.request(p))
	require.Equal(t, "observe_only", withheldReason(t, err))
	require.True(t, f.roleExists(t, p.BrokerRole()))
}

func TestEnsure_RefusedBeforePG16(t *testing.T) {
	super := livePool(t)
	if serverVersionNum(t, super) >= MinRoleServerVersion {
		t.Skip("covers PostgreSQL 14 and 15 only; the server is 16+")
	}
	s := NewStore(super)
	sponsor := createUser(t, super, "admin")
	p := newPrincipal(t, s, &sponsor)
	m, err := NewRoleManager(s, testKeyring(t), DefaultRoleConfig())
	require.NoError(t, err)
	req := RoleRequest{PrincipalID: p.ID, Cluster: Cluster{Key: "old", Admin: super},
		Approval: Approval{ApprovedBy: 1}, Executor: executor.New(super, nil,
			time.Now(), nil)}
	_, err = m.Ensure(context.Background(), req)
	require.ErrorIs(t, err, ErrRoleManagementUnsupported)
	_, err = m.Retire(context.Background(), req)
	require.ErrorIs(t, err, ErrRoleManagementUnsupported)
}
