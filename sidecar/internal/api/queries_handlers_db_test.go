package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The workload explorer routes (market-01) on a live server: the top
// route ranks the query store's window, the query route joins its history,
// plan and links, and the wait-load route reads the collector's samples.

const explorerQID = int64(7_500_000_001)

var explorerFrom = time.Date(2021, 5, 1, 10, 0, 0, 0, time.UTC)

func seedExplorer(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	cleanup := func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM sage.query_store WHERE queryid BETWEEN $1 AND $2`,
			explorerQID, explorerQID+1)
		_, _ = pool.Exec(bg, `DELETE FROM sage.explain_cache WHERE queryid = $1`, explorerQID)
		_, _ = pool.Exec(bg, `DELETE FROM sage.query_hints WHERE queryid = $1`, explorerQID)
		_, _ = pool.Exec(bg, `DELETE FROM sage.findings WHERE object_identifier = $1`,
			"queryid:7500000001")
		_, _ = pool.Exec(bg, `DELETE FROM sage.snapshots WHERE category = 'wait_events'
			AND collected_at BETWEEN $1 AND $2`, explorerFrom, explorerFrom.Add(time.Hour))
	}
	cleanup()
	t.Cleanup(cleanup)
	m := func(n int) time.Time { return explorerFrom.Add(time.Duration(n) * time.Minute) }
	for i, s := range []struct {
		qid   int64
		at    int
		calls int64
		total float64
		plan  any
	}{
		{explorerQID, 0, 0, 0, "p1"}, {explorerQID, 20, 100, 36_000, "p1"},
		{explorerQID, 40, 200, 72_000, "p2"},
		{explorerQID + 1, 0, 0, 0, nil}, {explorerQID + 1, 40, 10, 100, nil},
	} {
		exec(`INSERT INTO sage.query_store (queryid, captured_at, calls, total_exec_time,
			mean_exec_time, rows, plan_hash) VALUES ($1, $2, $3, $4, 0, $5, $6)`,
			s.qid, m(s.at), s.calls, s.total, int64(i), s.plan)
	}
	exec(`INSERT INTO sage.explain_cache (captured_at, queryid, plan_json, source,
		total_cost) VALUES ($1, $2, $3, 'auto_explain', 42.5)`, m(30), explorerQID,
		`[{"Plan": {"Node Type": "Seq Scan", "Total Cost": 42.5}}]`)
	exec(`INSERT INTO sage.query_hints (queryid, hint_text, symptom, status)
		VALUES ($1, 'SeqScan(t)', 'slow', 'active')`, explorerQID)
	exec(`INSERT INTO sage.findings (category, severity, object_type, object_identifier,
		title, detail) VALUES ('slow_query', 'warning', 'query', $1, 'Slow query', '{}')`,
		"queryid:7500000001")
	for _, w := range []struct {
		at  int
		doc string
	}{
		{10, `[{"wait_event_type":"CPU","wait_event":"","sessions":2},` +
			`{"wait_event_type":"Lock","wait_event":"relation","sessions":1}]`},
		{20, `[]`},
	} {
		exec(`INSERT INTO sage.snapshots (collected_at, category, data)
			VALUES ($1, 'wait_events', $2::jsonb)`, m(w.at), w.doc)
	}
}

func explorerGet(t *testing.T, pool *pgxpool.Pool, path string) (int, map[string]any) {
	t.Helper()
	return getWorkload(t, workloadMux(phase2MgrWithPool(pool)), path)
}

func windowQuery() string {
	return "since=" + explorerFrom.Format(time.RFC3339) + "&until=" +
		explorerFrom.Add(time.Hour).Format(time.RFC3339)
}

func TestQueriesTopDB_RanksTheWindowByLoad(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedExplorer(t, ctx, pool)
	code, body := explorerGet(t, pool, "/api/v1/queries/top?database=testdb&"+windowQuery())
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	queries, _ := body["queries"].([]any)
	if len(queries) != 2 {
		t.Fatalf("queries %v, want the two seeded queries", body["queries"])
	}
	first, _ := queries[0].(map[string]any)
	if first["queryid"] != "7500000001" || first["database"] != "testdb" {
		t.Fatalf("first row %v, want queryid 7500000001 of testdb as a string", first)
	}
	if first["calls"] != float64(200) || first["total_time_ms"] != float64(72_000) ||
		first["load"] != 0.02 || first["mean_time_ms"] != float64(360) {
		t.Errorf("first row %v, want 200 calls, 72000 ms, load 0.02, mean 360", first)
	}
	if body["coverage"] != "full" {
		t.Errorf("coverage %v, want full", body["coverage"])
	}
}

func TestQueriesTopDB_FleetAddsTheDatabaseColumn(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedExplorer(t, ctx, pool)
	code, body := explorerGet(t, pool, "/api/v1/queries/top?database=all&limit=1&"+
		windowQuery())
	if code != http.StatusOK || body["database"] != "all" {
		t.Fatalf("status %d body %v, want 200 for all", code, body)
	}
	queries, _ := body["queries"].([]any)
	dbs, _ := body["databases"].([]any)
	if len(queries) != 1 || len(dbs) != 1 {
		t.Fatalf("queries %v databases %v, want the top 1 of the one database", queries, dbs)
	}
	if row, _ := queries[0].(map[string]any); row["database"] != "testdb" {
		t.Fatalf("row %v, want its database named", row)
	}
}

func TestQueryDetailDB_HistoryPlanAndLinks(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedExplorer(t, ctx, pool)
	code, body := explorerGet(t, pool, "/api/v1/queries/7500000001?database=testdb&"+
		windowQuery())
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	buckets, _ := body["buckets"].([]any)
	markers, _ := body["plan_changes"].([]any)
	if len(buckets) == 0 || len(markers) != 1 {
		t.Fatalf("buckets %v markers %v, want history and one plan change", buckets, markers)
	}
	if mk, _ := markers[0].(map[string]any); mk["from"] != "p1" || mk["to"] != "p2" {
		t.Errorf("marker %v, want p1 -> p2", mk)
	}
	plan, _ := body["plan"].(map[string]any)
	if plan == nil || plan["source"] != "auto_explain" || plan["total_cost"] != 42.5 ||
		plan["plan_json"] == nil {
		t.Errorf("plan %v, want the cached auto_explain plan with its JSON", body["plan"])
	}
	cases, _ := body["cases"].([]any)
	hints, _ := body["hints"].([]any)
	if len(cases) != 1 || len(hints) != 1 {
		t.Fatalf("cases %v hints %v, want the seeded finding and hint", cases, hints)
	}
	if c, _ := cases[0].(map[string]any); c["title"] != "Slow query" || c["status"] != "open" {
		t.Errorf("case link %v, want the open slow query finding", c)
	}
	if body["coverage"] != "full" {
		t.Errorf("coverage %v, want full", body["coverage"])
	}
}

func TestWaitLoadDB_SeriesByWaitClass(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedExplorer(t, ctx, pool)
	code, body := explorerGet(t, pool, "/api/v1/wait-load?database=testdb&"+windowQuery())
	if code != http.StatusOK {
		t.Fatalf("status %d (%v), want 200", code, body)
	}
	series, _ := body["series"].([]any)
	if len(series) != 2 {
		t.Fatalf("series %v, want the 2 stored samples", body["series"])
	}
	first, _ := series[0].(map[string]any)
	byClass, _ := first["by_class"].(map[string]any)
	if byClass["CPU"] != float64(2) || byClass["Lock"] != float64(1) ||
		first["sessions"] != float64(3) {
		t.Fatalf("first sample %v, want CPU 2 Lock 1 = 3 sessions", first)
	}
	second, _ := series[1].(map[string]any)
	if second["sessions"] != float64(0) {
		t.Errorf("idle sample %v, want a known zero", second)
	}
	events, _ := body["events"].([]any)
	if len(events) != 2 || body["coverage"] != "full" {
		t.Fatalf("events %v coverage %v, want 2 events and full", events, body["coverage"])
	}
}

// Parallel requests share the pool and nothing else.
func TestQueriesTopDB_ParallelRequestsAgree(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	seedExplorer(t, ctx, pool)
	mux := workloadMux(phase2MgrWithPool(pool))
	path := "/api/v1/queries/top?database=testdb&" + windowQuery()
	var wg sync.WaitGroup
	bodies := make([]string, 12)
	for i := range bodies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, withUser(httptest.NewRequest(http.MethodGet, path, nil),
				viewerUser()))
			var b map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &b)
			if w.Code == http.StatusOK {
				q, _ := json.Marshal(b["queries"])
				bodies[i] = string(q)
			}
		}()
	}
	wg.Wait()
	for i, b := range bodies {
		if b == "" || b != bodies[0] {
			t.Fatalf("request %d answered %q, want the same 200 body as request 0 (%q)",
				i, b, bodies[0])
		}
	}
}
