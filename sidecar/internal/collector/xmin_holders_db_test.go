package collector

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rca-01 on a live server: the snapshot lists this database's xmin
// holders, the idle session's lock row carries state_change, and another
// database's prepared transaction is not collected.

func findHolder(hs []probes.XminHolder, kind string, pid int) *probes.XminHolder {
	for i := range hs {
		if hs[i].Kind == kind && hs[i].PID == int64(pid) {
			return &hs[i]
		}
	}
	return nil
}

func TestCollect_XminHoldersAndStateChange(t *testing.T) {
	ctx := context.Background()
	pool, dsn := graphPool(t, "holders_snap")
	holder := idleInTx(t, dsn, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ",
		"SELECT 1")
	// An XID assigned after the snapshot gives its xmin a nonzero age.
	if _, err := pool.Exec(ctx, "SELECT txid_current()"); err != nil {
		t.Fatalf("burn an xid: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if reason, ok := snap.Unavailable["xmin_holders"]; ok {
		t.Fatalf("xmin holders unavailable: %s", reason)
	}
	h := findHolder(snap.XminHolders, probes.HolderSession, holder)
	if h == nil {
		t.Fatalf("holders %+v, want session %d", snap.XminHolders, holder)
	}
	if h.XminAge <= 0 || h.XactAgeS < 1 || h.BackendStart.IsZero() ||
		h.State != "idle in transaction" {
		t.Fatalf("holder = %+v, want its xmin age, a transaction over 1 s old, its start", h)
	}
	// Server clocks only (the test host's clock may be skewed from the
	// server's): idle time = transaction age - (state_change - xact_start).
	rows := rowsOf(snap.Locks, holder)
	if len(rows) != 1 || rows[0].StateChange == nil || rows[0].XactStart == nil {
		t.Fatalf("lock rows %+v, want one row with state_change and xact_start", rows)
	}
	sc, xs := *rows[0].StateChange, *rows[0].XactStart
	idle := time.Duration(h.XactAgeS*float64(time.Second)) - sc.Sub(xs)
	if sc.Before(xs) || idle < time.Second {
		t.Fatalf("state_change %s, xact_start %s, idle %s: want idle over 1 s", sc, xs, idle)
	}
}

func TestCollect_PreparedXactsAreThisDatabasesOnly(t *testing.T) {
	ctx := context.Background()
	pool, dsn := graphPool(t, "holders_prep")
	var mpt int
	if err := pool.QueryRow(ctx,
		"SELECT current_setting('max_prepared_transactions')::int").Scan(&mpt); err != nil {
		t.Fatalf("read max_prepared_transactions: %v", err)
	}
	if mpt == 0 {
		t.Skip("max_prepared_transactions = 0: PREPARE TRANSACTION is unavailable here")
	}
	other := testdb.CreateDatabase(t, "holders_prep_other")
	for _, p := range []struct{ dsn, gid string }{{dsn, "rca01_mine"}, {other, "rca01_theirs"}} {
		conn, _ := graphConn(t, p.dsn)
		for _, s := range []string{"BEGIN", "SELECT txid_current()",
			"PREPARE TRANSACTION '" + p.gid + "'"} {
			if _, err := conn.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		gid := p.gid
		t.Cleanup(func() {
			if _, err := conn.Exec(context.Background(),
				"ROLLBACK PREPARED '"+gid+"'"); err != nil {
				t.Errorf("roll back %s: %v", gid, err)
			}
		})
	}
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(snap.PreparedXacts) != 1 || snap.PreparedXacts[0].GID != "rca01_mine" {
		t.Fatalf("prepared xacts = %+v, want only this database's rca01_mine",
			snap.PreparedXacts)
	}
	n := 0
	for _, h := range snap.XminHolders {
		if h.Kind == probes.HolderPreparedXact {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("holders %+v, want this database's one prepared xact", snap.XminHolders)
	}
}
