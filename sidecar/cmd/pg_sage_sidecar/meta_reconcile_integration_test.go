package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

type metaReconcileEnv struct {
	state  *metaDBState
	userID int
	admin  *pgxpool.Pool
}

func newMetaReconcileEnv(t *testing.T) *metaReconcileEnv {
	t.Helper()
	state, _, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	oldMeta := globalMetaState
	globalMetaState = state
	t.Cleanup(func() { globalMetaState = oldMeta })
	cfg.LLM.FleetTokenBudgetDaily = 10000
	initializeFleetBudget(nil)
	return &metaReconcileEnv{state: state, userID: userID, admin: reloadAdminPool(t)}
}

// record inserts a sage.databases row directly, the way another replica or
// an operator's SQL would, without going through this process's API.
func (env *metaReconcileEnv) record(t *testing.T, name string) store.DatabaseRecord {
	t.Helper()
	db := reloadDatabaseConfig(t, name, testdb.CreateDatabase(t, "meta_"+name))
	id, err := env.state.Store.Create(context.Background(), store.DatabaseInput{
		Name: name, Host: db.Host, Port: db.Port, DatabaseName: db.Database,
		Username: db.User, Password: db.Password, SSLMode: "disable",
		MaxConnections: 3, TrustLevel: "observation", ExecutionMode: "auto",
	}, env.userID)
	if err != nil {
		t.Fatalf("create record %q: %v", name, err)
	}
	rec, err := env.state.Store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read record %q: %v", name, err)
	}
	return *rec
}

func (env *metaReconcileEnv) reconcile(t *testing.T) metaReconcileReport {
	t.Helper()
	return reconcileMetaDatabases(context.Background(), fleetMgr, env.state)
}

func inputFromRecord(rec store.DatabaseRecord) store.DatabaseInput {
	return store.DatabaseInput{
		Name: rec.Name, Host: rec.Host, Port: rec.Port, DatabaseName: rec.DatabaseName,
		Username: rec.Username, SSLMode: rec.SSLMode, MaxConnections: rec.MaxConnections,
		Tags: rec.Tags, TrustLevel: rec.TrustLevel, ExecutionMode: rec.ExecutionMode,
	}
}

func TestMetaReconcileAddsOutOfBandRecordOnce(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "oob")
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Added, []string{"oob"}) || len(report.Errors) != 0 {
		t.Fatalf("report = %+v, want oob added", report)
	}
	inst := fleetMgr.GetInstance("oob")
	assertManagedRuntime(t, inst, rec.ID, "oob")
	if _, ok := fleetLLMBudget.Snapshot()["oob"]; !ok {
		t.Fatal("added database has no budget share")
	}
	again := env.reconcile(t)
	if !again.empty() || fleetMgr.GetInstance("oob") != inst {
		t.Fatalf("an unchanged pass changed the fleet: %+v", again)
	}
}

func TestMetaReconcileRemovesDeletedAndDisabledRecords(t *testing.T) {
	env := newMetaReconcileEnv(t)
	deleted, disabled := env.record(t, "deleted"), env.record(t, "disabled")
	env.reconcile(t)
	oldDeleted, oldDisabled := fleetMgr.GetInstance("deleted"), fleetMgr.GetInstance("disabled")
	if oldDeleted == nil || oldDisabled == nil {
		t.Fatal("fixture runtimes missing")
	}
	ctx := context.Background()
	if err := env.state.Store.Delete(ctx, deleted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.state.Pool.Exec(ctx,
		"UPDATE sage.databases SET enabled = false WHERE id = $1", disabled.ID); err != nil {
		t.Fatal(err)
	}
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Removed, []string{"deleted", "disabled"}) {
		t.Fatalf("removed = %v", report.Removed)
	}
	if fleetMgr.InstanceCount() != 0 || !poolClosed(oldDeleted.Pool) ||
		!poolClosed(oldDisabled.Pool) {
		t.Fatal("removed runtimes are still registered or connected")
	}
	snapshot := fleetLLMBudget.Snapshot()
	if _, ok := snapshot["deleted"]; ok {
		t.Fatal("deleted database kept its budget share")
	}
	for _, rec := range []store.DatabaseRecord{deleted, disabled} {
		database := rec.DatabaseName
		eventually(t, 5*time.Second, rec.Name+" backends to close", func() bool {
			return pgSageConnections(t, env.admin, database) == 0
		})
	}
}

