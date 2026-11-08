package collector

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-05 on a live server: the extra categories run as named steps
// that share the cycle's retry budget, a failed one is unknown (nil field,
// Unavailable, no persisted row), and prepared transactions are persisted.

// fillAndFail returns a step reader that writes a partial reading for cat
// into the snapshot and then fails, counting its calls.
func fillAndFail(cat string, calls *atomic.Int32, err error) func(context.Context,
	*Snapshot) error {
	return func(_ context.Context, s *Snapshot) error {
		calls.Add(1)
		switch cat {
		case "io":
			s.IO = []IOStats{{BackendType: "partial"}}
		case "prepared_xacts":
			s.PreparedXacts = []PreparedTransaction{{GID: "partial"}}
		case "partitions":
			s.Partitions = []PartitionInfo{{ChildTable: "partial"}}
		case "config_data":
			s.ConfigData = &ConfigSnapshot{WALPosition: "partial"}
		}
		return err
	}
}

func extraField(s *Snapshot, cat string) any {
	switch cat {
	case "io":
		return s.IO
	case "prepared_xacts":
		return s.PreparedXacts
	case "partitions":
		return s.Partitions
	}
	return s.ConfigData
}

// CHECK-01: an injected timeout in each extra is retried once, then the
// category is unknown: its field is nil, Unavailable names the error, no
// row is persisted, and the snapshot (and every other category) is kept.
func TestCollect_FailedExtraIsUnavailableNotEmpty(t *testing.T) {
	for _, cat := range []string{"io", "prepared_xacts", "partitions", "config_data"} {
		t.Run(cat, func(t *testing.T) {
			rec := &logRecorder{}
			pool := testPool(t)
			// The io gate follows the live version (collection-02), not
			// the 170000 seed: no pg_stat_io read to fail below PG16.
			if v := serverVersion(t, pool); cat == "io" && v < 160000 {
				t.Skipf("pg_stat_io is not read below PG16 (server %d)", v)
			}
			c := New(pool, testConfig(), 170000, rec.log)
			var calls atomic.Int32
			c.overrideStep(cat, fillAndFail(cat, &calls, stmtTimeout()))
			snap, err := c.collect(context.Background())
			if err != nil {
				t.Fatalf("collect: %v", err)
			}
			if calls.Load() != stepAttempts {
				t.Errorf("%s read %d times, want %d (one retry)", cat, calls.Load(),
					stepAttempts)
			}
			if snap.Available(cat) || !strings.Contains(snap.Unavailable[cat], "57014") {
				t.Fatalf("Unavailable[%s] = %q, want the timeout", cat, snap.Unavailable[cat])
			}
			if f := extraField(snap, cat); !reflectNil(f) {
				t.Errorf("%s field = %+v after failure, want nil (unknown)", cat, f)
			}
			got := rowCategories(t, snap)
			if _, ok := got[cat]; ok {
				t.Errorf("%s: row persisted for an unknown category", cat)
			}
			if !snap.Available("tables") || got["system"] == "" {
				t.Errorf("the rest of the snapshot was lost: %v", snap.Unavailable)
			}
			if !rec.has("WARN", cat+" unavailable") {
				t.Errorf("logs = %v, want the %s unavailable warning", rec.lines, cat)
			}
		})
	}
}

func reflectNil(v any) bool {
	switch x := v.(type) {
	case []IOStats:
		return x == nil
	case []PreparedTransaction:
		return x == nil
	case []PartitionInfo:
		return x == nil
	case *ConfigSnapshot:
		return x == nil
	}
	return v == nil
}

