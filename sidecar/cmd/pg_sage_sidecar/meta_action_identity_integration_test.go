package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/crypto"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-09b integration: a meta-DB member with a stored action
// credential runs two identities; adding, changing and removing the
// credential rebuilds the member once per change.

// actionMember records a member whose action identity is a fresh role.
func (env *metaReconcileEnv) actionMember(t *testing.T, name string) (store.DatabaseRecord,
	string) {
	t.Helper()
	rec := env.record(t, name)
	role, actionDSN := actionRoleFixture(t, testdb.SkipUnlessLive(t))
	u, _ := url.Parse(actionDSN)
	pw, _ := u.User.Password()
	in := inputFromRecord(rec)
	in.Action = &store.ActionCredentialInput{Username: role, Password: pw}
	if err := env.state.Store.Update(context.Background(), rec.ID, in); err != nil {
		t.Fatalf("store the action credential: %v", err)
	}
	return rec, role
}

func assertMemberActionRole(t *testing.T, inst *fleet.DatabaseInstance, observer, role string) {
	t.Helper()
	if inst == nil || inst.Pool == nil {
		t.Fatal("member has no connected runtime")
	}
	if inst.Pool.Config().ConnConfig.User != observer {
		t.Fatalf("observation pool runs as %q, want %q", inst.Pool.Config().ConnConfig.User,
			observer)
	}
	if role == "" {
		if inst.ActionPool != nil || inst.Executor.HasActionIdentity() {
			t.Fatal("a member without a credential has an action identity")
		}
		return
	}
	if inst.ActionPool == nil || !inst.Executor.HasActionIdentity() {
		t.Fatal("the member's executor has no action identity")
	}
	if got := inst.ActionPool.Config().ConnConfig.User; got != role {
		t.Fatalf("action pool runs as %q, want %q", got, role)
	}
	ctx := context.Background()
	conn, err := inst.ActionPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire an action connection: %v", err)
	}
	var who string
	err = conn.QueryRow(ctx, "SELECT current_user::text").Scan(&who)
	users := sidecarUsers(t, inst.Pool, observer, role)
	conn.Release()
	if err != nil || who != role || !users[role] || !users[observer] {
		t.Fatalf("action session runs as %q (%v); pg_sage roles %v", who, err, users)
	}
}

// Happy path and state transitions: add, change, remove the credential;
// each pass rebuilds once and the running action role matches the row.
func TestMetaReconcileActionCredentialTransitions(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec, role := env.actionMember(t, "actor_member")
	observer := rec.Username
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Added, []string{"actor_member"}) || len(report.Errors) != 0 {
		t.Fatalf("report = %+v, want the member added", report)
	}
	assertMemberActionRole(t, fleetMgr.GetInstance("actor_member"), observer, role)
	if again := env.reconcile(t); !again.empty() {
		t.Fatalf("an unchanged pass changed the fleet: %+v", again)
	}
	ctx := context.Background()
	if _, err := env.admin.Exec(ctx, "ALTER ROLE "+role+" PASSWORD 'act-pw-2'"); err != nil {
		t.Fatal(err)
	}
	in := inputFromRecord(rec)
	in.Action = &store.ActionCredentialInput{Username: role, Password: "act-pw-2"}
	env.updateAndExpectRebuild(t, rec.ID, in)
	inst := fleetMgr.GetInstance("actor_member")
	assertMemberActionRole(t, inst, observer, role)
	if inst.ActionPool.Config().ConnConfig.Password != "act-pw-2" {
		t.Fatal("the rebuilt action pool kept the old password")
	}
	in.Action = &store.ActionCredentialInput{}
	env.updateAndExpectRebuild(t, rec.ID, in)
	assertMemberActionRole(t, fleetMgr.GetInstance("actor_member"), observer, "")
}

func (env *metaReconcileEnv) updateAndExpectRebuild(t *testing.T, id int,
	in store.DatabaseInput) {
	t.Helper()
	if err := env.state.Store.Update(context.Background(), id, in); err != nil {
		t.Fatalf("update: %v", err)
	}
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Rebuilt, []string{in.Name}) || len(report.Errors) != 0 {
		t.Fatalf("report = %+v, want %s rebuilt once", report, in.Name)
	}
	if again := env.reconcile(t); !again.empty() {
		t.Fatalf("the pass after a rebuild changed the fleet: %+v", again)
	}
}

// Error propagation: an action password no key opens leaves the member
// observing, and its action identity refuses every change naming the
// database; nothing falls back to the observation role.
func TestMetaReconcileUnreadableActionCredentialFailsClosed(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec, role := env.actionMember(t, "lost_key")
	other := store.NewDatabaseStore(env.state.Pool,
		crypto.DeriveKey("another-key", []byte("another-salt-16b")))
	in := inputFromRecord(rec)
	in.Action = &store.ActionCredentialInput{Username: role, Password: "act-pw"}
	if err := other.Update(context.Background(), rec.ID, in); err != nil {
		t.Fatal(err)
	}
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Added, []string{"lost_key"}) || len(report.Errors) != 0 {
		t.Fatalf("report = %+v, want the member added", report)
	}
	inst := fleetMgr.GetInstance("lost_key")
	if err := healthCheckStoreDatabase(context.Background(), inst); err != nil {
		t.Fatalf("observation is not running: %v", err)
	}
	if inst.ActionPool == nil || !inst.Executor.HasActionIdentity() {
		t.Fatal("an unreadable credential fell back to the observation role")
	}
	err := inst.ActionPool.Ping(context.Background())
	if !errors.Is(err, executor.ErrActionIdentityUnavailable) ||
		!errors.Is(err, crypto.ErrWrongEncryptionKey) ||
		!strings.Contains(err.Error(), fmt.Sprintf("database %d", rec.ID)) {
		t.Fatalf("action ping = %v, want unavailable and wrong key naming database %d",
			err, rec.ID)
	}
	if again := env.reconcile(t); !again.empty() {
		t.Fatalf("a fail-closed member is rebuilt every pass: %+v", again)
	}
}

