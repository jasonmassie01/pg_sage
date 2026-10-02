package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sre"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M6 exit path for a family without an RCA signal: a commit
// storm on real PostgreSQL -> the reactive detector (three hot polls) ->
// an LWLock investigation -> its probe plan -> a concluded WAL-write
// contention diagnosis, through the constructor every runtime mode uses.
func TestComposedSRE_M6_DetectorOpensAnLWLockInvestigation(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool := openComposedPool(t, ctx, dsn)
	stopStorm := commitStorm(t, ctx, pool, dsn, 24)
	defer stopStorm()
	settings := composedConfig("standalone").SRE
	settings.AutomaticStart = true
	settings.TriggerIntervalSeconds, settings.SampleIntervalSeconds = 1, 1
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
		runner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1)),
		name:   "m6_db", runtimeKey: fmt.Sprintf("composed:m6:%d", time.Now().UnixNano()),
		settings: settings, logFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { svc.Coordinator().Run(runCtx); close(done) }()
	t.Cleanup(func() { stop(); <-done })
	inv := waitKind(t, ctx, svc, sre.TriggerLWLock)
	if inv.State != sre.StateConcluded || inv.Summary.Root != "wal_write_contention" ||
		inv.CaseID != "sre:detector:lwlock_contention:m6_db" {
		t.Fatalf("investigation = %+v, want concluded WAL write contention", inv)
	}
}

// The investigator's trigger source is RCA incidents plus the reactive
// detector; the detector needs a probe runner.
func TestSRETriggerSource_CombinesIncidentsAndDetector(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool := openComposedPool(t, ctx, dsn)
	var incident string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents (severity, root_cause,
		causal_chain, signal_ids, source, confidence, database_name)
		VALUES ('warning', 'fixture', '[]'::jsonb, ARRAY['log_checkpoint_too_frequent'],
		        'log_deterministic', 1.0, 'm6_src') RETURNING id::text`).
		Scan(&incident); err != nil {
		t.Fatalf("insert incident: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.incidents WHERE id = $1::uuid",
			incident)
	})
	src, err := sreTriggerSource(sreInvestigatorDeps{monitored: pool, name: "m6_src",
		runner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1)),
		logFn:  func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("trigger source: %v", err)
	}
	ts, err := src.Triggers(ctx)
	if err != nil {
		t.Fatalf("triggers: %v", err)
	}
	found := false
	for _, tr := range ts {
		found = found || (tr.IncidentID == incident && tr.Kind == sre.TriggerCheckpoint)
	}
	if !found {
		t.Fatalf("triggers = %+v, want the checkpoint incident %s", ts, incident)
	}
	if _, err := sreTriggerSource(sreInvestigatorDeps{monitored: pool, name: "x"}); err == nil {
		t.Fatal("a trigger source without a probe runner was built")
	}
}

// commitStorm runs n sessions committing one-row inserts as fast as they
// can: they queue on the WAL write lock.
func commitStorm(t *testing.T, ctx context.Context, pool *pgxpool.Pool, dsn string,
	n int) func() {
	t.Helper()
	table := pgx.Identifier{fmt.Sprintf("m6_storm_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (id bigserial, v int)"); err != nil {
		t.Fatalf("storm table: %v", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			cancel()
			t.Fatalf("storm session: %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close(context.Background())
			for sctx.Err() == nil {
				_, _ = conn.Exec(sctx, "INSERT INTO "+table+" (v) VALUES (1)")
			}
		}()
	}
	return func() {
		cancel()
		wg.Wait()
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	}
}

func waitKind(t *testing.T, ctx context.Context, svc *sre.Service,
	kind sre.TriggerKind) sre.Investigation {
	t.Helper()
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		page, err := svc.List(ctx, sre.ListFilter{})
		if err == nil {
			for _, inv := range page.Items {
				if inv.TriggerKind == kind && inv.State.Terminal() {
					return inv
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no finished %s investigation", kind)
	return sre.Investigation{}
}