// Boundary: the extras share the catalog steps' retry budget. With it used
// up by two timed-out catalog reads, a timed-out extra is read once.
func TestCollect_ExtrasShareTheCycleRetryBudget(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	for _, cat := range []string{"queries", "indexes"} {
		var n atomic.Int32
		c.overrideStep(cat, func(context.Context, *Snapshot) error {
			if n.Add(1) == 1 {
				return stmtTimeout()
			}
			return nil
		})
	}
	var calls atomic.Int32
	c.overrideStep("partitions", fillAndFail("partitions", &calls, stmtTimeout()))
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("partitions read %d times with the budget spent, want 1", calls.Load())
	}
	if snap.Available("partitions") || snap.Partitions != nil {
		t.Fatalf("partitions = %v / %v, want unknown", snap.Partitions, snap.Unavailable)
	}
	if !snap.Available("queries") || !snap.Available("indexes") {
		t.Fatalf("retried catalog steps lost: %v", snap.Unavailable)
	}
}

// Happy path: within the budget, a timed-out extra is read again and its
// second reading is kept.
func TestCollect_TimedOutExtraRetriedWithinBudget(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	var calls atomic.Int32
	c.overrideStep("prepared_xacts", func(_ context.Context, s *Snapshot) error {
		if calls.Add(1) == 1 {
			return stmtTimeout()
		}
		s.PreparedXacts = []PreparedTransaction{{GID: "retried"}}
		return nil
	})
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if calls.Load() != 2 || !snap.Available("prepared_xacts") ||
		len(snap.PreparedXacts) != 1 || snap.PreparedXacts[0].GID != "retried" {
		t.Fatalf("calls %d, prepared %+v / %v, want the retried read", calls.Load(),
			snap.PreparedXacts, snap.Unavailable)
	}
}

// Error propagation: a permanent failure of an extra is not retried.
func TestCollect_PermanentExtraFailureNotRetried(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	var calls atomic.Int32
	c.overrideStep("config_data", fillAndFail("config_data", &calls,
		&pgconn.PgError{Code: "42501", Message: "permission denied for table pg_class"}))
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if calls.Load() != 1 || snap.ConfigData != nil ||
		!strings.Contains(snap.Unavailable["config_data"], "permission denied") {
		t.Fatalf("calls %d, config %v, unavailable %q", calls.Load(), snap.ConfigData,
			snap.Unavailable["config_data"])
	}
}

// State: a disabled configuration snapshot is not collected, which is not
// a failure: the step does not run and config_data is not unavailable.
func TestCollect_DisabledConfigSnapshotIsNotUnavailable(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	c.WithoutConfigSnapshots()
	var cfg atomic.Int32
	c.overrideStep("config_data", fillAndFail("config_data", &cfg, stmtTimeout()))
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if cfg.Load() != 0 || !snap.Available("config_data") || snap.ConfigData != nil {
		t.Fatalf("config read %d times, config %v, unavailable %v; want not collected",
			cfg.Load(), snap.ConfigData, snap.Unavailable)
	}
}

// Nil/empty: with no prepared transaction the category is available and
// an empty, non-nil list, persisted as [].
func TestCollect_NoPreparedXactsIsAnEmptyList(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	snap, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !snap.Available("prepared_xacts") || snap.PreparedXacts == nil {
		t.Fatalf("prepared xacts = %v / %v, want an empty list", snap.PreparedXacts,
			snap.Unavailable)
	}
	if len(snap.PreparedXacts) != 0 || rowCategories(t, snap)["prepared_xacts"] != "[]" {
		t.Fatalf("no prepared xacts persisted %q, want []",
			rowCategories(t, snap)["prepared_xacts"])
	}
}

var lsnPattern = regexp.MustCompile(`^[0-9A-F]+/[0-9A-F]+$`)

// Integration (primary): the WAL position reads the current LSN.
func TestConfigSnapshot_PrimaryWALPositionIsTheCurrentLSN(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cs, err := collectConfigSnapshot(ctx, pool)
	if err != nil {
		t.Fatalf("collectConfigSnapshot: %v", err)
	}
	if !lsnPattern.MatchString(cs.WALPosition) {
		t.Fatalf("wal position = %q, want an LSN", cs.WALPosition)
	}
	var behind bool
	if err := pool.QueryRow(ctx, "SELECT $1::pg_lsn <= pg_current_wal_lsn()",
		cs.WALPosition).Scan(&behind); err != nil || !behind {
		t.Fatalf("wal position %s not at or before the current LSN (%v)", cs.WALPosition, err)
	}
	if len(cs.PGSettings) == 0 {
		t.Fatal("pg_settings empty on a live server")
	}
}

