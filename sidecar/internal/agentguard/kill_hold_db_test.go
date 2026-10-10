package agentguard

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-sage/sidecar/internal/testsupport/require"
)

// §6.2.7: a non-narrowing agent apply holds FOR SHARE on its principal's
// row in the control database until the target COMMIT returns
// (decide.HoldActive). The kill takes FOR UPDATE on the same rows with
// lock_timeout 2 s: it waits for an apply that commits, cancels one that
// does not (by its guard_inflight PID), and never deadlocks. An apply can
// never commit after the freeze, and none can start after it.

const holdSQL = `SELECT 1 FROM sage.guard_principals WHERE id = $1 AND status = 'active'
	FOR SHARE`

// hold opens the apply's control transaction and takes the share lock.
func hold(t *testing.T, f *killFixture, p Principal) pgx.Tx {
	t.Helper()
	tx, err := f.super.Begin(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var one int
	require.NoError(t, tx.QueryRow(context.Background(), holdSQL, p.ID).Scan(&one))
	return tx
}

func TestKill_WaitsForAnApplyThatCommits(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	tx := hold(t, f, p)
	type result struct {
		rep KillReport
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	begin := time.Now()
	go func() {
		rep, err := f.sw.Kill(ctx, killPrincipal(p))
		done <- result{rep, err, time.Now()}
	}()
	time.Sleep(800 * time.Millisecond)
	commitAt := time.Now()
	require.NoError(t, tx.Commit(ctx))
	var r result
	select {
	case r = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the kill did not finish after the apply committed")
	}
	require.NoError(t, r.err)
	require.GreaterOrEqual(t, r.at.Sub(begin), 700*time.Millisecond)
	got := mustGet(t, f.store, p.ID)
	require.Equal(t, StatusFrozen, got.Status)
	require.False(t, got.UpdatedAt.Before(commitAt.Add(-50*time.Millisecond)),
		"the freeze committed after the apply, not before")
	require.Empty(t, r.rep.ControlError)
}

func TestKill_CancelsAnApplyThatWouldCommitAfterTheFreeze(t *testing.T) {
	f := newKillFixture(t)
	p, _ := f.ensured(t)
	ctx := context.Background()
	const dbID = "00000000-0000-4000-8000-00000000c112"
	f.targets[0].DatabaseID = dbID
	f.rebuild(t)
	tx := hold(t, f, p)
	target, err := f.admin.Acquire(ctx)
	require.NoError(t, err)
	defer target.Release()
	var pid int
	require.NoError(t, target.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid))
	require.NoError(t, RegisterInflight(ctx, f.super, p.ID, dbID, pid, 0))
	t.Cleanup(func() { _ = UnregisterInflight(context.Background(), f.super, dbID, pid) })
	committed := make(chan bool, 1)
	go func() {
		// The apply: its target statement, then COMMIT of the hold.
		_, err := target.Exec(ctx, "SELECT pg_sleep(20)")
		if err != nil {
			_ = tx.Rollback(ctx)
			committed <- false
			return
		}
		committed <- tx.Commit(ctx) == nil
	}()
	time.Sleep(300 * time.Millisecond)
	begin := time.Now()
	rep, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	require.Less(t, time.Since(begin), 10*time.Second, "no deadlock, within the bound")
	select {
	case c := <-committed:
		require.False(t, c, "the apply must not commit after the freeze")
	case <-time.After(10 * time.Second):
		t.Fatal("the apply was neither cancelled nor finished")
	}
	require.Equal(t, StatusFrozen, mustGet(t, f.store, p.ID).Status)
	require.Empty(t, rep.ControlError)
	require.GreaterOrEqual(t, rep.Databases[0].StatementsCancelled, 1)
	// No apply can start after the freeze: the hold finds no active row.
	rows, err := f.super.Query(ctx, holdSQL, p.ID)
	require.NoError(t, err)
	require.False(t, rows.Next())
	rows.Close()
}

// A hold nobody can cancel (an idle transaction with no in-flight PID)
// exhausts the attempts; containment in the databases still happens and
// the report names the control-database failure.
func TestKill_UncancellableHoldStillContains(t *testing.T) {
	f := newKillFixture(t)
	f.cfg.LockTimeout = 300 * time.Millisecond
	f.rebuild(t)
	p, password := f.ensured(t)
	ctx := context.Background()
	tx := hold(t, f, p)
	stmt := f.sleepAs(t, f.super, f.dsn, p.BrokerRole(), password, 30)
	begin := time.Now()
	rep, err := f.sw.Kill(ctx, killPrincipal(p))
	require.NoError(t, err)
	require.Less(t, time.Since(begin), 10*time.Second)
	require.True(t, terminatedOrCancelled(waitDone(t, stmt, 10*time.Second)))
	login, _ := f.attrs(t, p.BrokerRole())
	require.False(t, login)
	require.Contains(t, rep.ControlError, "lock")
	require.True(t, rep.Verified, "no agent backend remains, logins are blocked")
	require.Equal(t, StatusActive, mustGet(t, f.store, p.ID).Status,
		"the principal row could not be locked, so it is not frozen yet")
	require.NoError(t, tx.Rollback(ctx))
	entries, err := f.fallback.Entries()
	require.NoError(t, err)
	require.NotEmpty(t, entries, "the unrecorded freeze is queued locally")
}
