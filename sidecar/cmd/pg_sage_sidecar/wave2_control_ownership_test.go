package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// No database integration test is required here: router construction and
// manager ownership must be decided entirely from the explicit dependencies.
// Sentinel pools are identity-compared and their methods are never called.
func TestWave2MetaControlOwnershipIgnoresMonitoredFleetPrimary(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"

	metaPool := &pgxpool.Pool{}
	monitoredPool := &pgxpool.Pool{}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "monitored-primary",
		Pool:   monitoredPool,
		Config: config.DatabaseConfig{Name: "monitored-primary"},
		Status: &fleet.InstanceStatus{Connected: true},
		PoolClose: func() {
			t.Fatal("router construction must not close a monitored pool")
		},
	})
	metaState := &metaDBState{
		Pool:  metaPool,
		Store: store.NewDatabaseStore(metaPool, nil),
	}

	result := wireRouter(WireParams{
		Cfg:       cfg,
		Pool:      metaPool,
		FleetMgr:  mgr,
		MetaState: metaState,
	})

	if result.AuthPool != metaPool {
		t.Fatalf("AuthPool = %p, want canonical meta pool %p; "+
			"monitored fleet primary was %p",
			result.AuthPool, metaPool, monitoredPool)
	}
	if result.DBDeps == nil {
		t.Fatal("meta mode must expose managed-database control dependencies")
	}
	if result.DBDeps.Store != metaState.Store {
		t.Fatal("managed-database control store must remain meta-backed")
	}
	if result.DBDeps.Fleet != mgr {
		t.Fatal("meta control store must retain the monitored fleet dependency")
	}
}

func TestWave2ManagedUpdateHealthChecksBeforePersistence(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	old := &fleet.DatabaseInstance{Name: "orders", DatabaseID: 7}
	mgr.RegisterInstance(old)

	events := make([]string, 0, 3)
	candidate := &fleet.DatabaseInstance{Name: "orders-v2", DatabaseID: 7}
	gotCandidate, retired, err := replaceManagedDatabase(
		context.Background(), mgr, "orders", "orders-v2",
		func(context.Context) (*fleet.DatabaseInstance, error) {
			events = append(events, "prepare")
			return candidate, nil
		},
		func(context.Context, *fleet.DatabaseInstance) error {
			events = append(events, "health")
			return nil
		},
		func(context.Context) error {
			events = append(events, "persist")
			return nil
		},
	)
	if err != nil {
		t.Fatalf("replace managed database: %v", err)
	}
	if gotCandidate != candidate || retired != old {
		t.Fatalf("replacement result candidate=%p retired=%p, want %p/%p",
			gotCandidate, retired, candidate, old)
	}
	want := []string{"prepare", "health", "persist"}
	for i := range want {
		if len(events) <= i || events[i] != want[i] {
			t.Fatalf("lifecycle events = %v, want %v", events, want)
		}
	}
	if mgr.GetInstance("orders") != nil ||
		mgr.GetInstance("orders-v2") != candidate {
		t.Fatal("candidate was not atomically published after persistence")
	}
}

func TestWave2ManagedUpdatePersistenceFailurePreservesOld(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	old := &fleet.DatabaseInstance{Name: "orders", DatabaseID: 7}
	mgr.RegisterInstance(old)

	wantErr := errors.New("catalog write failed")
	closed := 0
	candidate := &fleet.DatabaseInstance{
		Name:      "orders-v2",
		PoolClose: func() { closed++ },
	}
	_, _, err := replaceManagedDatabase(
		context.Background(), mgr, "orders", "orders-v2",
		func(context.Context) (*fleet.DatabaseInstance, error) {
			return candidate, nil
		},
		func(context.Context, *fleet.DatabaseInstance) error { return nil },
		func(context.Context) error { return wantErr },
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("replacement error = %v, want %v", err, wantErr)
	}
	if mgr.GetInstance("orders") != old || mgr.GetInstance("orders-v2") != nil {
		t.Fatal("failed persistence changed the active runtime")
	}
	if closed != 1 {
		t.Fatalf("rejected candidate close calls = %d, want 1", closed)
	}
}

func TestWave2RenameAndDeleteSerializeByDatabaseIdentity(t *testing.T) {
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	old := &fleet.DatabaseInstance{Name: "orders", DatabaseID: 7}
	mgr.RegisterInstance(old)
	renamed := &fleet.DatabaseInstance{Name: "orders-v2", DatabaseID: 7}

	published := make(chan struct{})
	releaseRename := make(chan struct{})
	renameDone := make(chan error, 1)
	go func() {
		renameDone <- mgr.WithLifecycle(
			context.Background(),
			func(op *fleet.LifecycleMutation) error {
				current, err := op.ValidateReplacement(
					"orders", "orders-v2", old,
				)
				if err != nil {
					return err
				}
				if err := op.PublishReplacement(
					"orders", current, renamed,
				); err != nil {
					return err
				}
				close(published)
				<-releaseRename
				return nil
			},
		)
	}()
	<-published

	persisted := make(chan struct{})
	deleteStarted := make(chan struct{})
	deleteDone := make(chan struct{})
	var retired *fleet.DatabaseInstance
	var deleteErr error
	go func() {
		defer close(deleteDone)
		close(deleteStarted)
		retired, deleteErr = deleteManagedDatabase(
			context.Background(), mgr, 7,
			func(context.Context) error {
				close(persisted)
				return nil
			},
		)
	}()
	<-deleteStarted
	select {
	case <-persisted:
		t.Fatal("delete persisted while rename still owned lifecycle reservation")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseRename)
	if err := <-renameDone; err != nil {
		t.Fatalf("rename: %v", err)
	}
	<-deleteDone
	if deleteErr != nil {
		t.Fatalf("delete: %v", deleteErr)
	}
	if retired != renamed {
		t.Fatalf("delete retired generation = %p, want renamed %p", retired, renamed)
	}
	if mgr.GetInstance("orders") != nil || mgr.GetInstance("orders-v2") != nil {
		t.Fatal("delete left an orphan runtime after concurrent rename")
	}
}

