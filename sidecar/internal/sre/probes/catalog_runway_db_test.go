package probes

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// M6 runway probes against real PostgreSQL: XID consumption, per-table
// freeze maxima, the holders of the xmin horizon, logged autovacuum
// cancellations, WAL position and directory, sequences against their
// binding limit, and the regression over sampled runway series.

func burnXIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	sql := fmt.Sprintf(`DO $$ BEGIN
		PERFORM set_config('synchronous_commit', 'off', false);
		FOR i IN 1..%d LOOP PERFORM pg_current_xact_id(); COMMIT; END LOOP; END $$`, n)
	if _, err := conn.Conn().PgConn().Exec(ctx, sql).ReadAll(); err != nil {
		t.Fatalf("burn %d xids: %v", n, err)
	}
}

func catalogRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id ID,
	args Args) Result {
	t.Helper()
	return NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, id, args)
}

func TestCatalog_XIDRunwayCountsConsumption(t *testing.T) {
	pool, ctx := livePool(t)
	before, err := XIDRunwayOf(catalogRun(t, ctx, pool, XIDRunwayProbe, Args{}))
	if err != nil {
		t.Fatalf("first sample: %v", err)
	}
	burnXIDs(t, ctx, pool, 2000)
	after, err := XIDRunwayOf(catalogRun(t, ctx, pool, XIDRunwayProbe, Args{}))
	if err != nil {
		t.Fatalf("second sample: %v", err)
	}
	if after.NextXID-before.NextXID < 2000 {
		t.Fatalf("next xid moved %v, want at least 2000", after.NextXID-before.NextXID)
	}
	if !(after.ClusterXIDAge >= after.DatabaseXIDAge) || after.OldestDatabase == "" {
		t.Fatalf("cluster age %v < database age %v (oldest %q)", after.ClusterXIDAge,
			after.DatabaseXIDAge, after.OldestDatabase)
	}
	var freezeMax, mxidMax, workers int64
	var avOn string
	if err := pool.QueryRow(ctx, `SELECT current_setting('autovacuum_freeze_max_age')::int8,
		current_setting('autovacuum_multixact_freeze_max_age')::int8,
		current_setting('autovacuum_max_workers')::int8, current_setting('autovacuum')`).
		Scan(&freezeMax, &mxidMax, &workers, &avOn); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if after.FreezeMaxAge != float64(freezeMax) || after.MXIDFreezeMaxAge != float64(mxidMax) ||
		after.MaxWorkers != float64(workers) || after.AutovacuumOn != (avOn == "on") {
		t.Fatalf("settings decoded as %+v", after)
	}
	if after.Workers < 0 || after.Workers > after.MaxWorkers || math.IsNaN(after.MXIDCounter) {
		t.Fatalf("workers %v of %v, mxid counter %v", after.Workers, after.MaxWorkers,
			after.MXIDCounter)
	}
}

func TestCatalog_WraparoundTablesUseTheTableMaximum(t *testing.T) {
	pool, ctx := livePool(t)
	name := fmt.Sprintf("sre_wrap_%d", time.Now().UnixNano())
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE TABLE "+ident+" (id int) WITH "+
		"(autovacuum_freeze_max_age = 100000, autovacuum_enabled = false)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE "+ident) })
	burnXIDs(t, ctx, pool, 30000)
	ts, err := WraparoundTables(catalogRun(t, ctx, pool, WraparoundTablesProbe, Args{}))
	if err != nil || len(ts) == 0 {
		t.Fatalf("tables = %+v (%v)", ts, err)
	}
	top := ts[0]
	if top.Relation != "public."+name {
		t.Fatalf("most urgent table = %s, want public.%s (%+v)", top.Relation, name, top)
	}
	var mxidMax int64
	_ = pool.QueryRow(ctx, "SELECT current_setting('autovacuum_multixact_freeze_max_age')::int8").
		Scan(&mxidMax)
	if top.FreezeMaxAge != 100000 || top.XIDAge < 30000 || top.AutovacuumEnabled ||
		top.MXIDFreezeMaxAge != float64(mxidMax) || !(top.Fraction() >= 0.3) {
		t.Fatalf("table = %+v (fraction %v)", top, top.Fraction())
	}
	for _, other := range ts[1:] {
		if other.Fraction() > top.Fraction() {
			t.Fatalf("%s (%v) ranked below %s (%v)", other.Relation, other.Fraction(),
				top.Relation, top.Fraction())
		}
	}
}

