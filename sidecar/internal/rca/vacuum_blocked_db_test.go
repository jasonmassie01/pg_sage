package rca

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rca-01 on a live server: a REPEATABLE READ session holding an old
// snapshot is named, a briefly idle session is not, a prepared
// transaction of another database is never blamed, and a logical slot's
// catalog_xmin (older than every session) is named only for a catalog
// table, since it does not hold back user tables.

// liveConn opens a session on dsn that lives until the test ends.
func liveConn(t *testing.T, dsn string, stmts ...string) (*pgx.Conn, int) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return conn, pid
}

func holderPool(t *testing.T, label string) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, label)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE public.acct (id int PRIMARY KEY, v int);
		INSERT INTO public.acct VALUES (1, 0)`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return pool, dsn
}

func requireSetting(t *testing.T, pool *pgxpool.Pool, sql, why string) {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), sql).Scan(&ok); err != nil {
		t.Fatalf("read setting (%s): %v", sql, err)
	}
	if !ok {
		t.Skip(why)
	}
}

// oldAndBrief opens the old REPEATABLE READ snapshot, burns an XID so it
// is strictly older, waits past a 2 s floor, then opens a session idle in
// a transaction with an XID.
func oldAndBrief(t *testing.T, pool *pgxpool.Pool, dsn string) (int, int) {
	t.Helper()
	_, old := liveConn(t, dsn, "BEGIN ISOLATION LEVEL REPEATABLE READ",
		"SELECT count(*) FROM public.acct")
	if _, err := pool.Exec(context.Background(), "SELECT pg_catalog.txid_current()"); err != nil {
		t.Fatalf("burn an xid: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	_, brief := liveConn(t, dsn, "BEGIN", "SELECT pg_catalog.txid_current()")
	return old, brief
}

func liveRules() vacuumRules {
	r := testVacuumRules()
	r.holderFloor = 2 * time.Second
	return r
}

func holderOf(snap *collector.Snapshot, kind string, pid int, name string) *probes.XminHolder {
	for i := range snap.XminHolders {
		h := &snap.XminHolders[i]
		if h.Kind == kind && h.PID == int64(pid) && h.Name == name {
			return h
		}
	}
	return nil
}

// CHECK-02: the old-snapshot pid is named and the briefly idle one is not.
func TestVacuumBlockedLive_NamesTheOldSnapshotNotTheIdleSession(t *testing.T) {
	pool, dsn := holderPool(t, "rca_holder_old")
	old, brief := oldAndBrief(t, pool, dsn)
	snap := firstSnapshot(t, pool)
	if holderOf(snap, probes.HolderSession, old, "") == nil ||
		holderOf(snap, probes.HolderSession, brief, "") == nil {
		t.Fatalf("holders %+v, want sessions %d and %d", snap.XminHolders, old, brief)
	}
	inc := newTestEngine().treeVacuumBlocked(vacuumSignal(snap.CollectedAt), snap, liveRules())
	b := linkBlocker(t, inc)
	if b == nil || b.PID != old ||
		!strings.Contains(inc.RootCause, "PID "+strconv.Itoa(old)+" ") {
		t.Fatalf("root cause %q blocker %+v, want the old snapshot %d, not %d",
			inc.RootCause, b, old, brief)
	}
	if b.XminAge <= 0 || b.XactStart.IsZero() || b.BackendStart.IsZero() {
		t.Fatalf("blocker %+v, want its xmin age, xact start and backend start", b)
	}
	assertNoSignal(t, inc) // idle for seconds, under the terminate minutes
}

// A prepared transaction in a second database is neither collected nor
// blamed for this database's vacuum.
func TestVacuumBlockedLive_OtherDatabasePreparedXactIsIgnored(t *testing.T) {
	pool, _ := holderPool(t, "rca_holder_mine")
	requireSetting(t, pool, "SELECT current_setting('max_prepared_transactions')::int > 0",
		"max_prepared_transactions = 0: PREPARE TRANSACTION is unavailable here")
	otherDSN := testdb.CreateDatabase(t, "rca_holder_other")
	conn, _ := liveConn(t, otherDSN, "BEGIN", "SELECT pg_catalog.txid_current()",
		"PREPARE TRANSACTION 'rca01_other_db'")
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(),
			"ROLLBACK PREPARED 'rca01_other_db'"); err != nil {
			t.Errorf("roll back the fixture prepared xact: %v", err)
		}
	})
	snap := firstSnapshot(t, pool)
	for _, p := range snap.PreparedXacts {
		if p.GID == "rca01_other_db" {
			t.Fatalf("collected another database's prepared xact: %+v", p)
		}
	}
	for _, h := range snap.XminHolders {
		if h.Kind == probes.HolderPreparedXact {
			t.Fatalf("holder %+v: another database's prepared xact listed", h)
		}
	}
	inc := newTestEngine().treeVacuumBlocked(vacuumSignal(snap.CollectedAt), snap, liveRules())
	if strings.Contains(inc.RecommendedSQL, "ROLLBACK PREPARED") ||
		strings.Contains(inc.RootCause, "rca01_other_db") {
		t.Fatalf("blamed another database's prepared xact: %q / %q", inc.RootCause,
			inc.RecommendedSQL)
	}
}

// A logical slot made before every session holds the oldest catalog_xmin:
// it is named for a catalog table, and the old session for a user table.
func TestVacuumBlockedLive_SlotCatalogXminOnlyForCatalogs(t *testing.T) {
	pool, dsn := holderPool(t, "rca_holder_slot")
	requireSetting(t, pool, "SELECT current_setting('wal_level') = 'logical'",
		"wal_level is not logical: logical replication slots are unavailable here")
	ctx := context.Background()
	slot := "rca01_slot_" + strconv.Itoa(int(time.Now().UnixNano()%1_000_000))
	if _, err := pool.Exec(ctx, "SELECT pg_catalog.pg_create_logical_replication_slot($1, "+
		"'test_decoding')", slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			"SELECT pg_catalog.pg_drop_replication_slot($1)", slot); err != nil {
			t.Errorf("drop slot %s: %v", slot, err)
		}
	})
	if _, err := pool.Exec(ctx, "SELECT pg_catalog.txid_current()"); err != nil {
		t.Fatalf("burn an xid past the slot's catalog_xmin: %v", err)
	}
	old, _ := oldAndBrief(t, pool, dsn)
	snap := firstSnapshot(t, pool)
	sh := holderOf(snap, probes.HolderSlotCatalog, 0, slot)
	oh := holderOf(snap, probes.HolderSession, old, "")
	if sh == nil || oh == nil || sh.XminAge <= oh.XminAge {
		t.Fatalf("holders %+v, want slot %s's catalog_xmin older than session %d",
			snap.XminHolders, slot, old)
	}
	eng := newTestEngine()
	user := eng.treeVacuumBlocked(vacuumSignal(snap.CollectedAt), snap, liveRules())
	b := linkBlocker(t, user)
	if b == nil || b.PID != old || strings.Contains(user.RootCause, slot) {
		t.Fatalf("user-table root cause %q, want session %d, not slot %s", user.RootCause,
			old, slot)
	}
	catSig := vacuumSignal(snap.CollectedAt)
	catSig.Metrics["blocked_tables"] = []string{"pg_catalog.pg_attribute"}
	cat := eng.treeVacuumBlocked(catSig, snap, liveRules())
	if b = linkBlocker(t, cat); b == nil || b.HolderName != slot || b.PID != 0 {
		t.Fatalf("catalog root cause %q blocker %+v, want slot %s", cat.RootCause, b, slot)
	}
	assertNoSignal(t, cat)
}
