package provideradvice

import (
	"strings"
	"testing"
	"time"
)

func insertFinding(t *testing.T, category, object, sql, status string, detail string) int64 {
	t.Helper()
	var id int64
	err := sharedDB.QueryRow(t.Context(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail, recommended_sql,
		 status)
		VALUES ($1, 'warning', 'index', $2, 'pa test', $3::jsonb, $4, $5) RETURNING id`,
		category, object, detail, sql, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id
}

func TestLoadFindings_ReadsOpenIndexFindings(t *testing.T) {
	pool, ctx := testPool(t)
	mi := insertFinding(t, "missing_index", "public.users|email",
		"CREATE INDEX CONCURRENTLY ON public.users (email);", "open", `{"pa_test":1}`)
	fk := insertFinding(t, "missing_fk_index", "public.orders(customer_id)",
		"CREATE INDEX CONCURRENTLY ON public.orders (customer_id);", "open",
		`{"pa_test":1,"constraint":"orders_customer_id_fkey"}`)
	insertFinding(t, "missing_index", "public.users|name",
		"CREATE INDEX CONCURRENTLY ON public.users (name);", "resolved", `{"pa_test":1}`)
	insertFinding(t, "table_bloat", "public.users", "", "open", `{"pa_test":1}`)
	got, err := LoadFindings(ctx, pool)
	if err != nil {
		t.Fatalf("LoadFindings: %v", err)
	}
	byID := map[int64]Finding{}
	for _, f := range got {
		byID[f.ID] = f
	}
	if len(byID) != 2 || byID[mi].Category != "missing_index" ||
		byID[mi].SQL != "CREATE INDEX CONCURRENTLY ON public.users (email);" ||
		byID[fk].Constraint != "orders_customer_id_fkey" ||
		byID[fk].Object != "public.orders(customer_id)" {
		t.Fatalf("findings = %+v", got)
	}
}

func TestDBEvaluator_IndexFacts(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, `CREATE SCHEMA pa_test;
		CREATE TABLE pa_test.users (id int PRIMARY KEY, email text, name text);
		CREATE INDEX users_name_idx ON pa_test.users (name);
		CREATE UNIQUE INDEX "Users_Email_Key" ON pa_test.users (email)`)
	ev := NewDBEvaluator(pool, nil)
	pk, err := ev.IndexFact(ctx, "pa_test.users_pkey")
	if err != nil || !pk.Exists || pk.Constraint != "users_pkey" {
		t.Fatalf("pkey = %+v, %v", pk, err)
	}
	plain, err := ev.IndexFact(ctx, "pa_test.users_name_idx")
	if err != nil || !plain.Exists || plain.Constraint != "" || plain.Scans != 0 {
		t.Fatalf("plain = %+v, %v", plain, err)
	}
	quoted, err := ev.IndexFact(ctx, `pa_test."Users_Email_Key"`)
	if err != nil || !quoted.Exists {
		t.Fatalf("quoted = %+v, %v", quoted, err)
	}
	gone, err := ev.IndexFact(ctx, "pa_test.no_such_idx")
	if err != nil || gone.Exists {
		t.Fatalf("missing = %+v, %v", gone, err)
	}
	for _, bad := range []string{"", "users_name_idx", "a.b.c", "pa_test.x; DROP TABLE y"} {
		if _, err := ev.IndexFact(ctx, bad); err == nil {
			t.Fatalf("%q: accepted", bad)
		}
	}
}

func TestDBEvaluator_IndexFactCountsScans(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, `CREATE SCHEMA pa_test;
		CREATE TABLE pa_test.t AS SELECT g AS a FROM generate_series(1, 5000) g;
		CREATE INDEX t_a_idx ON pa_test.t (a); ANALYZE pa_test.t`)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SET enable_seqscan = off",
		"SELECT count(*) FROM pa_test.t WHERE a = 7"} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	_, _ = conn.Exec(ctx, `DO $f$ BEGIN PERFORM pg_stat_force_next_flush();
		EXCEPTION WHEN undefined_function THEN NULL; END $f$`) // PG15+; PG14 sends at commit
	conn.Release()
	ev := NewDBEvaluator(pool, nil)
	var f IndexFact
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if f, err = ev.IndexFact(ctx, "pa_test.t_a_idx"); err != nil || f.Scans > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || f.Scans < 1 {
		t.Fatalf("fact = %+v, %v; the index was scanned", f, err)
	}
}

func TestDBEvaluator_WhatIfFailsClosed(t *testing.T) {
	pool, ctx := testPool(t)
	mustExec(t, pool, `CREATE SCHEMA pa_test;
		CREATE TABLE pa_test.users (id int PRIMARY KEY, email text);
		CREATE INDEX users_email_inc ON pa_test.users (email) INCLUDE (id)`)
	ev := NewDBEvaluator(pool, nil)
	act, reason := ev.WhatIf(ctx, "CREATE INDEX ON pa_test.users (email)")
	if act || !strings.HasPrefix(reason, "would not act: ") ||
		!strings.Contains(reason, "users_email_inc") {
		t.Fatalf("covered index: act %v, %q", act, reason)
	}
	act, reason = ev.WhatIf(ctx, "CREATE INDEX ON pa_test.users (id, email)")
	if act || !strings.HasPrefix(reason, "would not act: ") {
		t.Fatalf("uncovered index: act %v, %q", act, reason)
	}
	act, reason = ev.WhatIf(ctx, "CREATE INDEX ON pa_test.missing (a)")
	if act || !strings.HasPrefix(reason, "would not act: ") {
		t.Fatalf("missing table: act %v, %q", act, reason)
	}
}
