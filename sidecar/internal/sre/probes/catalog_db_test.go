package probes

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The R1 catalog against real conditions: a blocking chain, an
// idle-in-transaction session, connection pressure, a replication slot,
// and the WAL/checkpoint and vacuum views.

type chainFixture struct {
	table      string
	holderPID  int
	holderSeen time.Time
	alterPID   int
	release    func()
}

func dialLive(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// startChain holds ACCESS SHARE idle in transaction, queues an ALTER
// TABLE (ACCESS EXCLUSIVE) behind it and a reader behind the ALTER.
func startChain(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *chainFixture {
	t.Helper()
	dsn := os.Getenv(testdb.EnvName)
	cf := &chainFixture{table: fmt.Sprintf("sre_chain_%d", time.Now().UnixNano())}
	ident := pgx.Identifier{cf.table}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE TABLE "+ident+" (id int)"); err != nil {
		t.Fatalf("chain table: %v", err)
	}
	holder := dialLive(t, ctx, dsn)
	for _, sql := range []string{"BEGIN", "SELECT count(*) FROM " + ident} {
		if _, err := holder.Exec(ctx, sql); err != nil {
			t.Fatalf("holder %s: %v", sql, err)
		}
	}
	_ = holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&cf.holderPID)
	_ = pool.QueryRow(ctx, "SELECT backend_start FROM pg_stat_activity WHERE pid=$1",
		cf.holderPID).Scan(&cf.holderSeen)
	conns := []*pgx.Conn{holder}
	var wg sync.WaitGroup
	for i, sql := range []string{"ALTER TABLE " + ident + " ADD COLUMN v int",
		"SELECT count(*) FROM " + ident} {
		c := dialLive(t, ctx, dsn)
		if i == 0 {
			_ = c.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&cf.alterPID)
		}
		conns = append(conns, c)
		wg.Add(1)
		go func(sql string) { defer wg.Done(); _, _ = c.Exec(context.Background(), sql) }(sql)
		waitLockWaiters(t, ctx, pool, i+1)
	}
	var once sync.Once
	cf.release = func() {
		once.Do(func() {
			_, _ = holder.Exec(context.Background(), "ROLLBACK")
			wg.Wait()
			for _, c := range conns {
				_ = c.Close(context.Background())
			}
			_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+ident)
		})
	}
	t.Cleanup(cf.release)
	return cf
}

func waitLockWaiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("lock chain did not reach %d waiters", want)
}

func TestCatalog_LockProbesSeeRealChain(t *testing.T) {
	pool, ctx := livePool(t)
	cf := startChain(t, ctx, pool)
	r := NewRunner(pool, Catalog(), NewLimiter(MaxSidecarConcurrency))

	edges, err := LockEdges(r.Run(ctx, LockGraph, Args{}))
	if err != nil {
		t.Fatalf("lock_graph: %v", err)
	}
	var alterEdge, readerEdge *LockEdge
	for i := range edges {
		switch {
		case edges[i].WaiterPID == cf.alterPID && edges[i].BlockerPID == cf.holderPID:
			alterEdge = &edges[i]
		case edges[i].BlockerPID == cf.alterPID:
			readerEdge = &edges[i]
		}
	}
	if alterEdge == nil || readerEdge == nil {
		t.Fatalf("edges %+v lack holder<-alter or alter<-reader", edges)
	}
	if !alterEdge.StrongRelationWait() || alterEdge.RequestedMode != "AccessExclusiveLock" ||
		alterEdge.Relation != "public."+cf.table ||
		alterEdge.BlockerState != "idle in transaction" ||
		alterEdge.BlockerKind != "backend" ||
		!alterEdge.BlockerBackendStart.Equal(cf.holderSeen) {
		t.Fatalf("alter edge = %+v", *alterEdge)
	}
	if !readerEdge.BlockerWaiting || readerEdge.RequestedMode != "AccessShareLock" {
		t.Fatalf("reader edge = %+v (its blocker, the ALTER, is itself waiting)",
			*readerEdge)
	}

	roots, err := LockRoots(r.Run(ctx, LockChains, Args{}))
	if err != nil || len(roots) != 1 || roots[0].PID != cf.holderPID ||
		roots[0].TotalBlocked != 2 || roots[0].ChainDepth != 2 ||
		roots[0].State != "idle in transaction" {
		t.Fatalf("lock_chains roots = %+v err=%v", roots, err)
	}
	xacts, err := LongXacts(r.Run(ctx, LongTransactions, Args{}))
	if err != nil || !hasIdleXact(xacts, cf.holderPID) {
		t.Fatalf("long_transactions %+v lack idle holder %d (%v)", xacts,
			cf.holderPID, err)
	}
}

