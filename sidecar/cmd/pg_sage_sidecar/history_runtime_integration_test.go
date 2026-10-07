package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/store"
)

// metaHistoryRuntime builds a meta-db runtime with history.store: meta,
// the monitored database and the meta database being two databases.
func metaHistoryRuntime(t *testing.T, monDSN, metaDSN string) (store.DatabaseRecord,
	*config.Config) {
	t.Helper()
	base := parityBaseConfig(t, monDSN)
	preserveParityGlobals(t, base)
	cfg.Mode, cfg.MetaDB, cfg.History.Store = "fleet", metaDSN, "meta"
	control, err := connectMetaDB(metaDSN)
	if err != nil {
		t.Fatalf("connect meta pool: %v", err)
	}
	t.Cleanup(control.Close)
	globalMetaState = &metaDBState{Pool: control}
	if err := initHistoryStoreSchema(control); err != nil {
		t.Fatalf("history store schema: %v", err)
	}
	dbCfg := parityDatabaseConfig(base, "app")
	return store.DatabaseRecord{ID: 77, Name: "app", Host: dbCfg.Host, Port: dbCfg.Port,
		DatabaseName: dbCfg.Database, Username: dbCfg.User, SSLMode: "disable",
		MaxConnections: dbCfg.MaxConnections, TrustLevel: base.Trust.Level,
		ExecutionMode: "approval"}, base
}

func TestMetaRuntimeKeepsHistoryInTheStore(t *testing.T) {
	monDSN, metaDSN, mon, meta := historyDBs(t)
	rec, _ := metaHistoryRuntime(t, monDSN, metaDSN)
	inst, err := prepareStoreDatabaseConnection(context.Background(), rec, monDSN)
	if err != nil {
		t.Fatalf("build meta runtime: %v", err)
	}
	activateStoreDatabaseWithManager(fleetMgr, inst)
	deadline := time.Now().Add(60 * time.Second)
	for count(t, meta, `SELECT count(*) FROM sage.snapshots WHERE database_id = 77`) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first collection cycle wrote no snapshot to the store")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := count(t, mon, "SELECT count(*) FROM sage.snapshots"); n != 0 {
		t.Fatalf("meta mode wrote %d snapshots to the monitored database", n)
	}
	var seen int
	if err := histstore.Resolve(inst.Pool).QueryRow(context.Background(),
		`SELECT count(*) FROM sage.snapshots s WHERE {db:s}`).Scan(&seen); err != nil ||
		seen == 0 {
		t.Fatalf("readers holding the monitored pool must reach the store: %d %v", seen, err)
	}
}

func TestMetaRuntimeRefusesUnmigratedHistory(t *testing.T) {
	monDSN, metaDSN, mon, _ := historyDBs(t)
	seedHistoryRows(t, mon, 2)
	rec, _ := metaHistoryRuntime(t, monDSN, metaDSN)
	_, err := prepareStoreDatabaseConnection(context.Background(), rec, monDSN)
	if err == nil || !strings.Contains(err.Error(), "history migrate") {
		t.Fatalf("a database with unmigrated history must be refused: %v", err)
	}
}
