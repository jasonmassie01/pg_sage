package autovacuum

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/review/gen/autovacuum"))
}

// bulkTable builds review02b.bulk (200k rows), bulk-updates it with
// autovacuum held off so its dead tuples stay, and returns the table's
// fact as the pack states it (live rows, today's dead tuples, options).
func bulkTable(t *testing.T) (*pgxpool.Pool, context.Context, workpack.AutovacuumFact) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	for _, sql := range []string{"DROP SCHEMA IF EXISTS review02b CASCADE",
		"CREATE SCHEMA review02b",
		"CREATE TABLE review02b.bulk (id int PRIMARY KEY, v int) " +
			"WITH (autovacuum_enabled = false)",
		"INSERT INTO review02b.bulk SELECT i, 0 FROM generate_series(1, 200000) i",
		"ANALYZE review02b.bulk",
		"UPDATE review02b.bulk SET v = v + 1 WHERE id % 2 = 0",
		"UPDATE review02b.bulk SET v = v + 1 WHERE id % 4 = 0",
		"ALTER TABLE review02b.bulk RESET (autovacuum_enabled)",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	var live, dead int64
	var opts []string
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := pool.QueryRow(ctx, `SELECT s.n_live_tup, s.n_dead_tup,
			COALESCE(c.reloptions, '{}') FROM pg_stat_user_tables s
			JOIN pg_class c ON c.oid = s.relid
			WHERE s.schemaname = 'review02b' AND s.relname = 'bulk'`).Scan(&live, &dead,
			&opts)
		if err != nil {
			t.Fatalf("read the table's statistics: %v", err)
		}
		if dead >= 100_000 || time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if dead < 100_000 || live < 100_000 {
		t.Fatalf("the bulk update left live %d dead %d, want 100k+ each", live, dead)
	}
	kv := map[string]string{}
	for _, o := range opts {
		k, v, _ := strings.Cut(o, "=")
		kv[k] = v
	}
	return pool, ctx, workpack.AutovacuumFact{ID: "table/av/review02b.bulk",
		Table: "review02b.bulk", Autovacuums: int64(0), Autoanalyzes: int64(1),
		LastAutovacuum: "absent", LastAutoanalyze: "absent", LiveTuples: live,
		DeadTuples: series(dead/4, dead/2, dead), Reloptions: kv, ReloptionsText: []string{}}
}

// liveSettings reads the autovacuum settings the generator judges by, from
// pg_settings as the settings section states them.
func liveSettings(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *workpack.Settings {
	t.Helper()
	s := stockSettings()
	s.Settings = nil
	rows, err := pool.Query(ctx, `SELECT name, setting, COALESCE(unit, ''), source
		FROM pg_settings WHERE name IN ('autovacuum_analyze_scale_factor',
		'autovacuum_naptime', 'autovacuum_vacuum_scale_factor',
		'autovacuum_vacuum_threshold') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var f workpack.SettingFact
		if err := rows.Scan(&f.Name, &f.Value, &f.Unit, &f.Source); err != nil {
			t.Fatal(err)
		}
		f.ID, f.Changes = "setting/"+f.Name, []workpack.SettingChange{}
		s.Settings = append(s.Settings, f)
	}
	if err := rows.Err(); err != nil || len(s.Settings) != 4 {
		t.Fatalf("read %d autovacuum settings: %v", len(s.Settings), err)
	}
	return s
}

// Integration: the vacuum history of a real bulk-updated table gives one
// set_table_autovacuum step whose SQL the executor's own validation and
// classifier accept, which PostgreSQL applies, and whose rollback restores
// the table's options exactly.
func TestGenerateDB_BulkUpdateTableRoundTrips(t *testing.T) {
	pool, ctx, fact := bulkTable(t)
	cands, _ := generate(t, packOf(t, liveSettings(t, ctx, pool), fact))
	if len(cands) != 1 || cands[0].ActionType != "set_table_autovacuum" {
		t.Fatalf("candidates %+v, want one set_table_autovacuum step", cands)
	}
	c := cands[0]
	if got, _ := executor.ClassifySQL(c.ForwardSQL); got != "set_table_autovacuum" {
		t.Fatalf("%q classifies as %q", c.ForwardSQL, got)
	}
	for _, sql := range []string{c.ForwardSQL, c.RollbackSQL} {
		if err := executor.ValidateExecutorSQL(sql); err != nil {
			t.Fatalf("the executor refuses %q: %v", sql, err)
		}
	}
	if _, err := pool.Exec(ctx, c.ForwardSQL); err != nil {
		t.Fatalf("apply %q: %v", c.ForwardSQL, err)
	}
	opts := reloptions(t, ctx, pool)
	if !strings.Contains(opts, "autovacuum_vacuum_scale_factor=") {
		t.Fatalf("after the step the options are %q", opts)
	}
	if _, err := pool.Exec(ctx, c.RollbackSQL); err != nil {
		t.Fatalf("roll back %q: %v", c.RollbackSQL, err)
	}
	if opts := reloptions(t, ctx, pool); opts != "" {
		t.Fatalf("after the rollback the options are %q, want none", opts)
	}
}

func reloptions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var opts []string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(reloptions, '{}') FROM pg_class
		WHERE oid = 'review02b.bulk'::regclass`).Scan(&opts); err != nil {
		t.Fatal(err)
	}
	return strings.Join(opts, ",")
}
