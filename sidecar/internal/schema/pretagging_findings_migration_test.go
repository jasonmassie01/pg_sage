package schema

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// footprint-06: the runtime stopped treating every untagged statistics-view
// read as pg_sage's own. Findings captured before pg_sage tagged its
// statements carry such a read and no tag, so a one-time migration resolves
// the open ones created before the change; a tagged, a user or a later
// (third-party monitor) finding is untouched, and a rerun changes nothing.
// No concurrent access tests: bootstrap runs under its advisory lock.

const preTaggingProbe = "footprint06_probe"

type preTaggingRow struct {
	key, status, created, detail string
}

var preTaggingRows = []preTaggingRow{
	{"untagged_pgss", "open", "2026-01-01",
		`{"query": "SELECT queryid, calls FROM pg_stat_statements WHERE dbid = $1"}`},
	{"untagged_activity_text", "open", "2025-06-01",
		`{"query_text": "select state, count(*) from\n pg_stat_activity group by 1"}`},
	{"tagged_pgss", "open", "2026-01-01",
		`{"query": "/* pg_sage */ SELECT queryid FROM pg_stat_statements"}`},
	{"user_query", "open", "2026-01-01", `{"query": "SELECT * FROM orders WHERE id = $1"}`},
	{"info_view", "open", "2026-01-01", `{"query": "SELECT * FROM pg_stat_statements_info"}`},
	{"after_change", "open", "2026-12-01",
		`{"query": "SELECT queryid, calls FROM pg_stat_statements"}`},
	{"already_resolved", "resolved", "2026-01-01",
		`{"query": "SELECT * FROM pg_stat_user_tables"}`},
	{"array_detail", "open", "2026-01-01", `["SELECT * FROM pg_stat_statements"]`},
}

func seedPreTaggingFindings(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	cleanPreTaggingFindings(t, ctx, pool)
	t.Cleanup(func() { cleanPreTaggingFindings(t, context.Background(), pool) })
	for _, r := range preTaggingRows {
		_, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity,
			object_type, object_identifier, title, detail, status, created_at, resolved_at)
			VALUES ($1, 'warning', 'query', $2, 'Slow query', $3::jsonb, $4,
			        $5::timestamptz,
			        CASE WHEN $4 = 'resolved' THEN TIMESTAMPTZ '2026-02-01Z' END)`,
			preTaggingProbe, r.key, r.detail, r.status, r.created)
		if err != nil {
			t.Fatalf("seed %s: %v", r.key, err)
		}
	}
}

func cleanPreTaggingFindings(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `DELETE FROM sage.findings WHERE category = $1`,
		preTaggingProbe); err != nil {
		t.Fatalf("clean probe findings: %v", err)
	}
}

type preTaggingState struct {
	status, reason string
	resolvedAt     *time.Time
}

func readPreTaggingFindings(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool) map[string]preTaggingState {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT object_identifier, status,
		COALESCE(CASE WHEN jsonb_typeof(detail) = 'object'
		              THEN detail->>'resolved_reason' END, ''), resolved_at
		FROM sage.findings WHERE category = $1`, preTaggingProbe)
	if err != nil {
		t.Fatalf("read probe findings: %v", err)
	}
	defer rows.Close()
	out := map[string]preTaggingState{}
	for rows.Next() {
		var key string
		var s preTaggingState
		if err := rows.Scan(&key, &s.status, &s.reason, &s.resolvedAt); err != nil {
			t.Fatal(err)
		}
		out[key] = s
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPreTaggingSelfFindingsMigration_ResolvesOnce(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	seedPreTaggingFindings(t, ctx, pool)

	bootstrapWithRetry(t, ctx, pool)
	first := readPreTaggingFindings(t, ctx, pool)
	resolved := map[string]bool{"untagged_pgss": true, "untagged_activity_text": true}
	for _, r := range preTaggingRows {
		got := first[r.key]
		switch {
		case resolved[r.key]:
			if got.status != "resolved" || got.resolvedAt == nil ||
				got.reason != "pre-tagging pg_sage statement" {
				t.Errorf("%s = %+v, want resolved as a pre-tagging pg_sage statement",
					r.key, got)
			}
		case got.status != r.status || got.reason != "":
			t.Errorf("%s = %+v, want it untouched (%s, no reason)", r.key, got, r.status)
		}
	}
	if at := first["already_resolved"].resolvedAt; at == nil ||
		!at.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("an already resolved finding was re-resolved: %v", at)
	}

	bootstrapWithRetry(t, ctx, pool)
	second := readPreTaggingFindings(t, ctx, pool)
	for key, s := range first {
		again := second[key]
		if again.status != s.status || !sameTime(again.resolvedAt, s.resolvedAt) {
			t.Errorf("rerun changed %s: %+v -> %+v", key, s, again)
		}
	}
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// The pattern the migration keeps is the retired runtime signature, on
// PostgreSQL's regex engine.
func TestPreTaggingCatalogPatternOnRealPostgres(t *testing.T) {
	pool, ctx := requireDB(t)
	cases := map[string]bool{
		"SELECT COALESCE(queryid, $1), query FROM pg_stat_statements WHERE dbid = $2": true,
		"SELECT schemaname, relname FROM pg_stat_user_tables WHERE n_dead_tup > 0":    true,
		"SELECT indexrelname FROM pg_stat_user_indexes":                               true,
		"SELECT state, count(*) FROM\tpg_stat_activity GROUP BY state":                true,
		"select * from pg_stat_replication":                                           true,
		"SELECT * FROM pg_stat_database":                                              true,
		"SELECT * FROM pg_stat_bgwriter":                                              true,
		"SELECT * FROM pg_stat_checkpointer":                                          true,
		"SELECT * FROM pg_stat_statements_info":                                       false,
		"SELECT * FROM public.orders":                                                 false,
		"SELECT * FROM pg_catalog.pg_stat_activity":                                   false,
		"SELECT xfrom pg_stat_activity":                                               false,
		"":                                                                            false,
	}
	for q, want := range cases {
		var got bool
		if err := pool.QueryRow(ctx, `SELECT $1::text ~* $2`, q,
			preTaggingCatalogPattern).Scan(&got); err != nil {
			t.Fatalf("evaluate pattern on %q: %v", q, err)
		}
		if got != want {
			t.Errorf("pattern on %q = %v, want %v", q, got, want)
		}
	}
}
