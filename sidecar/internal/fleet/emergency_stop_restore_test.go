package fleet

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// isolatedStopDB creates a throwaway database so a persisted emergency stop
// can never leak into other packages sharing SAGE_TEST_DATABASE_URL.
func isolatedStopDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn, err := testdb.DesignatedDSN()
	if err != nil {
		t.Skipf("%v; restart restore needs PostgreSQL", err)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	name := fmt.Sprintf("sage_estop_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)")
		admin.Close()
	})
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// persistStopRow writes the row a previous sidecar process left behind.
func persistStopRow(t *testing.T, pool *pgxpool.Pool, by string, at time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sage.config (key, value, updated_at, updated_by)
		 VALUES ('emergency_stop', 'true', $1, $2)`, at, by)
	if err != nil {
		t.Fatalf("seed emergency_stop: %v", err)
	}
}

func restartedInstance(name string, pool *pgxpool.Pool) *DatabaseInstance {
	cfg := &config.Config{Trust: config.TrustConfig{Level: "autonomous"}}
	exec := executor.New(pool, cfg, time.Now(), func(string, string, ...any) {})
	return &DatabaseInstance{
		Name:     name,
		Config:   config.DatabaseConfig{Name: name},
		Pool:     pool,
		Executor: exec,
		Status:   &InstanceStatus{Connected: true, Platform: "postgres"},
	}
}

func TestStartupRestoresPersistedEmergencyStop(t *testing.T) {
	pool := isolatedStopDB(t)
	at := time.Date(2026, 9, 26, 22, 15, 0, 0, time.UTC)
	persistStopRow(t, pool, "op@example.com", at)

	for _, path := range []string{"register", "publish", "replace"} {
		t.Run(path, func(t *testing.T) {
			mgr := NewManager(&config.Config{Mode: "fleet"})
			inst := restartedInstance("orders", pool)
			registerThrough(t, mgr, path, inst, pool)

			assertRestoredStop(t, mgr, inst, "op@example.com", at)
			if !executor.CheckEmergencyStop(context.Background(), pool) {
				t.Fatal("executors must keep reading the durable stop")
			}
		})
	}
}

// registerThrough publishes inst via the standalone/fleet (register),
// meta-db create (publish) or meta-db reconnect (replace) path.
func registerThrough(
	t *testing.T, mgr *DatabaseManager, path string,
	inst *DatabaseInstance, pool *pgxpool.Pool,
) {
	t.Helper()
	ctx := context.Background()
	switch path {
	case "register":
		mgr.RegisterInstance(inst)
	case "publish":
		err := mgr.WithLifecycle(ctx, func(op *LifecycleMutation) error {
			return op.PublishRegistration(inst)
		})
		if err != nil {
			t.Fatalf("publish: %v", err)
		}
	case "replace":
		failed := &DatabaseInstance{Name: inst.Name, Status: &InstanceStatus{}}
		mgr.RegisterInstance(failed)
		err := mgr.ReplaceInstanceIfCurrent(ctx, inst.Name, failed, inst,
			func(context.Context, *DatabaseInstance) error { return nil })
		if err != nil {
			t.Fatalf("replace: %v", err)
		}
	}
}

func TestStartupRestoreKeepsRunningDatabaseRunning(t *testing.T) {
	pool := isolatedStopDB(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO sage.config (key, value, updated_by)
		 VALUES ('emergency_stop', 'false', 'admin@example.com')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := restartedInstance("orders", pool)

	mgr.RegisterInstance(inst)

	if mgr.InstanceStopped(inst) || inst.StoppedBy != "" {
		t.Fatalf("resumed database restored as stopped by %q", inst.StoppedBy)
	}
	if mgr.FleetStatus().Summary.EmergencyStopped {
		t.Fatal("summary reports a stop that is not persisted")
	}
}

func TestStartupStopFlagReadErrorFailsClosed(t *testing.T) {
	pool := isolatedStopDB(t)
	closed, err := pgxpool.NewWithConfig(context.Background(), pool.Config())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	closed.Close()
	mgr := NewManager(&config.Config{Mode: "fleet"})
	inst := restartedInstance("orders", closed)

	mgr.RegisterInstance(inst)

	if !mgr.InstanceStopped(inst) || inst.Executor.ExecutorEnabled() {
		t.Fatal("an unreadable emergency_stop flag must restore as stopped")
	}
	if inst.StoppedBy != executor.EmergencyStopActorSystem {
		t.Fatalf("StoppedBy = %q, want system", inst.StoppedBy)
	}
}

func TestEmergencyStopPersistsActorAndAuditRow(t *testing.T) {
	pool := isolatedStopDB(t)
	mgr := NewManager(&config.Config{Mode: "fleet"})
	mgr.RegisterInstance(restartedInstance("orders", pool))

	if _, err := mgr.EmergencyStopStrict("orders", "op@example.com"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := mgr.ResumeStrict("orders", "admin@example.com"); err != nil {
		t.Fatalf("resume: %v", err)
	}

	var by string
	err := pool.QueryRow(context.Background(),
		`SELECT updated_by FROM sage.config WHERE key = 'emergency_stop'`).Scan(&by)
	if err != nil || by != "admin@example.com" {
		t.Fatalf("updated_by = %q (%v), want admin@example.com", by, err)
	}
	assertAuditTrail(t, pool, []string{
		"<nil>->true by op@example.com",
		"true->false by admin@example.com",
	})
}

func assertAuditTrail(t *testing.T, pool *pgxpool.Pool, want []string) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT old_value, new_value, changed_by_actor FROM sage.config_audit
		 WHERE key = 'emergency_stop' ORDER BY id`)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var oldValue *string
		var newValue, actor string
		if err := rows.Scan(&oldValue, &newValue, &actor); err != nil {
			t.Fatalf("scan audit: %v", err)
		}
		old := "<nil>"
		if oldValue != nil {
			old = *oldValue
		}
		got = append(got, fmt.Sprintf("%s->%s by %s", old, newValue, actor))
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audit trail = %v, want %v", got, want)
	}
}

