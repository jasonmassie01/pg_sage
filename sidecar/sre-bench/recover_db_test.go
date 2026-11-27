package srebench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// benches-03: a run cut off mid-scenario (shutdown cancels its context, a
// crash or kill -9 ends the process) must not leave cluster-wide fixtures
// behind. Recover runs on a context of its own, so a cancelled run still
// drops its slot and resets its ALTER SYSTEM setting; and the next run's
// OpenEnv sweeps what a crashed process left: settings recorded in
// sage.bench_fixture_state, inactive bench slots, and bench prepared
// transactions older than leftoverAge.

// cancellingArm is a shutdown during the investigation: it cancels the
// run's context and fails the way a cancelled investigation does.
type cancellingArm struct {
	cancel context.CancelFunc
	during func(ctx context.Context, e *Env) error
	seen   error
}

func (cancellingArm) Name() string          { return "cancelling" }
func (cancellingArm) Ready() (bool, string) { return true, "" }

func (a *cancellingArm) Investigate(ctx context.Context, e *Env, _ Scenario) (Trace,
	error) {
	if a.during != nil {
		a.seen = a.during(ctx, e)
	}
	a.cancel()
	return Trace{}, ctx.Err()
}

func scenarioByID(t *testing.T, id string) Scenario {
	t.Helper()
	for _, sc := range Scenarios() {
		if sc.ID == id {
			return sc
		}
	}
	t.Fatalf("no scenario %q", id)
	return Scenario{}
}

