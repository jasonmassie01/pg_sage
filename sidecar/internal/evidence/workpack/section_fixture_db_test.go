package workpack

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// The evid-02b fixture: the monitored database's catalog objects and
// pg_sage rows, and the window's maintenance, config and sessions
// snapshots (section_db_test.go builds packs from them).

const privacyMarker = "MARKER-7f3a-evid02b@example.com"

const catalogFixtureSQL = `
DROP TABLE IF EXISTS public.orders, public.customers CASCADE;
DROP SCHEMA IF EXISTS app CASCADE;
CREATE TABLE public.customers (id int PRIMARY KEY, email text NOT NULL);
CREATE TABLE public.orders (id int PRIMARY KEY, c int REFERENCES public.customers (id),
    status text, x int);
CREATE INDEX orders_c ON public.orders (c);
CREATE INDEX orders_c_dup ON public.orders (c);
CREATE INDEX orders_c_status ON public.orders (c, status);
CREATE INDEX orders_open ON public.orders (x) WHERE status = 'open';
CREATE SCHEMA app;
CREATE TABLE app.events (at date NOT NULL, v int) PARTITION BY RANGE (at);
CREATE TABLE app.events_p1 PARTITION OF app.events FOR VALUES FROM ('2026-01-01') TO ('2026-02-01');
CREATE TABLE app.events_p2 PARTITION OF app.events FOR VALUES FROM ('2026-02-01') TO ('2026-03-01');
CREATE TABLE app.events_p3 PARTITION OF app.events FOR VALUES FROM ('2026-03-01') TO ('2026-04-01');
INSERT INTO public.customers SELECT g, CASE WHEN g % 4 <> 0 THEN '` + privacyMarker +
	`' ELSE 'c' || g END FROM generate_series(1, 400) g;
INSERT INTO public.orders SELECT g, 1 + g % 400, 'open', g FROM generate_series(1, 400) g;
ANALYZE public.customers;
ANALYZE public.orders;
DELETE FROM sage.explain_cache WHERE queryid BETWEEN 1 AND 5;
DELETE FROM sage.facts;
DELETE FROM sage.host_usage_hourly WHERE resource = 'evid02b-resource';
`

