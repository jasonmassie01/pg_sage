package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/retention"
	"github.com/pg-sage/sidecar/internal/schema"
)

// history.store (v2.3.0): where each database's telemetry history lives.
// monitored keeps it in the monitored database; meta keeps it in the meta
// database, as the database's meta-db record id. Every runtime resolves
// its store before it starts, refuses to start when its history would be
// split between the two places, and registers the store for its pool so
// every history reader holding that pool reaches it (histstore.Resolve).

// historyMetaPool is the meta database (nil without one). Tests replace it.
var historyMetaPool = func() *pgxpool.Pool {
	if globalMetaState == nil {
		return nil
	}
	return globalMetaState.Pool
}

// runtimeHistoryID is the database id a runtime's history rows carry: the
// meta-db record id. The perf gate replaces it to run the standalone
// runtime with its history in a store.
var runtimeHistoryID = func(spec databaseRuntimeSpec) int { return spec.DatabaseID }

// initHistoryStoreSchema makes the meta database a history store when
// history.store is meta (after the meta bootstrap, before any runtime).
func initHistoryStoreSchema(metaPool *pgxpool.Pool) error {
	if !cfg.HistoryInMeta() {
		return nil
	}
	if err := schema.BootstrapHistoryStore(context.Background(), metaPool); err != nil {
		return fmt.Errorf("history store bootstrap on the meta database: %w", err)
	}
	logInfo("startup", "history.store: meta: snapshots and the query store are kept in "+
		"the meta database")
	return nil
}

// resolveRuntimeHistory decides where spec's database keeps its history
// and refuses a placement that would split it.
func resolveRuntimeHistory(ctx context.Context, spec databaseRuntimeSpec) (histstore.Store,
	error) {
	meta, id := historyMetaPool(), runtimeHistoryID(spec)
	if cfg.HistoryInMeta() {
		return metaRuntimeHistory(ctx, spec, meta, id)
	}
	st, err := histstore.OpenMonitored(ctx, spec.Pool)
	if err != nil {
		return histstore.Store{}, fmt.Errorf("db %q: %w", spec.Name, err)
	}
	p := histstore.Placement{Name: spec.Name, Monitored: spec.Pool, Store: st,
		DatabaseID: id}
	if meta != nil {
		p.Meta = meta
	}
	if err := histstore.CheckLocation(ctx, p); err != nil {
		return histstore.Store{}, err
	}
	return st, nil
}

func metaRuntimeHistory(ctx context.Context, spec databaseRuntimeSpec, meta *pgxpool.Pool,
	id int) (histstore.Store, error) {
	if meta == nil {
		return histstore.Store{}, fmt.Errorf("db %q: history.store is meta but no meta "+
			"database is connected (set meta_db)", spec.Name)
	}
	if id <= 0 {
		return histstore.Store{}, fmt.Errorf("db %q: history.store: meta keeps history "+
			"under the database's meta-db record, and this database has none (YAML fleet "+
			"and agent databases); use history.store: monitored", spec.Name)
	}
	st, err := histstore.NewMeta(meta, id)
	if err != nil {
		return histstore.Store{}, err
	}
	err = histstore.CheckLocation(ctx, histstore.Placement{Name: spec.Name,
		Monitored: spec.Pool, Store: st, Meta: meta, DatabaseID: id})
	if err != nil {
		return histstore.Store{}, err
	}
	if err := histstore.RegisterDatabase(ctx, st, spec.Name); err != nil {
		return histstore.Store{}, err
	}
	return st, nil
}

// installHistory resolves the runtime's history store and registers it for
// the runtime's pool until the runtime stops.
func (rt *databaseRuntime) installHistory(ctx context.Context) error {
	st, err := resolveRuntimeHistory(ctx, rt.spec)
	if err != nil {
		return err
	}
	unregister := histstore.Register(rt.spec.Pool, rt.spec.Name, st)
	rt.start(func() {
		<-rt.ctx.Done()
		unregister()
	})
	if st.Scoped() {
		rt.note("history:meta")
	}
	return nil
}

// startHistoryStoreCleaner runs the history store's retention once per
// process when history.store is meta.
func startHistoryStoreCleaner(metaPool *pgxpool.Pool) {
	if !cfg.HistoryInMeta() || metaPool == nil {
		return
	}
	startHistoryStoreCleanerFor(metaPool)
}

// startHistoryStoreCleanerFor runs the store cleaner until the process
// stops or the returned func is called (it waits for the cleaner).
func startHistoryStoreCleanerFor(metaPool *pgxpool.Pool) func() {
	parent := shutdownCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	interval := cfg.Analyzer.Interval() + 5*time.Second
	c := retention.New(metaPool, cfg, logStructuredWrapper).ForHistoryStore()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.RunEvery(ctx, interval)
	}()
	return func() {
		cancel()
		<-done
	}
}
