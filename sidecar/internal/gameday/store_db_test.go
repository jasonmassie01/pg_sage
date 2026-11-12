package gameday

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/pg-sage/sidecar/internal/clone"
)

// benches-03: a process that stops mid-run (crash, kill -9, redeploy)
// leaves its game day "running" with a clone nobody destroys. The clone
// id is recorded right after Create, and runs still running MaxRunTime
// after they started are marked abandoned and their clones destroyed.

// insertRun records a run directly, as a stopped process left it.
func insertRun(t *testing.T, f *gdFixture, database, kind, status string,
	started time.Time, cloneID string) string {
	t.Helper()
	id := newUUID(t)
	var finished *time.Time
	if status != StatusRunning {
		at := started.Add(time.Minute)
		finished = &at
	}
	_, err := f.store.pool.Exec(f.ctx, `INSERT INTO sage.sre_game_days
		(deployment_id, id, database_name, provider, families, status, started_at,
		 finished_at, clone_id, kind)
		VALUES ($1, $2, $3, 'dle', '{}', $4, $5, $6, NULLIF($7, ''), $8)`,
		f.store.deployment, id, database, status, started, finished, cloneID, kind)
	if err != nil {
		t.Fatalf("insert %s %s run: %v", kind, status, err)
	}
	return id
}

type storedRun struct {
	status, err, cloneID string
	finished             *time.Time
}

func readRun(t *testing.T, f *gdFixture, id string) storedRun {
	t.Helper()
	var r storedRun
	err := f.store.pool.QueryRow(f.ctx, `SELECT status, COALESCE(error, ''),
		COALESCE(clone_id, ''), finished_at FROM sage.sre_game_days
		WHERE deployment_id = $1 AND id = $2`, f.store.deployment, id).
		Scan(&r.status, &r.err, &r.cloneID, &r.finished)
	if err != nil {
		t.Fatalf("read run %s: %v", id, err)
	}
	return r
}

func destroyedIDs(p *fakeProvider) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []string{}
	for _, c := range p.destroyed {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out
}

// State transition: running -> abandoned, and Destroy gets the stored
// clone id. A fresh running row is left alone (its run may be live).
func TestAbandonStaleRunDestroysItsRecordedClone(t *testing.T) {
	f := newGDFixture(t)
	stale := insertRun(t, f, f.database, "game_day", StatusRunning,
		f.now.Add(-MaxRunTime-time.Minute), "clone-stale")
	fresh := insertRun(t, f, f.database, "game_day", StatusRunning,
		f.now.Add(-time.Hour), "clone-fresh")
	n, err := f.runner().AbandonStale(f.ctx)
	if err != nil || n != 1 {
		t.Fatalf("abandon = %d (%v), want 1", n, err)
	}
	got := readRun(t, f, stale)
	if got.status != StatusAbandoned || got.finished == nil || !got.finished.Equal(f.now) ||
		!strings.Contains(got.err, "abandoned") || got.cloneID != "clone-stale" {
		t.Fatalf("stale run = %+v", got)
	}
	if ids := destroyedIDs(f.provider); strings.Join(ids, ",") != "clone-stale" {
		t.Fatalf("destroyed %v, want [clone-stale]", ids)
	}
	if got := readRun(t, f, fresh); got.status != StatusRunning || got.finished != nil {
		t.Fatalf("fresh run = %+v, want running", got)
	}
	// Abandoning again does nothing: the row is no longer running.
	if n, err := f.runner().AbandonStale(f.ctx); err != nil || n != 0 {
		t.Fatalf("second abandon = %d (%v), want 0", n, err)
	}
	if ids := destroyedIDs(f.provider); len(ids) != 1 {
		t.Fatalf("the clone was destroyed again: %v", ids)
	}
}