// Concurrency (ordering): two updates race; the next pass runs the
// member with whichever credential committed last. (-race is OWNER ACTION.)
func TestMetaReconcileUsesLastCommittedActionCredential(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec, role := env.actionMember(t, "racer")
	env.reconcile(t)
	var wg sync.WaitGroup
	for _, pw := range []string{"act-pw", "act-pw-other"} {
		wg.Add(1)
		go func(pw string) {
			defer wg.Done()
			in := inputFromRecord(rec)
			in.Action = &store.ActionCredentialInput{Username: role, Password: pw}
			_ = env.state.Store.Update(context.Background(), rec.ID, in)
		}(pw)
	}
	wg.Wait()
	stored, err := env.state.Store.GetActionConnectionString(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	env.reconcile(t)
	if got := runningActionDSN(fleetMgr.GetInstance("racer")); got != stored {
		t.Fatal("the running action pool is not the last committed credential")
	}
}

// Rotation: an action password sealed under key A is re-sealed to key B
// at startup (counted on its own line), and the member then builds with
// its action role under key B alone.
func TestMetaActionCredentialRotationReseals(t *testing.T) {
	env := newMetaReconcileEnv(t)
	keyA := crypto.DeriveKey("rotation-a", []byte("rotation-salt-16"))
	keyB := crypto.DeriveKey("rotation-b", []byte("rotation-salt-16"))
	rec := env.record(t, "rotated_member")
	role, actionDSN := actionRoleFixture(t, testdb.SkipUnlessLive(t))
	u, _ := url.Parse(actionDSN)
	pw, _ := u.User.Password()
	in := inputFromRecord(rec)
	in.Action = &store.ActionCredentialInput{Username: role, Password: pw}
	obs, err := env.state.Store.GetConnectionString(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	obsURL, _ := url.Parse(obs)
	in.Password, _ = obsURL.User.Password()
	if err := store.NewDatabaseStore(env.state.Pool, keyA).Update(context.Background(),
		rec.ID, in); err != nil {
		t.Fatal(err)
	}
	var lines []string
	logf := func(level, component, msg string, args ...any) {
		lines = append(lines, fmt.Sprintf(msg, args...))
	}
	rotated := store.NewDatabaseStore(env.state.Pool, keyB).WithPreviousKey(keyA)
	if err := resealStoredPasswords(context.Background(), rotated, logf); err != nil {
		t.Fatalf("reseal: %v", err)
	}
	if !containsLine(lines, "action passwords at rest: sealed 0, rotated 1") {
		t.Fatalf("reseal log = %q, want one rotated action password", lines)
	}
	env.state.Store = store.NewDatabaseStore(env.state.Pool, keyB)
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Added, []string{"rotated_member"}) || len(report.Errors) != 0 {
		t.Fatalf("report = %+v, want the member added", report)
	}
	assertMemberActionRole(t, fleetMgr.GetInstance("rotated_member"), rec.Username, role)
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// State transition through the API update path: a member running with an
// action password no key opens takes a re-entered credential at once (the
// published runtime acts as the role), and the next reconcile pass leaves
// it alone because the prospective DSN is the stored one.
func TestMetaUpdateReenteredActionCredentialActsAtOnce(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec, role := env.actionMember(t, "reentered")
	stored, err := env.state.Store.GetActionConnectionString(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(stored)
	pw, _ := u.User.Password()
	lost := store.NewDatabaseStore(env.state.Pool,
		crypto.DeriveKey("another-key", []byte("another-salt-16b")))
	in := inputFromRecord(rec)
	in.Action = &store.ActionCredentialInput{Username: role, Password: "sealed-elsewhere"}
	if err := lost.Update(context.Background(), rec.ID, in); err != nil {
		t.Fatal(err)
	}
	env.reconcile(t)
	if err := fleetMgr.GetInstance("reentered").ActionPool.Ping(
		context.Background()); !errors.Is(err, executor.ErrActionIdentityUnavailable) {
		t.Fatalf("unreadable credential ping = %v, want unavailable", err)
	}
	in.Action = &store.ActionCredentialInput{Username: role, Password: pw}
	if _, err := applyMetaDatabaseUpdate(context.Background(), fleetMgr, env.state,
		rec.ID, rec, in); err != nil {
		t.Fatalf("re-enter the action credential: %v", err)
	}
	assertMemberActionRole(t, fleetMgr.GetInstance("reentered"), rec.Username, role)
	if again := env.reconcile(t); !again.empty() {
		t.Fatalf("the pass after the API update changed the fleet: %+v", again)
	}
}
