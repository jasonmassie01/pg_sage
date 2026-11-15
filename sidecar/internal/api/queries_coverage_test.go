package api

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// evid-01b: GET /api/v1/queries/coverage reports, per database, how much of
// the execution time the long per-query history can speak for, its epoch
// breaks and gaps, and the rollup's size. The DB test's answer is the
// golden the dashboard's mocked spec copies (web/e2e/mocks/queries.ts).
// No concurrent access tests: the handler reads only; the fleet read path
// is covered by the workload routes' parallel test.

const coverageGolden = "testdata/queries_coverage_golden.json"

func TestQueriesCoverage_InvalidInputIs400(t *testing.T) {
	mux := workloadMux(noPoolMgr())
	for _, q := range []string{
		"from=2021-05-02T00:00:00Z&to=2021-05-01T00:00:00Z",
		"from=2021-05-01T00:00:00Z&to=2021-05-01T00:00:00Z",
		"from=yesterday",
		"to=2021-13-01T00:00:00Z",
		"from=2020-01-01T00:00:00Z&to=2021-05-01T00:00:00Z",
	} {
		code, body := getWorkload(t, mux, "/api/v1/queries/coverage?database=alpha&"+q)
		if code != http.StatusBadRequest || body["error"] == nil {
			t.Fatalf("%s: status %d (%v), want 400 with an error", q, code, body)
		}
	}
	code, body := getWorkload(t, mux,
		"/api/v1/queries/coverage?database=alpha&from=2021-05-01T00:00:00Z"+
			"&to=2021-05-01T00:00:01Z")
	if code != http.StatusOK {
		t.Fatalf("a one-second window must be accepted: %d %v", code, body)
	}
}

func TestQueriesCoverage_UnknownDatabaseIs404(t *testing.T) {
	code, body := getWorkload(t, workloadMux(noPoolMgr()),
		"/api/v1/queries/coverage?database=gamma")
	if code != http.StatusNotFound {
		t.Fatalf("status %d (%v), want 404", code, body)
	}
}

func TestQueriesCoverage_NoPoolListsNothing(t *testing.T) {
	code, body := getWorkload(t, workloadMux(noPoolMgr()),
		"/api/v1/queries/coverage?database=all")
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	if dbs, ok := body["databases"].([]any); !ok || len(dbs) != 0 {
		t.Fatalf("databases %#v, want an empty list (no database is connected)",
			body["databases"])
	}
}

func clearCoverage(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()
	clean := func() {
		for _, table := range []string{"query_rollup_coverage", "query_rollup_daily"} {
			_, _ = pool.Exec(ctx, "DELETE FROM sage."+table+" WHERE database_id IS NULL")
		}
	}
	clean()
	t.Cleanup(func() {
		for _, table := range []string{"query_rollup_coverage", "query_rollup_daily"} {
			_, _ = pool.Exec(t.Context(), "DELETE FROM sage."+table+
				" WHERE database_id IS NULL")
		}
	})
}

func TestQueriesCoverageDB_EmptyIsUnknownNotAnError(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	clearCoverage(t, pool)
	code, body := explorerGet(t, pool, "/api/v1/queries/coverage?database=testdb")
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	dbs, _ := body["databases"].([]any)
	if len(dbs) != 1 {
		t.Fatalf("databases %v, want testdb", body["databases"])
	}
	d, _ := dbs[0].(map[string]any)
	if d["status"] != "unknown" || d["hours"] != float64(0) || d["covered_exec_share"] != nil ||
		d["database"] != "testdb" {
		t.Fatalf("entry %v, want testdb unknown with 0 hours and no share", d)
	}
}

// seedCoverageGolden writes 28 days of coverage from 2021-05-01 (a 5-hour
// gap, three hours with breaks, one hour of unknown statement time, two
// counters-only hours) and one daily row.
func seedCoverageGolden(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_coverage (hour, db_exec_ms,
		sampled_exec_ms, sampled_queryids, epoch_breaks, dealloc_delta, snapshots,
		epoch_source)
		SELECT '2021-05-01T00:00:00Z'::timestamptz + make_interval(hours => h),
			CASE WHEN h = 400 THEN NULL ELSE 100 END, 87, 20,
			CASE WHEN h IN (10, 200, 300) THEN 20 ELSE 0 END, 1, 12,
			CASE WHEN h IN (500, 501) THEN 'counters_only' ELSE 'recorded' END
		FROM generate_series(0, 671) h WHERE h NOT BETWEEN 100 AND 104`); err != nil {
		t.Fatalf("seed coverage: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_daily (queryid, day, calls,
		exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read, temp_blks_written,
		hours, epoch_breaks, plan_hashes) VALUES (1, '2021-05-01', 1, 1, 1, 1, 1, 0, 0, 1,
		0, 0)`); err != nil {
		t.Fatalf("seed daily: %v", err)
	}
}

func TestQueriesCoverageDB_MatchesTheGolden(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	clearCoverage(t, pool)
	seedCoverageGolden(t, pool)
	code, body := explorerGet(t, pool, "/api/v1/queries/coverage?database=testdb"+
		"&from=2021-05-01T00:00:00Z&to=2021-05-29T00:00:00Z")
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	raw, err := os.ReadFile(coverageGolden)
	if err != nil {
		t.Fatalf("read %s: %v", coverageGolden, err)
	}
	var want map[string]any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("%s: %v", coverageGolden, err)
	}
	dbs, _ := body["databases"].([]any)
	if len(dbs) != 1 {
		t.Fatalf("databases %v, want one entry", body["databases"])
	}
	entry, _ := dbs[0].(map[string]any)
	if b, _ := entry["rollup_bytes"].(float64); b <= 0 {
		t.Fatalf("rollup_bytes %v, want a measurement", entry["rollup_bytes"])
	}
	// The size depends on the server's page layout: the golden carries a
	// representative figure.
	entry["rollup_bytes"] = want["databases"].([]any)[0].(map[string]any)["rollup_bytes"]
	if !reflect.DeepEqual(body, want) {
		got, _ := json.MarshalIndent(body, "", "  ")
		t.Fatalf("coverage answer differs from %s:\n%s", coverageGolden, got)
	}
	if !strings.Contains(string(raw), `"status": "ok"`) {
		t.Fatal("the golden must be the ok state the dashboard mock copies")
	}
}