func seedCatalog(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	ctx := context.Background()
	if _, err := p.Monitored.Exec(ctx, catalogFixtureSQL); err != nil {
		t.Fatalf("catalog fixture: %v", err)
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO sage.explain_cache (captured_at, queryid, plan_json, source) VALUES
			($1, 1, '[{"Plan": {"Node Type": "Index Scan", "Index Name": "orders_pkey",
			  "Relation Name": "orders", "Schema": "public", "Index Cond": "(id = 42)",
			  "Plan Rows": 1, "Total Cost": 8.3}}]', 'auto_explain'),
			($1, 2, '{"no": "plan"}', 'explain')`, []any{fixFrom.Add(time.Hour)}},
		{`INSERT INTO sage.facts (fact_type, subject_kind, subject, value, source, status,
			decided_by, decided_at) VALUES ('owner_brief', 'database', '*',
			'{"owner": "payments team"}', 'operator', 'confirmed', 'ops@example.com', $1),
			('append_only', 'table', 'public.orders', '{}', 'operator', 'confirmed', 'dba', $1)`,
			[]any{fixFrom}},
		{`INSERT INTO sage.host_usage_hourly (resource, hour, instance_class, vcpus,
			memory_total_bytes, allocated_storage_bytes, samples) VALUES
			('evid02b-resource', $1, 'db.r6g.large', 2, 17179869184, 107374182400, 4)`,
			[]any{fixFrom.Add(24 * time.Hour)}},
	}
	for _, s := range stmts {
		if _, err := p.Monitored.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, s.sql)
		}
	}
}

// seedMaintenance writes the window's config, index, table, system and
// workload_sessions snapshots.
func seedMaintenance(t *testing.T, db any) {
	t.Helper()
	snap := func(at time.Time, category, doc string) {
		execHist(t, db, `INSERT INTO sage.snapshots (collected_at, category, data{dbcol})
			VALUES ($1, $2, $3::jsonb{dbval})`, at, category, doc)
	}
	day := func(d int, h time.Duration) time.Time { return fixFrom.AddDate(0, 0, d).Add(h) }
	cfg := func(workMem string) string {
		return `{"pg_settings": [{"name": "work_mem", "setting": "` + workMem +
			`", "unit": "kB", "source": "configuration file"},
			{"name": "max_connections", "setting": "100", "unit": "", "source": "default"},
			{"name": "max_wal_size", "setting": "1024", "unit": "MB", "source": "default"}]}`
	}
	snap(day(0, 12*time.Hour), "config_data", cfg("4096"))
	snap(day(10, 12*time.Hour), "config_data", cfg("8192"))
	snap(day(0, 12*time.Hour), "indexes", `[
		{"schemaname": "public", "indexrelname": "orders_c", "idx_scan": 5},
		{"schemaname": "public", "indexrelname": "orders_c_dup", "idx_scan": 0}]`)
	snap(day(27, 13*time.Hour), "indexes", `[
		{"schemaname": "public", "indexrelname": "orders_c", "idx_scan": 25},
		{"schemaname": "public", "indexrelname": "orders_c_dup", "idx_scan": 0},
		{"schemaname": "public", "indexrelname": "orders_open", "idx_scan": 9}]`)
	snap(day(0, 12*time.Hour), "tables", `[{"schemaname": "public", "relname": "orders",
		"n_tup_ins": 100, "n_tup_upd": 10, "n_tup_del": 0, "n_dead_tup": 50,
		"autovacuum_count": 1, "autoanalyze_count": 0, "total_bytes": 5000}]`)
	snap(day(27, 23*time.Hour+30*time.Minute), "tables", `[{"schemaname": "public",
		"relname": "orders", "n_tup_ins": 400, "n_tup_upd": 40, "n_tup_del": 5,
		"n_dead_tup": 7, "autovacuum_count": 3, "autoanalyze_count": 1,
		"last_autovacuum": "2026-11-01T03:00:00Z", "total_bytes": 5000,
		"reloptions": ["fillfactor=90", "bogus"]},
		{"schemaname": "public", "relname": "customers", "total_bytes": 3000},
		{"schemaname": "app", "relname": "events", "total_bytes": 1000}]`)
	execHist(t, db, `UPDATE sage.snapshots s SET data = s.data || jsonb_build_object(
		'relation_stats_epoch', '2026-09-01T00:00:00Z',
		'checkpoint_stats_epoch', '2026-09-01T00:00:00Z',
		'wal_stats_epoch', '2026-09-01T00:00:00Z',
		'temp_bytes', d.n * 24000, 'checkpoints_timed', d.n * 10,
		'checkpoints_requested', d.n * 2, 'cluster_wal_bytes', d.n * 2400000,
		'client_backends', CASE WHEN d.n = 5 THEN 40 ELSE 10 END,
		'max_connections', 100, 'reserved_connections', 3)
		FROM (SELECT x.id, floor(extract(epoch FROM x.collected_at - $1::timestamptz)
		        / 86400)::int8 AS n FROM sage.snapshots x WHERE x.category = 'system') d
		WHERE {db:s} AND s.category = 'system' AND s.id = d.id`, fixFrom)
	seedSessions(t, db)
}

// seedSessions writes two Monday 09:00 workload_sessions snapshots with
// ages and a legacy one without at 10:00.
func seedSessions(t *testing.T, db any) {
	t.Helper()
	ws := func(at time.Time, ages string) {
		execHist(t, db, `INSERT INTO sage.snapshots (collected_at, category, data{dbcol})
			VALUES ($1, 'workload_sessions', $2::jsonb{dbval})`, at,
			`{"groups": [{"role": "app", "application": "web", "state": "idle",
			"sessions": 4}], "long": [], "roles": []`+ages+`}`)
	}
	ages := `, "ages": [{"state": "idle", "bucket": "ge_1h", "sessions": 4}]`
	ws(fixFrom.Add(9*time.Hour), ages)
	ws(fixFrom.AddDate(0, 0, 7).Add(9*time.Hour+time.Minute), ages)
	ws(fixFrom.Add(10*time.Hour), "")
}

// noiseMaintenance writes another database's config and sessions into the
// meta database: a reader that forgets its database id reports them.
func noiseMaintenance(t *testing.T, p *histfixture.Pair) {
	t.Helper()
	other, err := histstore.NewMeta(p.Meta, histfixture.OtherDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	execHist(t, other, `INSERT INTO sage.snapshots (collected_at, category, data{dbcol})
		VALUES ($1, 'config_data', '{"pg_settings": [{"name": "work_mem",
		"setting": "65536", "unit": "kB", "source": "noise"}]}'::jsonb{dbval}),
		($1, 'workload_sessions', '{"groups": [{"state": "idle", "sessions": 999}],
		"ages": [{"state": "idle", "bucket": "lt_1m", "sessions": 999}]}'::jsonb{dbval})`,
		fixFrom.AddDate(0, 0, 20).Add(9*time.Hour))
}