// Boundary: a run exactly MaxRunTime old may still be live; one second
// older is abandoned.
func TestAbandonStaleBoundaryIsMaxRunTime(t *testing.T) {
	f := newGDFixture(t)
	edge := insertRun(t, f, f.database, "game_day", StatusRunning,
		f.now.Add(-MaxRunTime), "clone-edge")
	past := insertRun(t, f, f.database, "game_day", StatusRunning,
		f.now.Add(-MaxRunTime-time.Second), "clone-past")
	if n, err := f.runner().AbandonStale(f.ctx); err != nil || n != 1 {
		t.Fatalf("abandon = %d (%v), want 1", n, err)
	}
	if got := readRun(t, f, edge); got.status != StatusRunning {
		t.Fatalf("a run exactly MaxRunTime old = %+v, want running", got)
	}
	if got := readRun(t, f, past); got.status != StatusAbandoned {
		t.Fatalf("a run past MaxRunTime = %+v, want abandoned", got)
	}
	if MaxRunTime != 6*time.Hour {
		t.Fatalf("MaxRunTime = %s, want 6h", MaxRunTime)
	}
}

// Nil/empty: a run that stopped before its clone was created has no
// clone id; it is abandoned and Destroy is not called.
func TestAbandonStaleRunWithoutACloneDestroysNothing(t *testing.T) {
	f := newGDFixture(t)
	id := insertRun(t, f, f.database, "game_day", StatusRunning,
		f.now.Add(-7*time.Hour), "")
	if n, err := f.runner().AbandonStale(f.ctx); err != nil || n != 1 {
		t.Fatalf("abandon = %d (%v), want 1", n, err)
	}
	if got := readRun(t, f, id); got.status != StatusAbandoned || got.cloneID != "" {
		t.Fatalf("run = %+v", got)
	}
	if ids := destroyedIDs(f.provider); len(ids) != 0 {
		t.Fatalf("destroyed %v, want nothing", ids)
	}
}

// Error propagation: a failed Destroy is recorded on its row, which is
// still abandoned, and the sweep goes on to the next run.
func TestAbandonStaleDestroyFailureIsRecordedAndTheSweepGoesOn(t *testing.T) {
	f := newGDFixture(t)
	f.provider.destroyErr = errors.New("DLE destroy failed with status 503")
	a := insertRun(t, f, f.database, "game_day", StatusRunning,
		f.now.Add(-8*time.Hour), "clone-a")
	b := insertRun(t, f, f.database, "game_day", StatusRunning,
		f.now.Add(-9*time.Hour), "clone-b")
	n, err := f.runner().AbandonStale(f.ctx)
	if n != 2 || err == nil || !strings.Contains(err.Error(), "status 503") ||
		!strings.Contains(err.Error(), "clone-a") || !strings.Contains(err.Error(), "clone-b") {
		t.Fatalf("abandon = %d (%v), want 2 with both destroy errors", n, err)
	}
	for id, cloneID := range map[string]string{a: "clone-a", b: "clone-b"} {
		got := readRun(t, f, id)
		if got.status != StatusAbandoned || !strings.Contains(got.err, "status 503") ||
			!strings.Contains(got.err, "destroy clone "+cloneID) {
			t.Fatalf("run %s = %+v", cloneID, got)
		}
	}
	if ids := destroyedIDs(f.provider); strings.Join(ids, ",") != "clone-a,clone-b" {
		t.Fatalf("destroy attempts %v, want both clones", ids)
	}
}

