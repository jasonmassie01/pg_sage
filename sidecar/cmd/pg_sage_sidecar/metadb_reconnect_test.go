package main

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// fleet-31: the meta-DB reconnect path (metadb_reconnect.go) keeps today's
// behaviour after the split. The DB-backed cases live in
// metadb_reconnect_db_test.go.
//
// Invalid input: not applicable. storeRecordToDBConfig has no error path,
// and the store validates records before they reach it.

func TestResolveExecModeEmptyIsAuto(t *testing.T) {
	if got := resolveExecMode(store.DatabaseRecord{}); got != "auto" {
		t.Fatalf("empty execution mode = %q, want auto", got)
	}
	for _, mode := range []string{"manual", "approval", "auto"} {
		got := resolveExecMode(store.DatabaseRecord{ExecutionMode: mode})
		if got != mode {
			t.Fatalf("execution mode %q resolved to %q", mode, got)
		}
	}
}

func TestStoreRecordToDBConfigZeroRecord(t *testing.T) {
	got := storeRecordToDBConfig(store.DatabaseRecord{Name: "bare", Tags: nil})
	want := config.DatabaseConfig{Name: "bare", TrustLevelExplicit: true,
		ExecutionMode: "auto"}
	if got.Tags != nil {
		t.Fatalf("nil tags map to %#v, want nil", got.Tags)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("zero record = %+v\nwant %+v", got, want)
	}
}

func TestStoreRecordToDBConfigGolden(t *testing.T) {
	rec := store.DatabaseRecord{ID: 3, Name: "billing", Host: "db.internal",
		Port: 6432, DatabaseName: "billing", Username: "sage", SSLMode: "verify-full",
		MaxConnections: 9, Tags: map[string]string{"env": "prod", "tenant": "t1"},
		TrustLevel: "autonomous", ExecutionMode: "approval"}
	got := storeRecordToDBConfig(rec)
	want := config.DatabaseConfig{Name: "billing", Host: "db.internal", Port: 6432,
		Database: "billing", User: "sage", SSLMode: "verify-full", MaxConnections: 9,
		Tags: []string{"env=prod", "tenant=t1"}, TrustLevel: "autonomous",
		TrustLevelExplicit: true, ExecutionMode: "approval"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("storeRecordToDBConfig = %+v\nwant %+v", got, want)
	}
}

// Zero failed instances: a healthy and a manually stopped instance are
// skipped without touching the store (a nil Store would panic) and without
// a log line.
func TestRetryFailedInstancesSkipsHealthyAndStopped(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	fleetMgr = fleet.NewManager(cfg)
	state := &metaDBState{}
	if out := captureStderr(t, func() { retryFailedInstances(state) }); out != "" {
		t.Fatalf("an empty fleet logged %q", out)
	}
	healthy, _ := connectedMetaInstance(t, store.DatabaseRecord{ID: 1, Name: "up"}, false)
	stopped := failedStoreInstance(store.DatabaseRecord{ID: 2, Name: "held"}, "down")
	stopped.Stopped = true
	fleetMgr.RegisterInstance(healthy)
	fleetMgr.RegisterInstance(stopped)
	out := captureStderr(t, func() { retryFailedInstances(state) })
	if strings.Contains(out, "attempting reconnect") {
		t.Fatalf("healthy or stopped instance retried:\n%s", out)
	}
	if fleetMgr.GetInstance("up") != healthy || fleetMgr.GetInstance("held") != stopped {
		t.Fatal("a skipped instance was replaced")
	}
	if stopped.SnapshotStatus().Error != "down" {
		t.Fatalf("stopped error = %q, want down", stopped.SnapshotStatus().Error)
	}
}

// The loop ticks on fleetReconnectInterval and returns on cancel without
// leaving a goroutine behind. CI runs this under -race.
func TestFleetReconnectLoopExitsOnCancel(t *testing.T) {
	preserveFleetRuntimeGlobals(t)
	cfg = config.DefaultConfig()
	fleetMgr = fleet.NewManager(cfg)
	oldInterval := fleetReconnectInterval
	fleetReconnectInterval = 5 * time.Millisecond
	t.Cleanup(func() { fleetReconnectInterval = oldInterval })
	baseline := runtime.NumGoroutine()
	out := captureStderr(t, func() {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			fleetReconnectLoop(ctx, &metaDBState{})
			close(done)
		}()
		time.Sleep(60 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("fleetReconnectLoop did not return after cancel")
		}
	})
	if !strings.Contains(out, "reconcile: meta reconcile: no fleet or store") {
		t.Fatalf("the loop never ticked:\n%s", out)
	}
	if n := settledGoroutines(baseline, 3*time.Second); n > baseline {
		t.Fatalf("goroutines %d after cancel, want at most %d", n, baseline)
	}
}