func TestMetaReconcileRebuildsConnectionAndNameChanges(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "conn")
	env.reconcile(t)
	old := fleetMgr.GetInstance("conn")
	input := inputFromRecord(rec)
	input.Name, input.MaxConnections = "conn-renamed", 4
	if err := env.state.Store.Update(context.Background(), rec.ID, input); err != nil {
		t.Fatal(err)
	}
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Rebuilt, []string{"conn-renamed"}) {
		t.Fatalf("report = %+v", report)
	}
	inst := fleetMgr.GetInstance("conn-renamed")
	assertManagedRuntime(t, inst, rec.ID, "conn-renamed")
	if fleetMgr.GetInstance("conn") != nil || inst == old || !poolClosed(old.Pool) {
		t.Fatal("rename left the old generation running or registered")
	}
	if inst.Pool.Config().MaxConns != 4 {
		t.Fatalf("rebuilt pool max = %d, want 4", inst.Pool.Config().MaxConns)
	}
	if _, ok := fleetLLMBudget.Snapshot()["conn"]; ok {
		t.Fatal("renamed database kept the old budget name")
	}
}

func TestMetaReconcileAppliesPolicyColumnsInPlace(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "policy")
	env.reconcile(t)
	old := fleetMgr.GetInstance("policy")
	if _, err := env.state.Pool.Exec(context.Background(),
		`UPDATE sage.databases SET trust_level = 'advisory', execution_mode = 'manual'
		 WHERE id = $1`, rec.ID); err != nil {
		t.Fatal(err)
	}
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Updated, []string{"policy"}) || len(report.Rebuilt) != 0 {
		t.Fatalf("report = %+v, want an in-place update", report)
	}
	inst := fleetMgr.GetInstance("policy")
	if inst != old || poolClosed(old.Pool) {
		t.Fatal("a policy-only change rebuilt the runtime")
	}
	if inst.Executor.TrustLevel() != "advisory" || inst.Executor.ExecutionMode() != "manual" {
		t.Fatalf("executor = %s/%s", inst.Executor.TrustLevel(), inst.Executor.ExecutionMode())
	}
	if inst.Config.TrustLevel != "advisory" || inst.SnapshotStatus().TrustLevel != "advisory" {
		t.Fatal("instance metadata does not show the new trust level")
	}
}

func TestMetaReconcileUnreachableChangeKeepsRuntime(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "keep")
	env.reconcile(t)
	old := fleetMgr.GetInstance("keep")
	input := inputFromRecord(rec)
	input.Port = 1
	if err := env.state.Store.Update(context.Background(), rec.ID, input); err != nil {
		t.Fatal(err)
	}
	report := env.reconcile(t)
	if len(report.Errors) != 1 || !errors.Is(report.Errors[0], errMetaRuntimeCandidate) ||
		!strings.Contains(report.Errors[0].Error(), `"keep"`) {
		t.Fatalf("errors = %v, want one errMetaRuntimeCandidate naming keep", report.Errors)
	}
	if fleetMgr.GetInstance("keep") != old || poolClosed(old.Pool) {
		t.Fatal("an unreachable change replaced the running runtime")
	}
	if err := healthCheckStoreDatabase(context.Background(), old); err != nil {
		t.Fatalf("old runtime unhealthy: %v", err)
	}
}

func TestMetaReconcileRemovesFailedPlaceholder(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "ghost")
	registerFailedInstance(rec, "connection refused")
	if err := env.state.Store.Delete(context.Background(), rec.ID); err != nil {
		t.Fatal(err)
	}
	report := env.reconcile(t)
	if !reflect.DeepEqual(report.Removed, []string{"ghost"}) || fleetMgr.GetInstance("ghost") != nil {
		t.Fatalf("failed placeholder for a deleted record survived: %+v", report)
	}
}

