package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Global runtime owners run sequentially: every test owns the process
// globals (cfg, fleetMgr, configController) for its duration.

func TestFleetReloadAddsDatabaseWithFullRuntime(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	if fleetMgr.InstanceCount() != 2 {
		t.Fatalf("fixture instances = %d, want 2", fleetMgr.InstanceCount())
	}
	extra := testdbExtra(t, env, "c")
	before := configController.Active().Generation
	if err := env.reload(func(c *config.Config) {
		c.Databases = append(c.Databases, extra)
	}); err != nil {
		t.Fatalf("add reload: %v", err)
	}
	inst := fleetMgr.GetInstance("c")
	if inst == nil || inst.Pool == nil || inst.Collector == nil || inst.Executor == nil ||
		inst.Analyzer == nil || inst.Cancel == nil || inst.Workers == nil {
		t.Fatalf("added database has no complete runtime: %+v", inst)
	}
	if err := healthCheckStoreDatabase(context.Background(), inst); err != nil {
		t.Fatalf("added runtime unhealthy: %v", err)
	}
	if inst.Executor.TrustLevel() != "observation" || inst.Executor.ExecutionMode() != "auto" {
		t.Fatalf("added runtime policy = %s/%s", inst.Executor.TrustLevel(),
			inst.Executor.ExecutionMode())
	}
	if _, ok := fleetLLMBudget.Snapshot()["c"]; !ok {
		t.Fatal("added database has no fleet LLM budget share")
	}
	if configController.Active().Generation != before+1 {
		t.Fatal("active generation did not advance for an applied add")
	}
	if databaseIndex(cfg.Databases, "c") < 0 {
		t.Fatal("process config does not list the added database")
	}
}

func testdbExtra(t *testing.T, env *fleetReloadEnv, name string) config.DatabaseConfig {
	t.Helper()
	env.dsn[name] = testdb.CreateDatabase(t, "reload_"+name)
	return env.config(t, name)
}

func TestFleetReloadRemovesDatabaseAndCleansUp(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	old := fleetMgr.GetInstance("b")
	database := env.config(t, "b").Database
	if pgSageConnections(t, env.admin, database) == 0 {
		t.Fatal("fixture runtime holds no connections to measure")
	}
	if metrics := renderMetrics(t); !strings.Contains(metrics, `database="b"`) {
		t.Fatal("fixture metrics do not label database b")
	}
	if err := env.reload(func(c *config.Config) {
		c.Databases = withoutDatabase(c.Databases, "b")
	}); err != nil {
		t.Fatalf("remove reload: %v", err)
	}
	if fleetMgr.GetInstance("b") != nil || fleetMgr.InstanceCount() != 1 {
		t.Fatal("removed database is still registered")
	}
	if !poolClosed(old.Pool) {
		t.Fatal("removed runtime's pool is still open")
	}
	eventually(t, 5*time.Second, "removed database backends to close", func() bool {
		return pgSageConnections(t, env.admin, database) == 0
	})
	if _, ok := fleetLLMBudget.Snapshot()["b"]; ok {
		t.Fatal("removed database kept its fleet LLM budget share")
	}
	if metrics := renderMetrics(t); strings.Contains(metrics, `database="b"`) {
		t.Fatalf("metrics still carry the removed database label:\n%s", metrics)
	}
	if databaseIndex(cfg.Databases, "b") >= 0 {
		t.Fatal("process config still lists the removed database")
	}
}

func TestFleetReloadHotSettingsApplyInPlace(t *testing.T) {
	newFleetReloadEnv(t, "ctl", "b")
	old := fleetMgr.GetInstance("b")
	disabled := false
	if err := reloadDatabase(t, "b", func(db *config.DatabaseConfig) {
		db.TrustLevel, db.TrustLevelExplicit = "advisory", true
		db.ExecutionMode = "approval"
		db.ExecutorEnabled = &disabled
		db.Tags = []string{"payments"}
	}); err != nil {
		t.Fatalf("hot reload: %v", err)
	}
	inst := fleetMgr.GetInstance("b")
	if inst != old || poolClosed(old.Pool) {
		t.Fatal("a hot-only change rebuilt the runtime")
	}
	if inst.Executor.TrustLevel() != "advisory" ||
		inst.Executor.ExecutionMode() != "approval" || inst.Executor.ExecutorEnabled() {
		t.Fatalf("executor policy = %s/%s/%t", inst.Executor.TrustLevel(),
			inst.Executor.ExecutionMode(), inst.Executor.ExecutorEnabled())
	}
	if !inst.Config.HasTag("payments") || !inst.HasTrustLevelOverride() ||
		inst.SnapshotStatus().TrustLevel != "advisory" {
		t.Fatalf("instance metadata not updated: %+v", inst.Config)
	}
}

func reloadDatabase(t *testing.T, name string, mutate func(*config.DatabaseConfig)) error {
	t.Helper()
	return (&fleetReloadEnv{}).reload(func(c *config.Config) {
		i := databaseIndex(c.Databases, name)
		if i < 0 {
			t.Fatalf("database %q not configured", name)
		}
		mutate(&c.Databases[i])
	})
}