func hasIdleXact(xs []LongXact, pid int) bool {
	for _, x := range xs {
		if x.PID == pid && x.IdleInTransaction() && x.XactAgeS >= 0 {
			return true
		}
	}
	return false
}

func TestCatalog_BackendIdentityPinsTheSession(t *testing.T) {
	pool, ctx := livePool(t)
	cf := startChain(t, ctx, pool)
	r := NewRunner(pool, Catalog(), NewLimiter(1))
	same := r.Run(ctx, BackendIdentity, Args{PID: int32(cf.holderPID),
		BackendStart: cf.holderSeen})
	if same.Status != StatusOK || len(same.Rows) != 1 ||
		same.Rows[0]["state"] != "idle in transaction" {
		t.Fatalf("matching identity = %+v", same)
	}
	reused := r.Run(ctx, BackendIdentity, Args{PID: int32(cf.holderPID),
		BackendStart: cf.holderSeen.Add(-time.Hour)})
	if reused.Status != StatusEmpty {
		t.Fatalf("a different backend_start (PID reuse) must not match: %+v", reused)
	}
}

// A chain in another database on the same cluster is not this database's
// evidence (fleet isolation).
func TestCatalog_LockGraphIgnoresOtherDatabases(t *testing.T) {
	pool, ctx := livePool(t)
	otherDSN := testdb.CreateDatabase(t, "sre_probe_other")
	other, err := pgxpool.New(ctx, otherDSN)
	if err != nil {
		t.Fatalf("connect other: %v", err)
	}
	t.Cleanup(other.Close)
	t.Setenv(testdb.EnvName, otherDSN)
	startChain(t, ctx, other)
	r := NewRunner(pool, Catalog(), NewLimiter(1))
	res := r.Run(ctx, LockGraph, Args{})
	if res.Status != StatusEmpty {
		t.Fatalf("lock_graph saw another database's chain: %+v", res)
	}
	if res := NewRunner(other, Catalog(), NewLimiter(1)).Run(ctx, LockGraph,
		Args{}); res.Status != StatusOK {
		t.Fatalf("lock_graph on the chain's own database = %+v", res)
	}
}

func TestCatalog_ConnectionSaturationCountsRealSessions(t *testing.T) {
	pool, ctx := livePool(t)
	app := fmt.Sprintf("sre_conn_pressure_%d", os.Getpid())
	cfg, err := pgx.ParseConfig(os.Getenv(testdb.EnvName))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.RuntimeParams["application_name"] = app
	for i := 0; i < 12; i++ {
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		t.Cleanup(func() { _ = c.Close(context.Background()) })
	}
	res := NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, ConnectionSaturation, Args{})
	if res.Status != StatusOK {
		t.Fatalf("connection_saturation = %+v", res)
	}
	var found bool
	for _, row := range res.Rows {
		if row["application_name"] == app && row["state"] == "idle" {
			found = true
			if row["backends"] != int64(12) || row["in_current_database"] != true {
				t.Fatalf("pressure row = %#v, want 12 idle backends here", row)
			}
		}
		if mc, _ := row["max_connections"].(int64); mc <= 0 {
			t.Fatalf("max_connections unknown in %#v", row)
		}
		if tb, _ := row["total_client_backends"].(int64); tb < 12 {
			t.Fatalf("total_client_backends = %v, want >= 12", row["total_client_backends"])
		}
	}
	if !found {
		t.Fatalf("no row for %s in %v", app, res.Rows)
	}
}

