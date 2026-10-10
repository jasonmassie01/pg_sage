package agentposture

import (
	"testing"
	"time"
)

// seedCheckpoints creates LangGraph's checkpoint tables in the fixture schema.
func seedCheckpoints(f *fixture) {
	f.exec("CREATE TABLE "+f.q("checkpoints")+" (thread_id text, payload text)",
		"CREATE TABLE "+f.q("checkpoint_blobs")+" (thread_id text, blob bytea)",
		"CREATE TABLE "+f.q("checkpoint_writes")+" (thread_id text, w text)")
}

// backdate moves the stored AP-12 observation a day into the past, as if
// the daily run had taken it yesterday.
func backdate(t *testing.T, s *ObservationStore) {
	t.Helper()
	prev, ok := s.Previous("AP-12")
	if !ok {
		t.Fatal("AP-12 recorded no observation")
	}
	prev.At = prev.At.Add(-24 * time.Hour)
	s.Record("AP-12", prev)
}

// G0-03 / AP-12: two timed observations. The first run records and
// reports nothing; a run a day later compares and reports the growth;
// once rows are deleted the store is not "growing with no deletes".
func TestAP12_TwoTimedObservations(t *testing.T) {
	f := newFixture(t)
	seedCheckpoints(f)
	store := NewObservationStore()
	env := f.env(func(e *Env) {
		e.Observations = store
		e.Config.MemoryGrowthGBDay = 1e-6 // about 1 kB a day, so a few rows count
	})
	if o := f.run("AP-12", env); len(o.Findings) != 0 {
		t.Fatalf("first observation reported %+v; it must only record", o.Findings)
	}
	first, ok := store.Previous("AP-12")
	if !ok || first.Values[f.schema].Tables == nil {
		t.Fatalf("first run recorded %+v, want the fixture schema", first)
	}

	f.exec("INSERT INTO " + f.q("checkpoints") +
		" SELECT g::text, repeat('x', 2000) FROM generate_series(1, 2000) g")
	if o := f.run("AP-12", env); len(o.Findings) != 0 {
		t.Fatalf("a second run within the hour compared: %+v", o.Findings)
	}
	backdate(t, store)
	got := requireFinding(t, f.run("AP-12", env), f.schema, Info)
	requireContains(t, "AP-12 detail", got.Detail, "checkpoints", "no rows deleted")

	backdate(t, store)
	f.exec("INSERT INTO "+f.q("checkpoints")+
		" SELECT g::text, repeat('y', 2000) FROM generate_series(1, 2000) g",
		"DELETE FROM "+f.q("checkpoints")+" WHERE thread_id = '1'")
	waitForDeleteStats(t, f, "checkpoints")
	requireNoFinding(t, f.run("AP-12", env), f.schema)
}

// waitForDeleteStats waits until the cumulative statistics show the delete
// (PG15+ flushes them at most once a second; PG14 through the collector).
func waitForDeleteStats(t *testing.T, f *fixture, table string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		if err := f.pool.QueryRow(f.ctx, "SELECT pg_stat_get_tuples_deleted($1::regclass)",
			f.q(table)).Scan(&n); err != nil {
			t.Fatalf("delete stats: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the delete on %s never reached the statistics", table)
}

// The first look runs AP-12 with no store: no error and no finding.
func TestAP12_NoStoreInTheFirstLook(t *testing.T) {
	f := newFixture(t)
	seedCheckpoints(f)
	o := f.run("AP-12", f.env(func(e *Env) { e.Observations = nil }))
	if len(o.Findings) != 0 {
		t.Fatalf("AP-12 without a store reported %+v", o.Findings)
	}
}

// Schemas without LangGraph's tables are not observed.
func TestAP12_IgnoresOtherTables(t *testing.T) {
	f := newFixture(t)
	f.exec("CREATE TABLE " + f.q("checkpoint_history") + " (id int)")
	store := NewObservationStore()
	f.run("AP-12", f.env(func(e *Env) { e.Observations = store }))
	if prev, _ := store.Previous("AP-12"); prev.Values[f.schema].Tables != nil {
		t.Fatalf("AP-12 observed a schema without checkpoint tables: %+v", prev)
	}
}

// RunAll hands the monitor's store to AP-12 across runs.
func TestAP12_RunAllKeepsObservationsBetweenRuns(t *testing.T) {
	f := newFixture(t)
	seedCheckpoints(f)
	d, _ := Default().Get("AP-12")
	reg := NewRegistry()
	if err := reg.Register(d); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := NewObservationStore()
	cfg := DefaultConfig()
	cfg.ClientPatterns = nil
	cfg.MemoryGrowthGBDay = 1e-6
	opts := RunOptions{Registry: reg, Config: cfg, Observations: store}
	if _, err := RunAll(f.ctx, f.pool, opts); err != nil {
		t.Fatalf("first run: %v", err)
	}
	backdate(t, store)
	f.exec("INSERT INTO " + f.q("checkpoint_blobs") +
		" SELECT g::text, convert_to(repeat('z', 3000), 'UTF8') FROM generate_series(1, 500) g")
	res, err := RunAll(f.ctx, f.pool, opts)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if findObject(res.Findings(), f.schema) == nil {
		t.Fatalf("second RunAll did not compare with the first: %+v (failed %v)",
			res.Findings(), res.Failed)
	}
}