func TestFleetReloadRestartClassChangeRebuildsOnlyThatDatabase(t *testing.T) {
	newFleetReloadEnv(t, "ctl", "b")
	oldB, ctl := fleetMgr.GetInstance("b"), fleetMgr.GetInstance("ctl")
	if err := reloadDatabase(t, "b", func(db *config.DatabaseConfig) {
		db.MaxConnections = 4
	}); err != nil {
		t.Fatalf("rebuild reload: %v", err)
	}
	newB := fleetMgr.GetInstance("b")
	if newB == nil || newB == oldB || newB.Config.MaxConnections != 4 {
		t.Fatalf("database b was not rebuilt: %+v", newB)
	}
	if newB.Pool.Config().MaxConns != 4 || !poolClosed(oldB.Pool) {
		t.Fatal("rebuilt runtime did not swap to a new pool")
	}
	if fleetMgr.GetInstance("ctl") != ctl || poolClosed(ctl.Pool) {
		t.Fatal("an unrelated database was rebuilt")
	}
	if _, ok := fleetLLMBudget.Snapshot()["b"]; !ok {
		t.Fatal("rebuild dropped the database's budget share")
	}
}

func TestFleetReloadUnreachableChangeKeepsRunningRuntime(t *testing.T) {
	newFleetReloadEnv(t, "ctl", "b")
	old := fleetMgr.GetInstance("b")
	desired := configController.Desired()
	err := reloadDatabase(t, "b", func(db *config.DatabaseConfig) {
		db.Port = 1
	})
	if !errors.Is(err, errFleetRuntimeCandidate) || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("unreachable change = %v, want errFleetRuntimeCandidate naming b", err)
	}
	if fleetMgr.GetInstance("b") != old || poolClosed(old.Pool) {
		t.Fatal("a rejected reload replaced or closed the running runtime")
	}
	if err := healthCheckStoreDatabase(context.Background(), old); err != nil {
		t.Fatalf("old runtime unhealthy after rejected reload: %v", err)
	}
	if got := configController.Desired(); got.Generation != desired.Generation ||
		got.Config.Databases[databaseIndex(got.Config.Databases, "b")].Port == 1 {
		t.Fatal("a rejected reload advanced the desired config")
	}
}

func TestFleetReloadRejectsInvalidDatabasePolicy(t *testing.T) {
	newFleetReloadEnv(t, "ctl", "b")
	old := fleetMgr.GetInstance("b")
	err := reloadDatabase(t, "b", func(db *config.DatabaseConfig) {
		db.TrustLevel = "autonomus"
	})
	if !errors.Is(err, errInvalidFleetDatabase) {
		t.Fatalf("invalid trust level = %v, want errInvalidFleetDatabase", err)
	}
	if fleetMgr.GetInstance("b") != old || old.Executor.TrustLevel() != "observation" {
		t.Fatal("an invalid reload changed the running policy")
	}
}

func TestFleetReloadRejectsDuplicateNames(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	old := fleetMgr.GetInstance("b")
	err := env.reload(func(c *config.Config) {
		c.Databases = append(c.Databases, c.Databases[databaseIndex(c.Databases, "b")])
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("duplicate database names = %v, want a validation error", err)
	}
	if fleetMgr.GetInstance("b") != old || fleetMgr.InstanceCount() != 2 {
		t.Fatal("a rejected reload changed the fleet")
	}
}

func TestFleetReloadRefusesToRemoveControlDatabase(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	ctl := fleetMgr.GetInstance("ctl")
	err := env.reload(func(c *config.Config) {
		c.Databases = withoutDatabase(c.Databases, "ctl")
	})
	if !errors.Is(err, errFleetControlDatabase) {
		t.Fatalf("removing the control database = %v", err)
	}
	if fleetMgr.GetInstance("ctl") != ctl || poolClosed(ctl.Pool) {
		t.Fatal("the control database was retired")
	}
}

func TestFleetReloadUnreachableAddIsRegisteredFailed(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl")
	down := env.config(t, "ctl")
	down.Name, down.Port = "down", 1
	if err := env.reload(func(c *config.Config) {
		c.Databases = append(c.Databases, down)
	}); err != nil {
		t.Fatalf("adding an unreachable database: %v", err)
	}
	inst := fleetMgr.GetInstance("down")
	if inst == nil || inst.Pool != nil || inst.SnapshotStatus().Error == "" {
		t.Fatalf("unreachable add not visible as failed: %+v", inst)
	}
	// The next reload with a reachable config replaces the placeholder.
	if err := reloadDatabase(t, "down", func(db *config.DatabaseConfig) {
		db.Port = env.config(t, "ctl").Port
	}); err != nil {
		t.Fatalf("repairing the failed database: %v", err)
	}
	repaired := fleetMgr.GetInstance("down")
	if repaired == nil || repaired.Pool == nil || repaired.SnapshotStatus().Error != "" {
		t.Fatalf("failed database not rebuilt once reachable: %+v", repaired)
	}
}

func TestFleetReloadWithoutDatabaseChangeKeepsRuntimes(t *testing.T) {
	env := newFleetReloadEnv(t, "ctl", "b")
	before := fleetMgr.Instances()
	if err := env.reload(func(c *config.Config) {
		c.Trust.Level = "advisory"
		for i := range c.Databases {
			c.Databases[i].TrustLevel = "advisory"
		}
	}); err != nil {
		t.Fatalf("global trust reload: %v", err)
	}
	for name, inst := range fleetMgr.Instances() {
		if before[name] != inst {
			t.Fatalf("db %q rebuilt by a policy-only reload", name)
		}
		if inst.Executor.TrustLevel() != "advisory" {
			t.Fatalf("db %q trust = %s", name, inst.Executor.TrustLevel())
		}
	}
}