func TestCatalog_IdleInTransactionSession(t *testing.T) {
	pool, ctx := livePool(t)
	conn := dialLive(t, ctx, os.Getenv(testdb.EnvName))
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var pid int
	_ = conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	for _, sql := range []string{"BEGIN", "SELECT txid_current()"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	time.Sleep(1100 * time.Millisecond)
	xacts, err := LongXacts(NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx,
		LongTransactions, Args{}))
	if err != nil {
		t.Fatalf("long_transactions: %v", err)
	}
	for _, x := range xacts {
		if x.PID == pid {
			if !x.IdleInTransaction() || x.XactAgeS < 1 || x.StateAgeS < 1 {
				t.Fatalf("idle session = %+v, want idle in transaction >= 1 s", x)
			}
			return
		}
	}
	t.Fatalf("long_transactions %+v lack idle session %d", xacts, pid)
}

func TestCatalog_ReplicationProbesOnAPrimaryWithoutReplicas(t *testing.T) {
	pool, ctx := livePool(t)
	r := NewRunner(pool, Catalog(), NewLimiter(1))
	if res := r.Run(ctx, ReplicationLag, Args{}); res.Status != StatusEmpty {
		t.Fatalf("replication_lag without replicas = %+v, want empty", res)
	}
	slot := fmt.Sprintf("sre_probe_slot_%d", os.Getpid())
	if _, err := pool.Exec(ctx,
		"SELECT pg_create_physical_replication_slot($1, true)", slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"SELECT pg_drop_replication_slot($1)", slot)
	})
	res := r.Run(ctx, ReplicationSlots, Args{})
	if res.Status != StatusOK {
		t.Fatalf("replication_slots = %+v", res)
	}
	for _, row := range res.Rows {
		if row["slot_name"] == slot {
			if row["active"] != false || row["slot_type"] != "physical" {
				t.Fatalf("slot row = %#v, want inactive physical", row)
			}
			if b, ok := row["retained_bytes"].(int64); !ok || b < 0 {
				t.Fatalf("retained_bytes = %#v", row["retained_bytes"])
			}
			return
		}
	}
	t.Fatalf("slot %s missing from %v", slot, res.Rows)
}

func TestCatalog_WALCheckpointAndVacuumViews(t *testing.T) {
	pool, ctx := livePool(t)
	r := NewRunner(pool, Catalog(), NewLimiter(1))
	wal := r.Run(ctx, WALCheckpoint, Args{})
	if wal.Status != StatusOK || len(wal.Rows) != 1 {
		t.Fatalf("wal_checkpoint = %+v", wal)
	}
	for _, col := range []string{"timed_checkpoints", "requested_checkpoints",
		"checkpoint_write_ms", "buffers_written", "wal_bytes",
		"checkpoint_timeout_s", "max_wal_size_mb"} {
		if wal.Rows[0][col] == nil {
			t.Errorf("wal_checkpoint %s is null: %#v", col, wal.Rows[0])
		}
	}
	av := r.Run(ctx, AutovacuumWraparound, Args{})
	if av.Status != StatusOK || len(av.Rows) == 0 {
		t.Fatalf("autovacuum_wraparound = %+v", av)
	}
	row := av.Rows[0]
	dbAge, _ := row["database_xid_age"].(int64)
	tblAge, _ := row["xid_age"].(int64)
	if dbAge <= 0 || tblAge > dbAge || row["freeze_max_age"] != int64(200000000) {
		t.Fatalf("wraparound row = %#v", row)
	}
	if !strings.Contains(fmt.Sprint(row["relation"]), ".") {
		t.Fatalf("relation %v is not schema-qualified", row["relation"])
	}
	if vp := r.Run(ctx, VacuumProgress, Args{}); vp.Status != StatusEmpty &&
		vp.Status != StatusOK {
		t.Fatalf("vacuum_progress = %+v", vp)
	}
	if pr := r.Run(ctx, PreparedXacts, Args{}); pr.Status != StatusEmpty {
		t.Fatalf("prepared_xacts with none prepared = %+v, want empty", pr)
	}
}