func TestWave2SchemaBootstrapFailureRejectsRuntime(t *testing.T) {
	wantErr := errors.New("permission denied creating sage schema")
	original := bootstrapManagedDatabaseSchema
	bootstrapManagedDatabaseSchema = func(
		context.Context, *pgxpool.Pool,
	) error {
		return wantErr
	}
	t.Cleanup(func() { bootstrapManagedDatabaseSchema = original })

	inst, err := buildStoreDatabaseRuntime(
		context.Background(),
		store.DatabaseRecord{Name: "orders"},
		&pgxpool.Pool{},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("bootstrap error = %v, want %v", err, wantErr)
	}
	if inst != nil {
		t.Fatalf("bootstrap failure returned runtime candidate: %#v", inst)
	}
}

func TestWave2MonitoredRemovalAndReplacementDoNotRebindMetaAuth(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"

	metaPool := &pgxpool.Pool{}
	oldPool := &pgxpool.Pool{}
	newPool := &pgxpool.Pool{}
	oldClosed := 0
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "orders",
		Pool:   oldPool,
		Config: config.DatabaseConfig{Name: "orders"},
		Status: &fleet.InstanceStatus{Connected: true},
		PoolClose: func() {
			oldClosed++
		},
	})
	metaState := &metaDBState{
		Pool:  metaPool,
		Store: store.NewDatabaseStore(metaPool, nil),
	}
	result := wireRouter(WireParams{
		Cfg:       cfg,
		Pool:      metaPool,
		FleetMgr:  mgr,
		MetaState: metaState,
	})
	if result.AuthPool != metaPool {
		t.Fatalf("initial AuthPool = %p, want meta pool %p",
			result.AuthPool, metaPool)
	}

	if err := mgr.RemoveInstanceContext(context.Background(), "orders"); err != nil {
		t.Fatalf("remove monitored database: %v", err)
	}
	if oldClosed != 1 {
		t.Fatalf("old monitored pool close calls = %d, want 1", oldClosed)
	}
	if result.AuthPool != metaPool {
		t.Fatalf("captured AuthPool changed after monitored removal: got %p, want %p",
			result.AuthPool, metaPool)
	}
	if result.DBDeps.Store != metaState.Store {
		t.Fatal("control store changed after monitored removal")
	}

	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "orders",
		Pool:   newPool,
		Config: config.DatabaseConfig{Name: "orders"},
		Status: &fleet.InstanceStatus{Connected: true},
		PoolClose: func() {
			t.Fatal("replacement pool must remain active")
		},
	})
	rewired := wireRouter(WireParams{
		Cfg:       cfg,
		Pool:      metaPool,
		FleetMgr:  mgr,
		MetaState: metaState,
	})
	if rewired.AuthPool != metaPool {
		t.Fatalf("rewired AuthPool = %p after monitored replacement, want %p",
			rewired.AuthPool, metaPool)
	}
	if rewired.AuthPool == oldPool || rewired.AuthPool == newPool {
		t.Fatal("meta auth must never capture an old or replacement monitored pool")
	}
	if rewired.DBDeps.Store != metaState.Store {
		t.Fatal("rewired managed-database store must remain canonical meta store")
	}
}

func TestWave2MetaAuthOwnershipSurvivesNoConnectedMonitoredDatabases(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	metaPool := &pgxpool.Pool{}
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{
		Name:   "offline",
		Config: config.DatabaseConfig{Name: "offline"},
		Status: &fleet.InstanceStatus{Connected: false, Error: "connection refused"},
	})
	metaState := &metaDBState{
		Pool:  metaPool,
		Store: store.NewDatabaseStore(metaPool, nil),
	}

	result := wireRouter(WireParams{
		Cfg:       cfg,
		Pool:      metaPool,
		FleetMgr:  mgr,
		MetaState: metaState,
	})
	if result.AuthPool != metaPool {
		t.Fatalf("AuthPool = %p with monitored fleet offline, want meta pool %p",
			result.AuthPool, metaPool)
	}
	if result.DBDeps == nil || result.DBDeps.Store != metaState.Store {
		t.Fatal("control store must remain available when all monitored databases are offline")
	}
}
