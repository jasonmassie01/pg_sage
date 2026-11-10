package gameday

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// benches-03: local bench runs were kept only in memory (LocalBench.last),
// so a process that stopped mid-run left no record of the clone it made.
// With a store they are written to sage.sre_game_days as kind 'local'.

// uuidBenchLedger stores a run under a UUID, as earned.Service does.
type uuidBenchLedger struct {
	t  *testing.T
	id string
}

func (l *uuidBenchLedger) IngestBench(_ context.Context, _ []byte,
	in earned.BenchIngest) (earned.EvalRun, error) {
	l.id = newUUID(l.t)
	return earned.EvalRun{ID: l.id, Origin: in.Origin}, nil
}

// closedStore is a store whose pool is closed: every write fails.
func closedStore(t *testing.T, f *gdFixture) *Store {
	t.Helper()
	pool, err := pgxpool.New(f.ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	st, err := NewStore(pool, f.store.deployment)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func storedLocalBench(t *testing.T, f *gdFixture, st *Store, l BenchLedger) *LocalBench {
	t.Helper()
	b, err := NewLocalBench(LocalBenchConfig{Database: f.database, Provider: "dle",
		Store: st, Now: func() time.Time { return f.now }}, f.provider, f.faults, l)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type localRow struct {
	kind, status, cloneID, evalRunID string
}

func localRows(t *testing.T, f *gdFixture) map[string]localRow {
	t.Helper()
	rows, err := f.store.pool.Query(f.ctx, `SELECT id::text, kind, status,
		COALESCE(clone_id, ''), COALESCE(eval_run_id::text, '') FROM sage.sre_game_days
		WHERE deployment_id = $1`, f.store.deployment)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]localRow{}
	for rows.Next() {
		var id string
		var r localRow
		if err := rows.Scan(&id, &r.kind, &r.status, &r.cloneID, &r.evalRunID); err != nil {
			t.Fatal(err)
		}
		out[id] = r
	}
	return out
}

// Happy path: a finished local run is a 'local' row with its clone and
// its stored report.
func TestLocalBenchWritesItsRun(t *testing.T) {
	f := newGDFixture(t)
	l := &uuidBenchLedger{t: t}
	run, err := storedLocalBench(t, f, f.store, l).Run(f.ctx, []string{"lock_blocking"})
	if err != nil {
		t.Fatal(err)
	}
	rows := localRows(t, f)
	got, ok := rows[run.ID]
	if len(rows) != 1 || !ok || got.kind != "local" || got.status != StatusCompleted ||
		got.cloneID != "clone-1" || got.evalRunID != l.id {
		t.Fatalf("stored local runs = %+v, want %s completed on clone-1", rows, run.ID)
	}
}

// The running row carries the clone id while the fault programs run.
func TestLocalBenchRecordsItsCloneWhileRunning(t *testing.T) {
	f := newGDFixture(t)
	f.faults.block = make(chan struct{})
	b := storedLocalBench(t, f, f.store, &uuidBenchLedger{t: t})
	started, err := b.Start(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got localRow
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got = localRows(t, f)[started.ID]; got.cloneID != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(f.faults.block)
	for b.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got.kind != "local" || got.status != StatusRunning || got.cloneID != "clone-1" {
		t.Fatalf("while running = %+v, want running local on clone-1", got)
	}
	if last, _ := b.Last(); last.Status != StatusCompleted {
		t.Fatalf("after the run = %+v", last)
	}
}

// Fail closed: a run that cannot be recorded is refused before a clone
// exists, so it can never be leaked unrecorded.
func TestLocalBenchRefusesARunItCannotRecord(t *testing.T) {
	f := newGDFixture(t)
	b := storedLocalBench(t, f, closedStore(t, f), &uuidBenchLedger{t: t})
	_, err := b.Run(f.ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "record") {
		t.Fatalf("run with a closed store: err = %v", err)
	}
	if len(f.provider.created) != 0 || b.Running() {
		t.Fatalf("created %v, running %v: the unrecorded run went ahead",
			f.provider.created, b.Running())
	}
	if _, ok := b.Last(); ok {
		t.Fatal("a refused run was kept as the last run")
	}
	// The slot is free again: the next run with a working store goes ahead.
	if _, err := storedLocalBench(t, f, f.store, &uuidBenchLedger{t: t}).Run(f.ctx,
		nil); err != nil {
		t.Fatalf("run after a refused one: %v", err)
	}
}

// Before a run the local bench sweeps its own stale runs: a crash
// within MaxRunTime is cleaned up by the next run after MaxRunTime.
func TestLocalBenchRunSweepsItsStaleRuns(t *testing.T) {
	f := newGDFixture(t)
	stale := insertRun(t, f, f.database, "local", StatusRunning,
		f.now.Add(-7*time.Hour), "clone-crashed")
	if _, err := storedLocalBench(t, f, f.store, &uuidBenchLedger{t: t}).Run(f.ctx,
		nil); err != nil {
		t.Fatal(err)
	}
	if got := readRun(t, f, stale); got.status != StatusAbandoned {
		t.Fatalf("stale local run = %+v, want abandoned", got)
	}
	ids := destroyedIDs(f.provider)
	if strings.Join(ids, ",") != "clone-1,clone-crashed" {
		t.Fatalf("destroyed %v, want the crashed clone and this run's", ids)
	}
}

// Nil: without a store a local bench keeps runs in memory as before and
// has nothing to sweep.
func TestLocalBenchWithoutAStoreSweepsNothing(t *testing.T) {
	f := newGDFixture(t)
	insertRun(t, f, f.database, "local", StatusRunning, f.now.Add(-7*time.Hour), "c-x")
	b := storedLocalBench(t, f, nil, &uuidBenchLedger{t: t})
	if n, err := b.AbandonStale(f.ctx); err != nil || n != 0 {
		t.Fatalf("abandon without a store = %d (%v)", n, err)
	}
	if _, err := b.Run(f.ctx, nil); err != nil {
		t.Fatal(err)
	}
	if ids := destroyedIDs(f.provider); strings.Join(ids, ",") != "clone-1" {
		t.Fatalf("destroyed %v, want only this run's clone", ids)
	}
}