// Only this database's runs of the runner's own kind are swept: other
// databases, finished runs and local bench runs are not the game-day
// runner's (the local bench sweeps its own).
func TestAbandonStaleTouchesOnlyItsOwnRuns(t *testing.T) {
	f := newGDFixture(t)
	old := f.now.Add(-10 * time.Hour)
	other := insertRun(t, f, "billing-"+newUUID(t)[:8], "game_day", StatusRunning, old, "c-other")
	done := insertRun(t, f, f.database, "game_day", StatusCompleted, old, "c-done")
	local := insertRun(t, f, f.database, "local", StatusRunning, old, "c-local")
	if n, err := f.runner().AbandonStale(f.ctx); err != nil || n != 0 {
		t.Fatalf("abandon = %d (%v), want 0", n, err)
	}
	for id, want := range map[string]string{other: StatusRunning, done: StatusCompleted,
		local: StatusRunning} {
		if got := readRun(t, f, id); got.status != want {
			t.Fatalf("run %s = %+v, want %s", id, got, want)
		}
	}
	b, err := NewLocalBench(LocalBenchConfig{Database: f.database, Provider: "dle",
		Store: f.store, Now: func() time.Time { return f.now }}, f.provider, f.faults,
		&fakeBenchLedger{})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := b.AbandonStale(f.ctx); err != nil || n != 1 {
		t.Fatalf("local abandon = %d (%v), want 1", n, err)
	}
	if got := readRun(t, f, local); got.status != StatusAbandoned {
		t.Fatalf("local run = %+v, want abandoned", got)
	}
	if ids := destroyedIDs(f.provider); strings.Join(ids, ",") != "c-local" {
		t.Fatalf("destroyed %v, want [c-local]", ids)
	}
}

// State transition: while this runner's own game day runs, nothing is
// swept (it cannot tell its live run from a stale one); once it is done,
// the stale run is abandoned.
func TestAbandonStaleWaitsWhileThisRunnerRuns(t *testing.T) {
	f := newGDFixture(t)
	stale := insertRun(t, f, f.database, "game_day", StatusRunning,
		f.now.Add(-7*time.Hour), "clone-stale")
	f.faults.block = make(chan struct{})
	r := f.runner()
	if err := r.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := r.AbandonStale(f.ctx); err != nil || n != 0 {
		t.Fatalf("abandon while running = %d (%v), want 0", n, err)
	}
	if got := readRun(t, f, stale); got.status != StatusRunning {
		t.Fatalf("stale run swept while the runner ran: %+v", got)
	}
	close(f.faults.block)
	waitIdle(t, r)
	if n, err := r.AbandonStale(f.ctx); err != nil || n != 1 {
		t.Fatalf("abandon after the run = %d (%v), want 1", n, err)
	}
}

func waitIdle(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for r.running.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the game day did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Concurrent access: two processes sweeping at once destroy each stale
// clone exactly once (the row is claimed before its clone is destroyed).
func TestAbandonStaleConcurrentSweepsDestroyEachCloneOnce(t *testing.T) {
	f := newGDFixture(t)
	want := []string{}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("clone-%02d", i)
		insertRun(t, f, f.database, "game_day", StatusRunning,
			f.now.Add(-7*time.Hour-time.Duration(i)*time.Minute), id)
		want = append(want, id)
	}
	providers := []*fakeProvider{{}, {}, {}}
	var wg sync.WaitGroup
	total := make([]int, len(providers))
	for i, p := range providers {
		r, err := NewRunner(Config{Database: f.database, Provider: "dle",
			Now: func() time.Time { return f.now }}, p, f.faults, f.ledger, f.store)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			n, err := r.AbandonStale(f.ctx)
			if err != nil {
				t.Errorf("sweep %d: %v", i, err)
			}
			total[i] = n
		}(i)
	}
	wg.Wait()
	got := []string{}
	for _, p := range providers {
		got = append(got, destroyedIDs(p)...)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") ||
		total[0]+total[1]+total[2] != len(want) {
		t.Fatalf("destroyed %v (counts %v), want each of %v once", got, total, want)
	}
}

// Error propagation: an unreachable store is an error naming the sweep,
// and nothing is destroyed.
func TestAbandonStaleStoreErrorIsReported(t *testing.T) {
	f := newGDFixture(t)
	closed := closedStore(t, f)
	r, err := NewRunner(Config{Database: f.database, Provider: "dle"}, f.provider,
		f.faults, f.ledger, closed)
	if err != nil {
		t.Fatal(err)
	}
	n, err := r.AbandonStale(f.ctx)
	if n != 0 || err == nil || !strings.Contains(err.Error(), "abandon stale") {
		t.Fatalf("abandon on a closed pool = %d (%v)", n, err)
	}
	if ids := destroyedIDs(f.provider); len(ids) != 0 {
		t.Fatalf("destroyed %v", ids)
	}
}