// Every kind of horizon holder is listed with its age; the probe's own
// session never is.
func TestCatalog_XminHorizonListsHolders(t *testing.T) {
	pool, ctx := livePool(t)
	dsn := os.Getenv(testdb.EnvName)
	slot := ""
	var walLevel string
	_ = pool.QueryRow(ctx, "SHOW wal_level").Scan(&walLevel)
	if walLevel == "logical" {
		slot = fmt.Sprintf("sre_xmin_%d", time.Now().UnixNano())
		if _, err := pool.Exec(ctx, "SELECT pg_create_logical_replication_slot($1, "+
			"'pgoutput')", slot); err != nil {
			t.Fatalf("slot: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
		})
	}
	holder, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	var pid int64
	if _, err := holder.Exec(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("pid: %v", err)
	}
	gid := preparedHolder(t, ctx, pool, dsn)
	burnXIDs(t, ctx, pool, 500)
	hs, err := XminHolders(catalogRun(t, ctx, pool, XminHorizon, Args{}))
	if err != nil {
		t.Fatalf("holders: %v", err)
	}
	var session, prepared, catalog bool
	for _, h := range hs {
		switch {
		case h.Kind == HolderSession && h.PID == pid:
			session = h.State == "idle in transaction" && h.XminAge >= 500 &&
				!h.BackendStart.IsZero()
		case h.Kind == HolderPreparedXact && gid != "" && h.Name == md5Hex(t, ctx, pool, gid):
			prepared = h.XminAge >= 500
		case h.Kind == HolderSlotCatalog && h.Name == slot:
			catalog = h.XminAge >= 500
		}
	}
	if !session || (gid != "" && !prepared) || (slot != "" && !catalog) {
		t.Fatalf("session %v prepared %v slot %v in %+v", session, prepared, catalog, hs)
	}
	for i := 1; i < len(hs); i++ {
		if hs[i].XminAge > hs[i-1].XminAge {
			t.Fatalf("holders not ordered by age: %+v", hs)
		}
	}
}

// preparedHolder prepares a transaction holding an XID, when the server
// allows prepared transactions; it returns the gid ("" when it cannot).
func preparedHolder(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	dsn string) string {
	t.Helper()
	var max int
	_ = pool.QueryRow(ctx, "SELECT current_setting('max_prepared_transactions')::int").
		Scan(&max)
	if max == 0 {
		t.Log("max_prepared_transactions = 0: prepared holder not checked")
		return ""
	}
	gid := fmt.Sprintf("sre_xmin_gid_%d", time.Now().UnixNano())
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if _, err := c.Exec(ctx, "BEGIN"); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := c.Exec(ctx, "SELECT pg_current_xact_id()"); err != nil {
		t.Fatalf("xid: %v", err)
	}
	if _, err := c.Exec(ctx, "PREPARE TRANSACTION '"+gid+"'"); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "ROLLBACK PREPARED '"+gid+"'")
	})
	return gid
}

func md5Hex(t *testing.T, ctx context.Context, pool *pgxpool.Pool, s string) string {
	t.Helper()
	var h string
	if err := pool.QueryRow(ctx, "SELECT md5($1)", s).Scan(&h); err != nil {
		t.Fatalf("md5: %v", err)
	}
	return h
}