// prepareTransaction prepares a transaction named gid in pool's database
// and rolls it back when the test ends (before the database is dropped).
func prepareTransaction(t *testing.T, ctx context.Context, pool *pgxpool.Pool, gid string) {
	t.Helper()
	var maxPrepared int
	if err := pool.QueryRow(ctx, "SELECT current_setting('max_prepared_transactions')::int").
		Scan(&maxPrepared); err != nil {
		t.Fatalf("max_prepared_transactions: %v", err)
	}
	if maxPrepared == 0 {
		t.Skip("max_prepared_transactions = 0 on this server; start it with " +
			"-c max_prepared_transactions=10 to run the 2PC persistence test")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, sql := range []string{"CREATE TABLE IF NOT EXISTS public.c05_2pc (id int)",
		"BEGIN", "INSERT INTO public.c05_2pc VALUES (1)",
		"PREPARE TRANSACTION '" + gid + "'"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "ROLLBACK PREPARED '"+gid+"'"); err != nil {
			t.Errorf("rollback prepared %s: %v", gid, err)
		}
	})
}

// CHECK-02: a prepared transaction appears in the persisted prepared_xacts
// row, and the second cycle stores a delta on the first.
func TestPersist_PreparedXactsRowHoldsTheGID(t *testing.T) {
	ctx := context.Background()
	pool := bootstrappedPool(t, ctx, testdb.CreateDatabase(t, "collector_2pc"))
	const gid = "sage_c05_g1"
	prepareTransaction(t, ctx, pool, gid)
	c := New(pool, testConfig(), 170000, noopLog)
	start := time.Now().UTC().Truncate(time.Second)
	for i := 0; i < 2; i++ {
		snap, err := c.collect(ctx)
		if err != nil {
			t.Fatalf("collect %d: %v", i, err)
		}
		if !snap.Available("prepared_xacts") || len(snap.PreparedXacts) != 1 ||
			snap.PreparedXacts[0].GID != gid {
			t.Fatalf("cycle %d prepared = %+v / %v", i, snap.PreparedXacts, snap.Unavailable)
		}
		snap.CollectedAt = start.Add(time.Duration(i) * time.Minute)
		if err := c.persist(ctx, snap); err != nil {
			t.Fatalf("persist %d: %v", i, err)
		}
	}
	assertPreparedRows(t, ctx, pool, gid)
}

func assertPreparedRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, gid string) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id, base_id,
		sage.snapshot_data(data, base_id, collected_at)::text
		FROM sage.snapshots WHERE category = 'prepared_xacts' ORDER BY collected_at`)
	if err != nil {
		t.Fatalf("read prepared_xacts rows: %v", err)
	}
	type stored struct {
		id   int64
		base *int64
		doc  string
	}
	got, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (stored, error) {
		var s stored
		return s, r.Scan(&s.id, &s.base, &s.doc)
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("prepared_xacts rows = %+v (%v), want 2", got, err)
	}
	for i, s := range got {
		var xacts []PreparedTransaction
		if err := json.Unmarshal([]byte(s.doc), &xacts); err != nil {
			t.Fatalf("row %d %q: %v", i, s.doc, err)
		}
		if len(xacts) != 1 || xacts[0].GID != gid || xacts[0].Database == "" {
			t.Fatalf("row %d = %+v, want %s", i, xacts, gid)
		}
	}
	if got[0].base != nil || got[1].base == nil || *got[1].base != got[0].id {
		t.Fatalf("bases = %v, %v; want a keyframe then a delta on row %d",
			got[0].base, got[1].base, got[0].id)
	}
}
