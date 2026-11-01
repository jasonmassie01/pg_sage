package lakebaselive

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

// TestLakebaseFindingScenarios exercises real catalog/statistics inputs through
// the production collector and analyzer. It never inserts synthetic findings,
// runs the global SRE poller, or executes a generated recommendation.
func TestLakebaseFindingScenarios(t *testing.T) {
	p, ctx := livePool(t)
	if err := schema.Bootstrap(ctx, p); err != nil {
		t.Fatal(err)
	}
	ns := fmt.Sprintf("nerdlab_%x", time.Now().UnixNano())
	var oid uint32
	exec(t, p, ctx, "CREATE SCHEMA "+ns)
	if err := p.QueryRow(ctx, "SELECT oid FROM pg_namespace WHERE nspname=$1 AND nspowner=(SELECT oid FROM pg_roles WHERE rolname=current_user)", ns).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var same bool
		if err := p.QueryRow(clean, "SELECT EXISTS(SELECT FROM pg_namespace WHERE nspname=$1 AND oid=$2 AND nspowner=(SELECT oid FROM pg_roles WHERE rolname=current_user))", ns, oid).Scan(&same); err != nil || !same {
			t.Errorf("cleanup ownership guard: %v same=%v", err, same)
			return
		}
		if _, err := p.Exec(clean, "DROP SCHEMA "+ns+" CASCADE"); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	// Table names are globally unique because FK collection does not include
	// schema names; a duplicate table name elsewhere must fail closed.
	suffix := strings.TrimPrefix(ns, "nerdlab_")
	name := func(kind string) string { return ns + "." + kind + "_" + suffix }
	dup, parent, child := name("dups"), name("parent"), name("child")
	stale, bloat, scan := name("stale"), name("bloat"), name("scan")
	sequence := name("pressure")
	for _, sql := range []string{
		"CREATE TABLE " + dup + "(id int, a int)",
		"INSERT INTO " + dup + " SELECT g,g%100 FROM generate_series(1,12000)g",
		"CREATE INDEX dup_a_one ON " + dup + "(a)",
		"CREATE INDEX dup_a_two ON " + dup + "(a)",
		"CREATE TABLE " + parent + "(id int PRIMARY KEY)",
		"INSERT INTO " + parent + " SELECT generate_series(1,100)",
		"CREATE TABLE " + child + "(id int, parent_id int REFERENCES " + parent + "(id))",
		"INSERT INTO " + child + " SELECT g,1+g%100 FROM generate_series(1,12000)g",
		"CREATE TABLE " + stale + "(id int) WITH (autovacuum_enabled=false)",
		"INSERT INTO " + stale + " SELECT generate_series(1,12000)",
		"VACUUM " + stale,
		"CREATE TABLE " + bloat + "(id int,payload text) WITH (autovacuum_enabled=false)",
		"INSERT INTO " + bloat + " SELECT g,repeat('x',40) FROM generate_series(1,12000)g",
		"ANALYZE " + bloat,
		"DELETE FROM " + bloat + " WHERE id<=9000",
		"CREATE TABLE " + scan + "(id int,payload text)",
		"INSERT INTO " + scan + " SELECT g,md5(g::text)||md5((g+1)::text) FROM generate_series(1,110000)g",
		"ANALYZE " + scan,
		"CREATE SEQUENCE " + sequence + " AS integer MAXVALUE 1000 START 950",
		"SELECT nextval('" + sequence + "')",
	} {
		exec(t, p, ctx, sql)
	}
	_, err := p.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY invalid_a ON "+dup+"(a)")
	if pgerr, ok := err.(*pgconn.PgError); !ok || pgerr.Code != "23505" {
		t.Fatalf("expected owned duplicate-key build failure: %v", err)
	}
	for i := 0; i < 105; i++ {
		exec(t, p, ctx, "SELECT count(*) FROM "+scan+" WHERE id=-1")
	}
	exec(t, p, ctx, "SELECT pg_stat_force_next_flush()")

	query := "SELECT sum(length(payload)) FROM (SELECT payload FROM " + scan + " ORDER BY payload OFFSET 0) sorted"
	plan := func(mem string, run bool) []byte {
		t.Helper()
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, "SET LOCAL work_mem='"+mem+"'"); err != nil {
			t.Fatal(err)
		}
		var raw []byte
		if err = tx.QueryRow(ctx, "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+query).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if run {
			var value int64
			if err = tx.QueryRow(ctx, query).Scan(&value); err != nil || value != 7040000 {
				t.Fatalf("sort result=%d err=%v", value, err)
			}
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	high := plan("64MB", true) // establishes the exact query's zero-spill baseline
	if planUsesDisk(high) {
		t.Fatal("64MB negative-control plan still spills")
	}

	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 2
	cfg.Analyzer.IntervalSeconds = 2
	cfg.Advisor.Enabled = false
	cfg.Safety.QueryTimeoutMs = 10000
	runCtx, cancel := context.WithCancel(ctx)
	logFn := func(level, format string, args ...any) {
		if level == "ERROR" && runCtx.Err() == nil {
			t.Logf(level+" "+format, args...)
		}
	}
	c := collector.New(p, cfg, 180000, logFn)
	a := analyzer.New(p, cfg, c, nil, nil, nil, nil, logFn)
	a.WithDatabaseName("pg_sage_test")
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.Run(runCtx) }()
	go func() { defer wg.Done(); a.Run(runCtx) }()
	defer func() { cancel(); wg.Wait() }()
	deadline := time.Now().Add(60 * time.Second)
	for c.PreviousSnapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if c.PreviousSnapshot() == nil {
		t.Fatal("no production collector baseline")
	}
	low := plan("64kB", true)
	if !planUsesDisk(low) {
		t.Fatal("64kB positive-control plan did not spill")
	}
	var queryID int64
	if err = p.QueryRow(ctx, "SELECT queryid FROM pg_stat_statements WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database()) AND strpos(query,$1)>0 AND query ~ '^SELECT sum'", scan).Scan(&queryID); err != nil {
		t.Fatal(err)
	}
	want := []string{"duplicate_index", "missing_fk_index", "stale_statistics", "table_bloat", "seq_scan_heavy", "sequence_exhaustion", "invalid_index"}
	seen := map[string]bool{}
	deadline = time.Now().Add(75 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := p.Query(ctx, "SELECT DISTINCT category FROM sage.findings WHERE status='open' AND object_identifier LIKE $1", ns+".%")
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var category string
			if err = rows.Scan(&category); err != nil {
				t.Fatal(err)
			}
			seen[category] = true
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		var spilled bool
		if err = p.QueryRow(ctx, "SELECT EXISTS(SELECT FROM sage.findings WHERE category='temp_spill' AND object_identifier=$1 AND (detail->>'delta_temp_blks_written')::bigint>=128 AND COALESCE(recommended_sql,'')='')", fmt.Sprintf("queryid:%d", queryID)).Scan(&spilled); err != nil {
			t.Fatal(err)
		}
		seen["temp_spill"] = spilled
		complete := spilled
		for _, category := range want {
			complete = complete && seen[category]
		}
		if complete {
			break
		}
		time.Sleep(time.Second)
	}
	for _, category := range append(want, "temp_spill") {
		if !seen[category] {
			t.Errorf("production finding absent: %s (seen=%v)", category, seen)
		} else {
			t.Logf("production finding confirmed: %s", category)
		}
	}
	if t.Failed() {
		return
	}
	// Correct each real fixture and require the next complete analyzer window
	// to resolve findings. A zero-spill re-run is the work_mem negative control.
	plan("64MB", true)
	for _, sql := range []string{
		"DROP INDEX " + ns + ".dup_a_two",
		"DROP INDEX " + ns + ".invalid_a",
		"CREATE INDEX child_parent ON " + child + "(parent_id)",
		"ANALYZE " + dup,
		"ANALYZE " + child,
		"ANALYZE " + stale,
		"VACUUM (ANALYZE) " + bloat,
		"ALTER SEQUENCE " + sequence + " MAXVALUE 1000000",
		"DROP TABLE " + scan,
	} {
		exec(t, p, ctx, sql)
	}
	deadline = time.Now().Add(60 * time.Second)
	var open int
	for time.Now().Before(deadline) {
		if err = p.QueryRow(ctx, "SELECT count(*) FROM sage.findings WHERE status='open' AND (object_identifier LIKE $1 OR (category='temp_spill' AND object_identifier=$2)) AND category=ANY($3)", ns+".%", fmt.Sprintf("queryid:%d", queryID), append(want, "temp_spill")).Scan(&open); err != nil {
			t.Fatal(err)
		}
		if open == 0 {
			break
		}
		time.Sleep(time.Second)
	}
	if open != 0 {
		t.Errorf("%d fixture findings still open after corrections", open)
		rows, err := p.Query(ctx, "SELECT category,object_identifier FROM sage.findings WHERE status='open' AND (object_identifier LIKE $1 OR (category='temp_spill' AND object_identifier=$2)) AND category=ANY($3)", ns+".%", fmt.Sprintf("queryid:%d", queryID), append(want, "temp_spill"))
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var category, ident string
				if rows.Scan(&category, &ident) == nil {
					t.Logf("remaining open: %s %s", category, ident)
				}
			}
		}
		return
	}
	t.Logf("owned fixture schema=%s oid=%d; 8 findings detected and corrected; default rule thresholds; advisory only", ns, oid)
}

func planUsesDisk(raw []byte) bool {
	var root any
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	var walk func(any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case map[string]any:
			if space, ok := x["Sort Space Type"].(string); ok && space == "Disk" {
				return true
			}
			if batches, ok := x["Hash Batches"].(float64); ok && batches > 1 {
				return true
			}
			for _, value := range x {
				if walk(value) {
					return true
				}
			}
		case []any:
			for _, value := range x {
				if walk(value) {
					return true
				}
			}
		}
		return false
	}
	return walk(root)
}