// autoConfSettings counts settings postgresql.auto.conf sets among names.
func autoConfSettings(ctx context.Context, t *testing.T, e *Env, names ...string) int {
	t.Helper()
	n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_settings
		WHERE name = ANY($1) AND sourcefile LIKE '%postgresql.auto.conf'`, names)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func fixtureRows(ctx context.Context, t *testing.T, e *Env) int {
	t.Helper()
	n, err := e.count(ctx, "SELECT count(*) FROM sage.bench_fixture_state")
	if err != nil {
		t.Fatalf("read sage.bench_fixture_state: %v", err)
	}
	return n
}

func slotCount(ctx context.Context, t *testing.T, e *Env, prefix string) int {
	t.Helper()
	n, err := e.count(ctx, `SELECT count(*) FROM pg_catalog.pg_replication_slots
		WHERE starts_with(slot_name::text, $1)`, prefix)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// cleanupFixtures undoes, whatever the test reached, what it may leave.
func cleanupFixtures(t *testing.T, e *Env, settings []string, slotPrefixes ...string) {
	t.Cleanup(func() {
		ctx := context.Background()
		for _, s := range settings {
			_, _ = e.Pool.Exec(ctx, "ALTER SYSTEM RESET "+s)
		}
		_, _ = e.Pool.Exec(ctx, "SELECT pg_reload_conf()")
		_, _ = e.Pool.Exec(ctx, "DELETE FROM sage.bench_fixture_state")
		for _, p := range slotPrefixes {
			_, _ = e.Pool.Exec(ctx, `SELECT pg_drop_replication_slot(slot_name)
				FROM pg_catalog.pg_replication_slots
				WHERE starts_with(slot_name::text, $1) AND NOT active`, p)
		}
		_ = truncateWAL(ctx, e)
	})
}

// Integration: shutdown cancels the run's context during the
// investigation of an inactive-slot and an undersized-max_wal_size
// scenario; the slot is still dropped and max_wal_size still reset.
func TestRecoverRunsAfterTheRunContextIsCancelled(t *testing.T) {
	_, e := liveEnv(t)
	cleanupFixtures(t, e, []string{"max_wal_size"}, slotPrefix)
	for _, id := range []string{"wal-inactive-slot", "ckpt-max-wal-size-undersized"} {
		t.Run(id, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			arm := &cancellingArm{cancel: cancel, during: func(ctx context.Context,
				e *Env) error {
				if n := slotCount(ctx, t, e, slotPrefix) + autoConfSettings(ctx, t, e,
					"max_wal_size"); n != 1 {
					return fmt.Errorf("%d fixtures present during the run, want 1", n)
				}
				return nil
			}}
			r := e.attempt(ctx, scenarioByID(t, id), arm)
			if arm.seen != nil {
				t.Fatalf("before the cancel: %v", arm.seen)
			}
			if r.Err == nil || !errors.Is(r.Err, context.Canceled) ||
				strings.Contains(r.Err.Error(), "recover") {
				t.Fatalf("attempt err = %v, want the cancelled investigation only", r.Err)
			}
			bg := context.Background()
			if n := slotCount(bg, t, e, slotPrefix); n != 0 {
				t.Fatalf("%d bench slot(s) left after a cancelled run", n)
			}
			if n := autoConfSettings(bg, t, e, "max_wal_size"); n != 0 {
				t.Fatal("max_wal_size is still set by ALTER SYSTEM after a cancelled run")
			}
			if n := fixtureRows(bg, t, e); n != 0 {
				t.Fatalf("%d bench_fixture_state row(s) left after recovery", n)
			}
		})
	}
}

// alterSystem records the setting before it changes it and resetSystem
// forgets it; a refused ALTER SYSTEM leaves nothing recorded.
func TestAlterSystemRecordsTheSettingItChanges(t *testing.T) {
	ctx, e := liveEnv(t)
	cleanupFixtures(t, e, []string{"max_wal_size"})
	if err := e.lockCluster(ctx); err != nil {
		t.Fatal(err)
	}
	defer e.unlockCluster()
	var u *Unsupported
	if err := e.alterSystem(ctx, "max_wal_size", "not a size"); !errors.As(err, &u) {
		t.Fatalf("an invalid value: err = %v, want Unsupported", err)
	}
	if n := fixtureRows(ctx, t, e); n != 0 {
		t.Fatalf("a refused ALTER SYSTEM left %d recorded setting(s)", n)
	}
	if err := e.alterSystem(ctx, "max_wal_size", undersizedWAL); err != nil {
		t.Fatal(err)
	}
	n, err := e.count(ctx, `SELECT count(*) FROM sage.bench_fixture_state
		WHERE setting = 'max_wal_size'`)
	if err != nil || n != 1 {
		t.Fatalf("recorded max_wal_size rows = %d (%v), want 1", n, err)
	}
	if err := e.resetSystem(ctx, "max_wal_size"); err != nil {
		t.Fatal(err)
	}
	if n := fixtureRows(ctx, t, e); n != 0 {
		t.Fatalf("reset left %d recorded setting(s)", n)
	}
}

// Integration: a crashed run's physical slot and recorded max_wal_size
// are removed by the next OpenEnv, so the checkpoint program then runs
// instead of skipping ("already set by ALTER SYSTEM").
func TestSweepRemovesACrashedRunsLeftovers(t *testing.T) {
	ctx, e := liveEnv(t)
	crashed := "bench_slot_0badc0de_"
	cleanupFixtures(t, e, []string{"max_wal_size"}, crashed, slotPrefix)
	mustExec(ctx, t, e, "SELECT pg_create_physical_replication_slot($1, true)",
		uniqueName(crashed))
	mustExec(ctx, t, e, "INSERT INTO sage.bench_fixture_state (setting) VALUES ($1)",
		"max_wal_size")
	mustExec(ctx, t, e, "ALTER SYSTEM SET max_wal_size = '48MB'")
	mustExec(ctx, t, e, "SELECT pg_reload_conf()")

	next, closeNext, err := OpenEnv(ctx, e.DSN)
	if err != nil {
		t.Fatalf("open the next run's env: %v", err)
	}
	defer closeNext()
	if n := slotCount(ctx, t, next, crashed); n != 0 {
		t.Fatalf("the crashed run's slot survived the sweep (%d)", n)
	}
	if n := autoConfSettings(ctx, t, next, "max_wal_size"); n != 0 {
		t.Fatal("the crashed run's max_wal_size survived the sweep")
	}
	if n := fixtureRows(ctx, t, next); n != 0 {
		t.Fatalf("%d recorded setting(s) survived the sweep", n)
	}
	p := undersizedMaxWAL()
	injectErr := p.Inject(ctx, next)
	recoverErr := p.Recover(ctx, next)
	next.unlockCluster()
	if injectErr != nil || recoverErr != nil {
		t.Fatalf("checkpoint program after the sweep: inject %v, recover %v", injectErr,
			recoverErr)
	}
}

// What no bench recorded or named is never touched: an operator's ALTER
// SYSTEM setting and an application's inactive slot survive the sweep.
func TestSweepLeavesWhatNoBenchMade(t *testing.T) {
	ctx, e := liveEnv(t)
	appSlot := uniqueName("app_slot_")
	cleanupFixtures(t, e, []string{"checkpoint_completion_target"}, appSlot)
	mustExec(ctx, t, e, "SELECT pg_create_physical_replication_slot($1)", appSlot)
	mustExec(ctx, t, e, "ALTER SYSTEM SET checkpoint_completion_target = 0.8")
	mustExec(ctx, t, e, "SELECT pg_reload_conf()")
	if err := sweepLeftovers(ctx, e, leftoverAge); err != nil {
		t.Fatal(err)
	}
	if n := slotCount(ctx, t, e, appSlot); n != 1 {
		t.Fatal("the sweep dropped an application's slot")
	}
	if n := autoConfSettings(ctx, t, e, "checkpoint_completion_target"); n != 1 {
		t.Fatal("the sweep reset an operator's ALTER SYSTEM setting")
	}
}

// Concurrent access: the sweep waits for the WAL cluster lock, so it never
// drops the slot of a WAL program another process is running.
func TestSweepWaitsForTheClusterLock(t *testing.T) {
	ctx, e := liveEnv(t)
	live := "bench_slot_11ve0000_"
	cleanupFixtures(t, e, nil, live)
	mustExec(ctx, t, e, "SELECT pg_create_physical_replication_slot($1)", uniqueName(live))
	release, err := lockCluster(ctx, e.DSN)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err = sweepLeftovers(short, e, leftoverAge)
	release()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sweep while another holds the lock: err = %v, want the deadline", err)
	}
	if n := slotCount(ctx, t, e, live); n != 1 {
		t.Fatal("the sweep dropped a slot while another process held the WAL lock")
	}
	if err := sweepLeftovers(ctx, e, leftoverAge); err != nil {
		t.Fatal(err)
	}
	if n := slotCount(ctx, t, e, live); n != 0 {
		t.Fatal("once the lock was free the leftover slot was not dropped")
	}
}

// preparedEnv is a live env with prepared transactions enabled.
func preparedEnv(t *testing.T) (context.Context, *Env) {
	t.Helper()
	ctx, e := liveEnv(t)
	n, err := e.count(ctx, "SELECT current_setting('max_prepared_transactions')::int")
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Skip("max_prepared_transactions is 0 on the test server; the sweep's " +
			"prepared-transaction checks need it above 0")
	}
	return ctx, e
}

func preparedCount(ctx context.Context, t *testing.T, e *Env, gid string) int {
	t.Helper()
	n, err := e.count(ctx, "SELECT count(*) FROM pg_catalog.pg_prepared_xacts WHERE gid = $1",
		gid)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Boundary: a bench prepared transaction younger than leftoverAge may be
// a live run's and survives; once older it is rolled back. An
// application's prepared transaction is never touched, whatever its age.
func TestSweepRollsBackOnlyOldBenchPreparedTransactions(t *testing.T) {
	ctx, e := preparedEnv(t)
	legacy := fmt.Sprintf("%s%d", legacyGIDNamespace, time.Now().UnixNano())
	wrap, app := uniqueName(gidPrefix), fmt.Sprintf("app_txn_%d", time.Now().UnixNano())
	for _, gid := range []string{legacy, wrap, app} {
		if err := e.prepareTransaction(ctx, gid); err != nil {
			t.Fatal(err)
		}
		cleanupForeign(t, e, gid, "", 0)
	}
	if err := sweepLeftovers(ctx, e, leftoverAge); err != nil {
		t.Fatal(err)
	}
	for _, gid := range []string{legacy, wrap, app} {
		if preparedCount(ctx, t, e, gid) != 1 {
			t.Fatalf("%s younger than %s was rolled back", gid, leftoverAge)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	if err := sweepLeftovers(ctx, e, time.Second); err != nil {
		t.Fatal(err)
	}
	if preparedCount(ctx, t, e, legacy)+preparedCount(ctx, t, e, wrap) != 0 {
		t.Fatal("bench prepared transactions older than the cutoff survived the sweep")
	}
	if preparedCount(ctx, t, e, app) != 1 {
		t.Fatal("the sweep rolled back an application's prepared transaction")
	}
	if leftoverAge != 6*time.Hour {
		t.Fatalf("leftoverAge = %s, want the 6 h maximum run time", leftoverAge)
	}
}
