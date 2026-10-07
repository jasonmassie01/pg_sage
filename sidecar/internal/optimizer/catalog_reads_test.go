package optimizer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/schema"
)

// lc_collate was removed as a GUC in PostgreSQL 16, so "SHOW lc_collate"
// failed and every table read as collation "C". The database's collation
// comes from pg_database instead (Phase 0 item 7).
func TestFetchCollation_FromPgDatabase(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	var datcollate, provider string
	if err := pool.QueryRow(ctx, `SELECT datcollate,
		COALESCE(to_jsonb(d)->>'datlocprovider', 'c')
		FROM pg_database d WHERE datname = current_database()`).
		Scan(&datcollate, &provider); err != nil {
		t.Fatal(err)
	}
	got, err := fetchCollation(ctx, pool)
	if err != nil {
		t.Fatalf("fetchCollation: %v", err)
	}
	switch provider {
	case "c":
		if got != datcollate {
			t.Fatalf("collation = %q, want datcollate %q", got, datcollate)
		}
	case "b":
		if got != "C" {
			t.Fatalf("builtin provider collation = %q, want C", got)
		}
	default:
		if got == "" || got == "C" {
			t.Fatalf("ICU database reported %q", got)
		}
	}
}

func TestFetchCollation_CanceledIsAnError(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := fetchCollation(ctx, pool); err == nil || got != "" {
		t.Fatalf("canceled = %q %v, want an error and no collation", got, err)
	}
}

// The cold-start check reads at most minSnapshots rows (Phase 0 item 8):
// it used to COUNT(*) every row of sage.snapshots on every optimizer run.
func TestCheckColdStart_BoundedRead(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE sage.snapshots"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "TRUNCATE sage.snapshots") })
	cold, err := CheckColdStart(ctx, pool, 3)
	if err != nil || !cold {
		t.Fatalf("empty table: cold=%t err=%v, want cold", cold, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (category, data)
		SELECT 'system', '{}'::jsonb FROM generate_series(1, 5000)`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		min  int
		cold bool
	}{{3, false}, {5000, false}, {5001, true}, {0, false}, {-1, false}} {
		cold, err := CheckColdStart(ctx, pool, c.min)
		if err != nil || cold != c.cold {
			t.Errorf("min %d: cold=%t err=%v, want %t", c.min, cold, err, c.cold)
		}
	}
	var raw string
	// The statement as sent: its history-store markers bound (histstore).
	sql, args := histstore.Resolve(pool).Bind(coldStartSQL, 3)
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).
		Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	if rows := maxScanRows(t, raw); rows > 3 {
		t.Fatalf("cold-start check scanned %v rows, want at most 3", rows)
	}
}

func TestCheckColdStart_ErrorFailsCold(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cold, err := CheckColdStart(ctx, pool, 2)
	if err == nil || !cold {
		t.Fatalf("canceled check = cold %t err %v, want cold with error", cold, err)
	}
}

// maxScanRows returns the largest "Actual Rows" of any scan node.
func maxScanRows(t *testing.T, raw string) float64 {
	t.Helper()
	var plans []map[string]any
	if err := json.Unmarshal([]byte(raw), &plans); err != nil {
		t.Fatalf("plan json: %v", err)
	}
	var walk func(n map[string]any) float64
	walk = func(n map[string]any) float64 {
		best := 0.0
		if nt, _ := n["Node Type"].(string); len(nt) > 4 && nt[len(nt)-4:] == "Scan" {
			best, _ = n["Actual Rows"].(float64)
		}
		children, _ := n["Plans"].([]any)
		for _, c := range children {
			if v := walk(c.(map[string]any)); v > best {
				best = v
			}
		}
		return best
	}
	return walk(plans[0]["Plan"].(map[string]any))
}

// Post-test audit: with expression indexes admitted, every function in
// the keys and the predicate is checked against pg_proc; a function with
// no IMMUTABLE overload is rejected, immutable ones and keywords pass.
func TestCheckExpressionVolatility_DB(t *testing.T) {
	pool := connectTestDB(t)
	defer pool.Close()
	v := NewValidator(pool, fnTestOptimizerConfig(), noopLog)
	for ddl, want := range map[string]bool{
		"CREATE INDEX CONCURRENTLY i ON t ((lower(email)))":                              true,
		"CREATE INDEX CONCURRENTLY i ON t (date_trunc('day', created_at))":               true,
		"CREATE INDEX CONCURRENTLY i ON t (status) WHERE status IN ('a', 'b')":           true,
		"CREATE INDEX CONCURRENTLY i ON t ((created_at > now()))":                        false,
		"CREATE INDEX CONCURRENTLY i ON t (status) WHERE created_at > clock_timestamp()": false,
		"CREATE INDEX CONCURRENTLY i ON t ((lower(email)), (random() > 0.5))":            false,
		"CREATE INDEX CONCURRENTLY i ON t ((no_such_function_xyz(a)))":                   true,
	} {
		ok, reason := v.checkExpressionVolatility(context.Background(), Recommendation{DDL: ddl})
		if ok != want {
			t.Errorf("%s: ok=%t (%s), want %t", ddl, ok, reason, want)
		}
	}
}
