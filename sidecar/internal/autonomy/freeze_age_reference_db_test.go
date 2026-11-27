package autonomy

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// No unit-only variant: age() references exist only on a real server.

// ageGateKey is the advisory lock that pauses the holder read.
const ageGateKey = 0x5a6e1

// pauseHolderRead makes the scan's holder read wait on an advisory lock
// after it measured every holder's age, and holds that lock. It returns
// the function that lets the read finish.
func pauseHolderRead(t *testing.T, pool *pgxpool.Pool) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the gate connection: %v", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", ageGateKey); err != nil {
		conn.Release()
		t.Fatalf("take the gate lock: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", ageGateKey); err != nil {
			t.Errorf("release the gate lock: %v", err)
		}
		conn.Release()
	}
	saved := xminHoldersSQL
	xminHoldersSQL = `WITH h AS MATERIALIZED (` + probes.XminHorizonSQL + `),
	w AS MATERIALIZED (SELECT count(*) AS n FROM h
		WHERE pg_catalog.pg_advisory_xact_lock_shared(` + strconv.Itoa(ageGateKey) + `)::text
			IS NOT NULL)
	SELECT h.* FROM h, w ORDER BY h.xmin_age DESC NULLS LAST`
	t.Cleanup(func() {
		release()
		xminHoldersSQL = saved
	})
	return release
}

// waitForGate waits until a session waits on the gate lock.
func waitForGate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(context.Background(), `SELECT count(*)::int FROM pg_locks
			WHERE locktype = 'advisory' AND NOT granted AND objid = $1`, ageGateKey).
			Scan(&waiting); err != nil {
			t.Fatalf("read gate waiters: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the scan never reached the paused holder read")
}

// XIDs the cluster consumes after the holders were read must not shrink a
// holder's share of the tables it pins: the holders and the horizons are
// read at one age reference, so an idle writer about as old as its table
// still makes a freeze futile. The test holds the exclusive server: on the
// shared one PostgreSQL 14 dates the table's freeze back to the oldest XID
// running in any database while a backend logs in, and the writer's 0.93
// share can then need more than the 2M XIDs a settle step may burn.
func TestFreezeScanReadsHoldersAndHorizonsAtOneAgeReference(t *testing.T) {
	pool := exclusiveAutonomyDB(t, "holder and table XID ages at one reference")
	holderSchema(t, pool)
	tables := holderTables(t, pool, "r", 1, "")
	pid := idleWriter(t, pool, tables[0])
	testdb.BurnXIDs(t, context.Background(), pool, burnForRed(t, pool))
	settleShares(t, pool, "SELECT age(backend_xid) FROM pg_stat_activity WHERE pid = $2", pid,
		map[string]shareBand{tables[0]: pinsAll})
	var tableAge int
	if err := pool.QueryRow(context.Background(), `SELECT age(relfrozenxid)::int
		FROM pg_class WHERE oid = $1::regclass`, tables[0]).Scan(&tableAge); err != nil {
		t.Fatalf("read the table's age: %v", err)
	}
	release := pauseHolderRead(t, pool)
	type result struct {
		proposals []Proposal
		err       error
	}
	done := make(chan result, 1)
	go func() {
		proposals, err := NewPostgresFreezeCustodian(pool, "testdb", holderRedBuffer).
			Scan(context.Background())
		done <- result{proposals, err}
	}()
	waitForGate(t, pool)
	// A quarter of the table's age: measured against a fresh reference the
	// writer would hold under 0.8 of it, and a freeze would look useful.
	testdb.BurnXIDs(t, context.Background(), pool, tableAge/4)
	release()
	r := <-done

	if r.err != nil {
		t.Fatalf("Scan: %v", r.err)
	}
	blockers := fixtureProposals(r.proposals, "freeze_blocker")
	if len(blockers) != 1 || blockers[0].Evidence["pid"] != pid {
		t.Fatalf("holder proposals = %#v, want one naming pid %d", blockers, pid)
	}
	if freezes := fixtureProposals(r.proposals, "freeze"); len(freezes) != 0 {
		t.Fatalf("freeze proposed past a holder measured at another reference: %#v", freezes)
	}
}