func TestCatalog_AutovacuumCancellationsReadLoggedIncidents(t *testing.T) {
	pool, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	tag := fmt.Sprintf("sre_cancel_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, signal_ids, source, database_name, last_detected_at)
		VALUES ('warning', $1, '{log_autovacuum_cancel}', 'log_deterministic', $1, now()),
		       ('warning', $1, '{log_autovacuum_cancel}', 'log_deterministic', $1,
		        now() - interval '3 hours'),
		       ('warning', $1, '{lock_contention}', 'deterministic', $1, now())`,
		tag); err != nil {
		t.Fatalf("insert incidents: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE root_cause = $1", tag)
	})
	n, err := AutovacuumCancellationCount(catalogRun(t, ctx, pool, AutovacuumCancellations,
		Args{Window: time.Hour}))
	if err != nil || n != 1 {
		t.Fatalf("cancellations in the last hour = %v (%v), want 1", n, err)
	}
	fresh, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "sre_cancel_fresh"))
	if err != nil {
		t.Fatalf("connect fresh: %v", err)
	}
	t.Cleanup(fresh.Close)
	if res := catalogRun(t, ctx, fresh, AutovacuumCancellations, Args{}); res.Status !=
		StatusUnsupported {
		t.Fatalf("without the sage schema = %+v, want unsupported", res)
	}
}

func TestCatalog_WALRunwayReadsPositionAndSettings(t *testing.T) {
	pool, ctx := livePool(t)
	before, err := WALRunwayOf(catalogRun(t, ctx, pool, WALRunwayProbe, Args{}))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := pool.Exec(ctx, "SELECT pg_logical_emit_message(true, 'sre', "+
		"repeat('x', 200000))"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	after, err := WALRunwayOf(catalogRun(t, ctx, pool, WALRunwayProbe, Args{}))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if after.PositionBytes-before.PositionBytes < 200000 {
		t.Fatalf("WAL position moved %v bytes, want at least 200000",
			after.PositionBytes-before.PositionBytes)
	}
	var maxWAL, slotKeep, segment, dbSize int64
	if err := pool.QueryRow(ctx, `SELECT pg_size_bytes(current_setting('max_wal_size')),
		pg_size_bytes(current_setting('max_slot_wal_keep_size')),
		pg_size_bytes(current_setting('wal_segment_size')),
		pg_database_size(current_database())`).Scan(&maxWAL, &slotKeep, &segment,
		&dbSize); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if after.MaxWALSize != float64(maxWAL) || after.MaxSlotWALKeepSize != float64(slotKeep) ||
		after.SegmentSize != float64(segment) || after.DatabaseBytes < float64(dbSize) ||
		after.UnreadableDatabases != 0 || after.InRecovery {
		t.Fatalf("wal runway = %+v (max_wal %d, slot keep %d, db %d)", after, maxWAL,
			slotKeep, dbSize)
	}
}

// pg_ls_waldir needs pg_monitor: a plain role gets no_privilege, never an
// empty (healthy) directory.
func TestCatalog_WALDirectoryIsPrivilegedAndTyped(t *testing.T) {
	pool, ctx := livePool(t)
	d, err := WALDirectoryOf(catalogRun(t, ctx, pool, WALDirectoryProbe, Args{}))
	if err != nil || !(d.Bytes > 0) || !(d.Files >= 1) || d.ReadyFiles < 0 {
		t.Fatalf("wal directory = %+v (%v)", d, err)
	}
	restricted := restrictedPool(t, ctx, pool, "sre_waldir_np")
	res := catalogRun(t, ctx, restricted, WALDirectoryProbe, Args{})
	if res.Status != StatusNoPrivilege || len(res.Rows) != 0 {
		t.Fatalf("restricted wal directory = %+v, want no_privilege", res)
	}
}

func restrictedPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	prefix string) *pgxpool.Pool {
	t.Helper()
	role := fmt.Sprintf("%s_%d", prefix, os.Getpid())
	ident := pgx.Identifier{role}.Sanitize()
	if _, err := pool.Exec(ctx, "DROP ROLE IF EXISTS "+ident+
		"; CREATE ROLE "+ident+" LOGIN PASSWORD 'sre-probe-test'"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+ident)
	})
	u, _ := url.Parse(os.Getenv(testdb.EnvName))
	u.User = url.UserPassword(role, "sre-probe-test")
	r, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatalf("connect restricted: %v", err)
	}
	t.Cleanup(r.Close)
	return r
}

func TestCatalog_SequenceRunwayFindsTheBindingLimit(t *testing.T) {
	pool, ctx := livePool(t)
	sch := fmt.Sprintf("sre_seq_%d", time.Now().UnixNano())
	q := pgx.Identifier{sch}.Sanitize()
	ddl := strings.ReplaceAll(`CREATE SCHEMA S;
		CREATE SEQUENCE S.int_seq AS integer;
		CREATE TABLE S.a (id int DEFAULT nextval('S.int_seq'));
		ALTER SEQUENCE S.int_seq OWNED BY S.a.id;
		SELECT setval('S.int_seq', 2147483647 - 1000);
		CREATE SEQUENCE S.big_seq;
		CREATE TABLE S.b (id int DEFAULT nextval('S.big_seq'));
		ALTER SEQUENCE S.big_seq OWNED BY S.b.id;
		SELECT setval('S.big_seq', 2147483647 - 2000);
		CREATE SEQUENCE S.capped_seq MAXVALUE 1000000;
		SELECT setval('S.capped_seq', 900000);
		CREATE SEQUENCE S.cycle_seq AS smallint CYCLE;
		SELECT setval('S.cycle_seq', 32000);
		CREATE SEQUENCE S.down_seq INCREMENT -1;
		SELECT nextval('S.down_seq');
		CREATE SEQUENCE S.fresh_seq;`, "S.", q+".")
	ddl = strings.ReplaceAll(ddl, "SCHEMA S;", "SCHEMA "+q+";")
	if _, err := pool.Exec(ctx, ddl); err != nil {
		t.Fatalf("sequences: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+q+" CASCADE")
	})
	ss, err := Sequences(catalogRun(t, ctx, pool, SequenceRunwayProbe, Args{}))
	if err != nil {
		t.Fatalf("sequences: %v", err)
	}
	got := map[string]SequenceRunway{}
	for _, s := range ss {
		if strings.HasPrefix(s.Sequence, sch+".") {
			got[strings.TrimPrefix(s.Sequence, sch+".")] = s
		}
	}
	checkSequence(t, got, "int_seq", LimitSequenceType, 2147483647, "integer")
	checkSequence(t, got, "big_seq", LimitColumnType, 2147483647, "bigint")
	checkSequence(t, got, "capped_seq", LimitExplicitMax, 1000000, "bigint")
	if c := got["cycle_seq"]; !c.Cycle || c.DataType != "smallint" {
		t.Fatalf("cycle_seq = %+v", c)
	}
	if _, ok := got["down_seq"]; ok {
		t.Fatal("a descending sequence is listed; the runway covers ascending ones")
	}
	if f, ok := got["fresh_seq"]; !ok || !math.IsNaN(f.LastValue) || !math.IsNaN(f.Fraction) {
		t.Fatalf("never-called sequence = %+v (%v), want an unknown last value", f, ok)
	}
	if got["int_seq"].OwnerColumn != sch+".a.id" {
		t.Fatalf("owner column = %q", got["int_seq"].OwnerColumn)
	}
}

func checkSequence(t *testing.T, got map[string]SequenceRunway, name, binding string,
	limit float64, typ string) {
	t.Helper()
	s, ok := got[name]
	if !ok {
		t.Fatalf("%s not listed in %v", name, got)
	}
	if s.BindingLimit() != binding || s.Limit != limit || s.DataType != typ ||
		!(s.Fraction > 0.89 && s.Fraction < 1) {
		t.Fatalf("%s = %+v (binding %s), want %s at %v", name, s, s.BindingLimit(),
			binding, limit)
	}
}

// The trend regresses only the current epoch of each series inside the
// window: a counter series by its counter, a level series by its value.
func TestCatalog_RunwayTrendsRegressesTheCurrentEpoch(t *testing.T) {
	pool, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	subject := fmt.Sprintf("sre_trend_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO sage.runway_samples
		(kind, subject, epoch, sampled_at, value, counter, limit_value) VALUES
		('sequence', $1, 'old', now() - interval '50 seconds', 900, 900, 1000),
		('sequence', $1, 'new', now() - interval '20 seconds', 1, 100, 500),
		('sequence', $1, 'new', now() - interval '10 seconds', 2, 120, 500),
		('sequence', $1, 'new', now(), 3, 140, 500),
		('database_bytes', $1, 'e', now() - interval '3 hours', 1, NULL, NULL),
		('database_bytes', $1, 'e', now() - interval '20 seconds', 10, NULL, NULL),
		('database_bytes', $1, 'e', now() - interval '10 seconds', 20, NULL, NULL),
		('database_bytes', $1, 'e', now(), 30, NULL, NULL)`, subject); err != nil {
		t.Fatalf("insert samples: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.runway_samples WHERE subject = $1", subject)
	})
	ts, err := RunwayTrends(catalogRun(t, ctx, pool, RunwayTrendsProbe,
		Args{Window: time.Minute}))
	if err != nil {
		t.Fatalf("trends: %v", err)
	}
	seq, ok := FindTrend(ts, RunwaySequence, subject)
	if !ok || seq.Samples != 3 || math.Abs(seq.RatePerS-2) > 1e-6 || seq.LastValue != 3 ||
		seq.Limit != 500 || math.Abs(seq.SpanS()-20) > 0.5 {
		t.Fatalf("counter series = %+v (%v)", seq, ok)
	}
	db, ok := FindTrend(ts, RunwayDatabaseBytes, subject)
	if !ok || db.Samples != 3 || math.Abs(db.RatePerS-1) > 1e-6 || !math.IsNaN(db.Limit) ||
		math.Abs(db.R2-1) > 1e-9 {
		t.Fatalf("level series = %+v (%v)", db, ok)
	}
}
