package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Global runtime owners intentionally run sequentially; each database is an isolated test fixture.
func metaLifecycleFixture(t *testing.T) (*metaDBState, store.DatabaseInput, int) {
	t.Helper()
	if os.Getenv(testdb.EnvName) == "" {
		t.Skip("requires explicit disposable SAGE_TEST_DATABASE_URL")
	}
	p, err := connectMetaDB(os.Getenv(testdb.EnvName))
	if err != nil {
		t.Fatalf("connect fixture: %v", err)
	}
	t.Cleanup(p.Close)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	count, err := auth.UserCount(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		if err := auth.BootstrapAdmin(ctx, p, adminEmail, "fixture-admin-password"); err != nil {
			t.Fatal(err)
		}
	}
	state, err := initMetaDB(p, "fixture-encryption-passphrase")
	if err != nil || state == nil || len(state.EncryptKey) != 32 {
		t.Fatalf("meta bootstrap err=%v state present=%v", err, state != nil)
	}
	var userID int
	if err := p.QueryRow(ctx, "SELECT id FROM sage.users WHERE email=$1", adminEmail).
		Scan(&userID); err != nil {
		t.Fatal(err)
	}
	conn := p.Config().ConnConfig
	input := store.DatabaseInput{Name: "managed-a", Host: conn.Host, Port: int(conn.Port),
		DatabaseName: conn.Database, Username: conn.User, Password: conn.Password,
		SSLMode: "disable", MaxConnections: 6, TrustLevel: "observation", ExecutionMode: "approval"}
	return state, input, userID
}

func prepareMetaGlobals(t *testing.T) {
	t.Helper()
	preserveFleetRuntimeGlobals(t)
	oldCtx := shutdownCtx
	ctx, cancel := context.WithCancel(context.Background())
	shutdownCtx = ctx
	cfg = config.DefaultConfig()
	cfg.Mode, cfg.Trust.Level = "fleet", "observation"
	cfg.Collector.IntervalSeconds, cfg.Analyzer.IntervalSeconds = 3600, 3600
	cfg.LLM.Enabled, cfg.RCA.Enabled = false, true
	llmClient, configController, fleetLLMBudget = nil, nil, nil
	analyzeSem = make(chan struct{}, 1)
	fleetMgr = fleet.NewManager(cfg)
	t.Cleanup(func() {
		cancel()
		for _, inst := range fleetMgr.Instances() {
			if err := fleet.ShutdownInstance(context.Background(), inst); err != nil {
				t.Errorf("drain runtime: %v", err)
			}
		}
		shutdownCtx = oldCtx
	})
}

func TestMetaLifecycleCreateReplaceDelete(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx := context.Background()
	rec, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID)
	if err != nil || rec == nil {
		t.Fatalf("create: %v", err)
	}
	old := fleetMgr.GetInstance(input.Name)
	assertManagedRuntime(t, old, rec.ID, input.Name)
	if old.Config.ExecutionMode != "approval" || old.Executor.ExecutionMode() != "approval" {
		t.Fatal("managed runtime lost stored approval authority")
	}
	input.Name, input.ExecutionMode, input.TrustLevel = "managed-b", "auto", "advisory"
	updated, err := applyMetaDatabaseUpdate(ctx, fleetMgr, state, rec.ID, *rec, input)
	if err != nil || updated == nil || updated.Name != input.Name {
		t.Fatalf("replace: record=%+v err=%v", updated, err)
	}
	replacement := fleetMgr.GetInstance(input.Name)
	assertManagedRuntime(t, replacement, rec.ID, input.Name)
	if fleetMgr.GetInstance(rec.Name) != nil || replacement == old ||
		old.Pool.Ping(ctx) == nil || replacement.Config.TrustLevel != "advisory" {
		t.Fatal("replacement retained old authority, pool, or registration")
	}
	if err := applyMetaDatabaseDelete(ctx, fleetMgr, state, rec.ID, *updated); err != nil {
		t.Fatal(err)
	}
	if fleetMgr.InstanceCount() != 0 || replacement.Pool.Ping(ctx) == nil {
		t.Fatal("delete did not remove registration and drain pool")
	}
	if _, err := state.Store.Get(ctx, rec.ID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("deleted store row remained: %v", err)
	}
}