// The clone id is on the row while the run is still exercising it, so a
// process that stops then leaves a clone the sweep can destroy.
func TestGameDayRecordsTheCloneIDBeforeTheFaultsRun(t *testing.T) {
	f := newGDFixture(t)
	f.faults.block = make(chan struct{})
	r := f.runner()
	if err := r.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	var running []GameDay
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		running, err = r.List(f.ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(running) == 1 && running[0].CloneID != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(f.faults.block)
	waitIdle(t, r)
	if len(running) != 1 || running[0].Status != StatusRunning ||
		running[0].CloneID != "clone-1" {
		t.Fatalf("while running = %+v, want running with clone-1", running)
	}
}

// A local bench run is not a game day: it neither shows in the game-day
// list nor postpones the next scheduled game day.
func TestLocalRunsAreNotGameDays(t *testing.T) {
	f := newGDFixture(t)
	insertRun(t, f, f.database, "local", StatusCompleted, f.now.Add(-time.Hour), "c-local")
	r := f.runner()
	if listed, err := r.List(f.ctx, 10); err != nil || len(listed) != 0 {
		t.Fatalf("game days = %+v (%v), want none", listed, err)
	}
	if last, err := f.store.Latest(f.ctx, f.database); err != nil || last != nil {
		t.Fatalf("latest game day = %+v (%v), want none", last, err)
	}
	if _, ran, err := r.RunDue(f.ctx); err != nil || !ran {
		t.Fatalf("run due = %v (%v): a local run postponed the game day", ran, err)
	}
}

// failingCloneRecord deletes the run's row when the clone is created, so
// recording the clone id fails.
type failingCloneRecord struct {
	*fakeProvider
	f *gdFixture
}

func (p failingCloneRecord) Create(ctx context.Context, spec clone.CloneSpec) (clone.Clone,
	error) {
	if _, err := p.f.store.pool.Exec(ctx, `DELETE FROM sage.sre_game_days
		WHERE deployment_id = $1`, p.f.store.deployment); err != nil {
		return clone.Clone{}, err
	}
	return p.fakeProvider.Create(ctx, spec)
}

// Fail closed: a clone whose id cannot be recorded is destroyed at once
// and the fault programs never run on it.
func TestGameDayDestroysACloneItCannotRecord(t *testing.T) {
	f := newGDFixture(t)
	p := failingCloneRecord{fakeProvider: f.provider, f: f}
	r, err := NewRunner(Config{Database: f.database, Provider: "dle",
		Now: func() time.Time { return f.now }}, p, f.faults, f.ledger, f.store)
	if err != nil {
		t.Fatal(err)
	}
	gd, err := r.Run(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "record clone") {
		t.Fatalf("run = %+v (%v), want a record-clone error", gd, err)
	}
	if ids := destroyedIDs(f.provider); strings.Join(ids, ",") != "clone-1" {
		t.Fatalf("destroyed %v, want [clone-1]", ids)
	}
	if len(f.faults.dsns) != 0 {
		t.Fatalf("fault programs ran on an unrecorded clone: %v", f.faults.dsns)
	}
}

// benches-08 (real PostgreSQL): a run's summary is stored with its row
// and read back unchanged by List and Latest.
func TestStoreSummaryRoundTrips(t *testing.T) {
	f := newGDFixture(t)
	at := f.now.Add(time.Minute)
	gd := GameDay{ID: newUUID(t), Database: f.database, Provider: "dle",
		Families: []string{}, Status: StatusRunning, StartedAt: f.now}
	if err := f.store.start(f.ctx, gd, rowGameDay); err != nil {
		t.Fatal(err)
	}
	gd.Status, gd.FinishedAt = StatusCompletedUnscored, &at
	gd.Summary = &Summary{Scored: 0, Skipped: 2, Errored: 1, TopSkips: []SkipCount{
		{Reason: "needs superuser", Count: 2}}}
	if err := f.store.finish(f.ctx, gd); err != nil {
		t.Fatal(err)
	}
	list, err := f.store.List(f.ctx, f.database, 10)
	if err != nil || len(list) != 1 || !reflect.DeepEqual(list[0].Summary, gd.Summary) ||
		list[0].Status != StatusCompletedUnscored {
		t.Fatalf("list = %+v (%v), want summary %+v", list, err, gd.Summary)
	}
	latest, err := f.store.Latest(f.ctx, f.database)
	if err != nil || latest == nil || !reflect.DeepEqual(latest.Summary, gd.Summary) {
		t.Fatalf("latest = %+v (%v)", latest, err)
	}
}

// A row written before benches-08 (or a run that failed before any
// report) holds '{}' and reads as no summary, not as "scored 0".
func TestStoreLegacyRowReadsAsNoSummary(t *testing.T) {
	f := newGDFixture(t)
	id := insertRun(t, f, f.database, "game_day", StatusCompleted, f.now, "")
	var raw string
	if err := f.store.pool.QueryRow(f.ctx, `SELECT summary::text FROM sage.sre_game_days
		WHERE deployment_id = $1 AND id = $2`, f.store.deployment, id).Scan(&raw); err != nil ||
		raw != "{}" {
		t.Fatalf("legacy summary = %q (%v), want {}", raw, err)
	}
	list, err := f.store.List(f.ctx, f.database, 10)
	if err != nil || len(list) != 1 || list[0].Summary != nil {
		t.Fatalf("list = %+v (%v), want no summary", list, err)
	}
	failed := GameDay{ID: newUUID(t), Database: "other-" + f.database, Provider: "dle",
		Families: []string{}, Status: StatusRunning, StartedAt: f.now}
	if err := f.store.start(f.ctx, failed, rowGameDay); err != nil {
		t.Fatal(err)
	}
	at := f.now.Add(time.Minute)
	failed.Status, failed.Error, failed.FinishedAt = StatusFailed, "create clone: no snapshot",
		&at
	if err := f.store.finish(f.ctx, failed); err != nil {
		t.Fatal(err)
	}
	if got, err := f.store.Latest(f.ctx, failed.Database); err != nil || got == nil ||
		got.Summary != nil || got.Status != StatusFailed {
		t.Fatalf("failed run = %+v (%v), want no summary", got, err)
	}
}

// The status check takes completed_unscored and still refuses an unknown
// status; the summary must be a JSON object. Each refusal must come from
// that constraint (finished_at is set so the running/finished_at check
// cannot be the one that refuses the row).
func TestStoreStatusCheckTakesCompletedUnscored(t *testing.T) {
	f := newGDFixture(t)
	insertRun(t, f, f.database, "game_day", StatusCompletedUnscored, f.now, "")
	_, err := f.store.pool.Exec(f.ctx, `INSERT INTO sage.sre_game_days (deployment_id, id,
		database_name, provider, families, status, finished_at) VALUES ($1, $2, $3, 'dle',
		'{}', 'unscored', now())`, f.store.deployment, newUUID(t), f.database)
	requireCheckViolation(t, err, "sre_game_days_status_v2_check")
	_, err = f.store.pool.Exec(f.ctx, `INSERT INTO sage.sre_game_days (deployment_id, id,
		database_name, provider, families, status, summary) VALUES ($1, $2, $3, 'dle', '{}',
		'running', '[]')`, f.store.deployment, newUUID(t), f.database)
	requireCheckViolation(t, err, "sre_game_days_summary_object")
}

// requireCheckViolation fails unless err is a check violation (23514) of
// the named constraint.
func requireCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
		t.Fatalf("err = %v, want a check violation of %s", err, constraint)
	}
}