func TestMetaReconcileListFailureChangesNothing(t *testing.T) {
	env := newMetaReconcileEnv(t)
	env.record(t, "steady")
	env.reconcile(t)
	steady := fleetMgr.GetInstance("steady")
	closed, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	broken := &metaDBState{Pool: closed, Store: store.NewDatabaseStore(closed, nil)}
	report := reconcileMetaDatabases(context.Background(), fleetMgr, broken)
	if len(report.Errors) == 0 || !strings.Contains(report.Errors[0].Error(), "list") {
		t.Fatalf("errors = %v, want a list failure", report.Errors)
	}
	if fleetMgr.GetInstance("steady") != steady || poolClosed(steady.Pool) {
		t.Fatal("an unreadable store removed a running database")
	}
}

func TestMetaReconcileConcurrentPassesPublishOneRuntime(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "race")
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reconcileMetaDatabases(context.Background(), fleetMgr, env.state)
		}()
	}
	wg.Wait()
	if fleetMgr.InstanceCount() != 1 {
		t.Fatalf("instances = %d, want exactly one", fleetMgr.InstanceCount())
	}
	assertManagedRuntime(t, fleetMgr.GetInstance("race"), rec.ID, "race")
	eventually(t, 10*time.Second, "losing candidates to close", func() bool {
		return pgSageConnections(t, env.admin, rec.DatabaseName) <= rec.MaxConnections
	})
}

func TestMetaReconcileConcurrentWithAPIUpdateConverges(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "api")
	env.reconcile(t)
	input := inputFromRecord(rec)
	input.MaxConnections = 5
	var wg sync.WaitGroup
	wg.Add(2)
	var apiErr error
	go func() {
		defer wg.Done()
		_, apiErr = applyMetaDatabaseUpdate(context.Background(), fleetMgr,
			env.state, rec.ID, rec, input)
	}()
	go func() {
		defer wg.Done()
		reconcileMetaDatabases(context.Background(), fleetMgr, env.state)
	}()
	wg.Wait()
	if apiErr != nil {
		t.Fatalf("API update: %v", apiErr)
	}
	env.reconcile(t)
	inst := fleetMgr.GetInstance("api")
	if fleetMgr.InstanceCount() != 1 || inst.Pool.Config().MaxConns != 5 {
		t.Fatalf("fleet did not converge on the stored record: %+v", inst.Config)
	}
}

func TestMetaReconcileCyclesLeakNothing(t *testing.T) {
	env := newMetaReconcileEnv(t)
	rec := env.record(t, "cycle")
	cycle := func() {
		setEnabled(t, env, rec.ID, true)
		if report := env.reconcile(t); len(report.Added) != 1 {
			t.Fatalf("add pass = %+v", report)
		}
		setEnabled(t, env, rec.ID, false)
		if report := env.reconcile(t); len(report.Removed) != 1 {
			t.Fatalf("remove pass = %+v", report)
		}
	}
	setEnabled(t, env, rec.ID, false)
	env.reconcile(t)
	cycle()
	baseline := settledGoroutines(0, 2*time.Second)
	for range 3 {
		cycle()
	}
	if got := settledGoroutines(baseline+2, 15*time.Second); got > baseline+2 {
		t.Fatalf("goroutines grew from %d to %d over 3 cycles", baseline, got)
	}
	eventually(t, 5*time.Second, "cycle backends to close", func() bool {
		return pgSageConnections(t, env.admin, rec.DatabaseName) == 0
	})
}

func setEnabled(t *testing.T, env *metaReconcileEnv, id int, enabled bool) {
	t.Helper()
	if _, err := env.state.Pool.Exec(context.Background(),
		"UPDATE sage.databases SET enabled = $1 WHERE id = $2", enabled, id); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
}