func assertManagedRuntime(t *testing.T, inst *fleet.DatabaseInstance, id int, name string) {
	t.Helper()
	if inst == nil || inst.DatabaseID != id || inst.Name != name || inst.Collector == nil ||
		inst.Analyzer == nil || inst.Executor == nil || inst.Cancel == nil || inst.Workers == nil {
		t.Fatalf("incomplete managed runtime: %+v", inst)
	}
	if err := healthCheckStoreDatabase(nil, inst); err != nil || !inst.SnapshotStatus().Connected {
		t.Fatalf("runtime unhealthy: %v", err)
	}
}

func TestMetaLifecycleFailedCreateRollsBackStore(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	input.Host, input.Port, input.Name = "127.0.0.1", 1, "unreachable"
	rec, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID)
	if err == nil || rec != nil || fleetMgr.InstanceCount() != 0 {
		t.Fatalf("failed create published runtime: rec=%+v err=%v", rec, err)
	}
	var count int
	err = state.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.databases WHERE name=$1", input.Name).Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("failed create orphaned store row: count=%d err=%v", count, err)
	}
}

func TestMetaLifecycleFailedReplacementPreservesOld(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	prepareMetaGlobals(t)
	ctx := context.Background()
	rec, err := applyMetaDatabaseCreate(ctx, fleetMgr, state, input, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Store.Delete(ctx, rec.ID) })
	old := fleetMgr.GetInstance(rec.Name)
	input.Name, input.Host, input.Port = "broken-replacement", "127.0.0.1", 1
	deadline, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	updated, err := applyMetaDatabaseUpdate(deadline, fleetMgr, state, rec.ID, *rec, input)
	if err == nil || updated != nil || fleetMgr.GetInstance(rec.Name) != old ||
		fleetMgr.GetInstance(input.Name) != nil || old.Pool.Ping(ctx) != nil {
		t.Fatalf("failed update changed live owner: record=%+v err=%v", updated, err)
	}
	persisted, err := state.Store.Get(ctx, rec.ID)
	if err != nil || persisted.Name != rec.Name || persisted.Host != rec.Host {
		t.Fatalf("failed update changed persisted settings: %+v err=%v", persisted, err)
	}
}

func TestMetaBootstrapKeepsEncryptionKeyAcrossRestart(t *testing.T) {
	state, input, userID := metaLifecycleFixture(t)
	id, err := state.Store.Create(context.Background(), input, userID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Store.Delete(context.Background(), id) })
	restarted, err := initMetaDB(state.Pool, "fixture-encryption-passphrase")
	if err != nil || !bytes.Equal(state.EncryptKey, restarted.EncryptKey) {
		t.Fatalf("restart changed credential key: %v", err)
	}
	dsn, err := restarted.Store.GetConnectionString(context.Background(), id)
	parsed, parseErr := pgxpool.ParseConfig(dsn)
	if err != nil || parseErr != nil || parsed.ConnConfig.Password != input.Password {
		t.Fatalf("restart cannot decrypt stored credential: get=%v parse=%v", err, parseErr)
	}
	rows, err := loadDatabasesFromStore(context.Background(), restarted.Store)
	if err != nil || len(rows) != 1 || rows[0].ID != id {
		t.Fatalf("restart lost enabled database: count=%d err=%v", len(rows), err)
	}
}

func TestMetaHealthCheckRejectsWrongDatabaseAndCanceledContext(t *testing.T) {
	state, input, _ := metaLifecycleFixture(t)
	inst := &fleet.DatabaseInstance{Pool: state.Pool,
		Config: config.DatabaseConfig{Database: "wrong_database"}}
	err := healthCheckStoreDatabase(context.Background(), inst)
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("wrong database accepted: %v", err)
	}
	inst.Config.Database = input.DatabaseName
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := healthCheckStoreDatabase(ctx, inst); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled health check: %v", err)
	}
	if err := healthCheckStoreDatabase(nil, nil); !errors.Is(err, fleet.ErrInvalidInstance) {
		t.Fatalf("nil instance: %v", err)
	}
}