// TestStopDuringReconnectSwap_SavedFlagInPostgres is the meta-db reconnect
// case against real PostgreSQL: the stop lands on the failed runtime (which
// has no executor to persist through) while the reconnected copy is being
// health-checked. The new copy must be stopped and sage.config must say so.
func TestStopDuringReconnectSwap_SavedFlagInPostgres(t *testing.T) {
	pool := isolatedStopDB(t)
	mgr := NewManager(&config.Config{Mode: "fleet"})
	old := &DatabaseInstance{Name: "orders", Status: &InstanceStatus{Error: "down"}}
	mgr.RegisterInstance(old)
	candidate := restartedInstance("orders", pool)
	checking, release := make(chan struct{}), make(chan struct{})
	swapped := make(chan error, 1)
	go func() {
		swapped <- mgr.ReplaceInstanceIfCurrent(context.Background(), "orders",
			old, candidate, func(context.Context, *DatabaseInstance) error {
				close(checking)
				<-release
				return nil
			})
	}()
	<-checking
	_, stopErr := mgr.EmergencyStopStrict("orders", "op@example.com")
	close(release)
	if err := <-swapped; err != nil {
		t.Fatalf("swap: %v", err)
	}

	if stopErr != nil {
		t.Fatalf("stop: %v", stopErr)
	}
	if !mgr.InstanceStopped(candidate) || candidate.Executor.ExecutorEnabled() {
		t.Fatal("reconnected copy is running after a stop during the swap")
	}
	state, err := executor.ReadEmergencyStop(context.Background(), pool)
	if err != nil || !state.Stopped || state.UpdatedBy != "op@example.com" {
		t.Fatalf("saved flag = %+v (%v), want stopped by op@example.com", state, err)
	}
	assertAuditTrail(t, pool, []string{"<nil>->true by op@example.com"})
}
