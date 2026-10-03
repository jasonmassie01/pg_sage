package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
)

var keysetBase = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// seedFindings inserts n findings named prefix_<i>; step spaces last_seen
// (0 ties every row on it).
func seedFindings(t *testing.T, pool *pgxpool.Pool, prefix, status, severity string,
	n int, step time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.findings
		(category, severity, title, detail, object_identifier, status, last_seen, created_at)
		SELECT 'keyset_probe', $1, $2 || '_' || g, '{}', 'public.' || $2 || '_' || g, $3,
		       $4::timestamptz + make_interval(secs => g * $5::float8), $4::timestamptz
		  FROM generate_series(1, $6) g`,
		severity, prefix, status, keysetBase, step.Seconds(), n)
	if err != nil {
		t.Fatalf("seed findings: %v", err)
	}
}

type listResponse struct {
	Total       int              `json:"total"`
	TotalCapped bool             `json:"total_capped"`
	NextCursor  string           `json:"next_cursor"`
	Findings    []map[string]any `json:"findings"`
	Actions     []map[string]any `json:"actions"`
}

func getList(t *testing.T, h http.Handler, path string) (listResponse, int) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	var resp listResponse
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	return resp, w.Code
}

// walkPages follows next_cursor from the first page to the last.
func walkPages(t *testing.T, h http.Handler, base string, rowsOf func(listResponse) []map[string]any,
) ([]map[string]any, int) {
	t.Helper()
	var rows []map[string]any
	path := base
	for pages := 1; pages < 1000; pages++ {
		resp, code := getList(t, h, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, code)
		}
		rows = append(rows, rowsOf(resp)...)
		if resp.NextCursor == "" {
			return rows, pages
		}
		path = base + "&cursor=" + url.QueryEscape(resp.NextCursor)
	}
	t.Fatal("cursor walk did not terminate")
	return nil, 0
}

func findingRows(r listResponse) []map[string]any { return r.Findings }

func TestFindingsKeyset_WalksEveryRowOnceWithTiedSortKeys(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedFindings(t, pool, "tied", "open", "warning", 120, 0)
	h := findingsListHandler(phase2MgrWithPool(pool))
	first, code := getList(t, h, "/api/v1/findings?database=testdb&limit=50")
	if code != 200 || first.Total != 120 || first.TotalCapped || len(first.Findings) != 50 {
		t.Fatalf("first page: code=%d total=%d capped=%v rows=%d", code, first.Total,
			first.TotalCapped, len(first.Findings))
	}
	rows, pages := walkPages(t, h, "/api/v1/findings?database=testdb&limit=50", findingRows)
	if pages != 3 || len(rows) != 120 {
		t.Fatalf("walk = %d rows in %d pages, want 120 in 3", len(rows), pages)
	}
	seen := map[any]bool{}
	for _, r := range rows {
		if seen[r["id"]] {
			t.Fatalf("finding %v returned twice", r["id"])
		}
		seen[r["id"]] = true
	}
}

// idOf is a list row's id (a JSON number decodes as float64).
func idOf(t *testing.T, r map[string]any) int64 {
	t.Helper()
	switch v := r["id"].(type) {
	case float64:
		return int64(v)
	case string:
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			return id
		}
	}
	t.Fatalf("id %v (%T) is not an integer", r["id"], r["id"])
	return 0
}

func lastSeenOf(t *testing.T, r map[string]any) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, fmt.Sprint(r["last_seen"]))
	if err != nil {
		t.Fatalf("last_seen %v: %v", r["last_seen"], err)
	}
	return ts
}

func TestFindingsKeyset_LastSeenOrderBothDirections(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedFindings(t, pool, "seen", "open", "info", 30, time.Minute)
	h := findingsListHandler(phase2MgrWithPool(pool))
	for _, order := range []string{"desc", "asc"} {
		rows, _ := walkPages(t, h, "/api/v1/findings?database=testdb&limit=7&sort=last_seen"+
			"&order="+order, findingRows)
		if len(rows) != 30 {
			t.Fatalf("%s: %d rows, want 30", order, len(rows))
		}
		for i := 1; i < len(rows); i++ {
			prev, cur := lastSeenOf(t, rows[i-1]), lastSeenOf(t, rows[i])
			if (order == "desc" && cur.After(prev)) || (order == "asc" && cur.Before(prev)) {
				t.Fatalf("%s: row %d out of order: %v then %v", order, i, prev, cur)
			}
		}
	}
}

func TestFindingsKeyset_SeverityMostSevereFirst(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedFindings(t, pool, "crit", "open", "critical", 5, time.Second)
	seedFindings(t, pool, "warn", "open", "warning", 5, time.Second)
	seedFindings(t, pool, "info", "open", "info", 5, time.Second)
	h := findingsListHandler(phase2MgrWithPool(pool))
	rows, _ := walkPages(t, h, "/api/v1/findings?database=testdb&limit=4", findingRows)
	rank := map[any]int{"critical": 3, "warning": 2, "info": 1}
	if len(rows) != 15 {
		t.Fatalf("%d rows, want 15", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if rank[b["severity"]] > rank[a["severity"]] {
			t.Fatalf("row %d: %v after %v", i, b["severity"], a["severity"])
		}
		// Within a severity, newest first by id: last_seen is no tie-breaker
		// (no findings index may key it, so refreshes stay HOT).
		if a["severity"] == b["severity"] && idOf(t, b) > idOf(t, a) {
			t.Fatalf("row %d: newer finding after an older one of the same severity", i)
		}
	}
}

func TestFindingsKeyset_TotalIsCapped(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedFindings(t, pool, "many", "resolved", "info", maxListTotal+5, time.Second)
	h := findingsListHandler(phase2MgrWithPool(pool))
	resp, code := getList(t, h, "/api/v1/findings?database=testdb&status=resolved")
	if code != 200 || resp.Total != maxListTotal || !resp.TotalCapped ||
		len(resp.Findings) != 50 || resp.NextCursor == "" {
		t.Fatalf("capped page: code=%d total=%d capped=%v rows=%d next=%q", code,
			resp.Total, resp.TotalCapped, len(resp.Findings), resp.NextCursor)
	}
	resp, _ = getList(t, h, "/api/v1/findings?database=testdb&status=open")
	if resp.Total != 0 || resp.TotalCapped || resp.NextCursor != "" {
		t.Fatalf("empty status: total=%d capped=%v next=%q", resp.Total, resp.TotalCapped,
			resp.NextCursor)
	}
}

func TestFindingsKeyset_OffsetMatchesCursorOrderAndIsBounded(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedFindings(t, pool, "off", "open", "warning", 25, time.Second)
	h := findingsListHandler(phase2MgrWithPool(pool))
	walk, _ := walkPages(t, h, "/api/v1/findings?database=testdb&limit=10", findingRows)
	page, code := getList(t, h, "/api/v1/findings?database=testdb&limit=10&offset=10")
	if code != 200 || len(page.Findings) != 10 {
		t.Fatalf("offset page: code=%d rows=%d", code, len(page.Findings))
	}
	for i, r := range page.Findings {
		if r["id"] != walk[10+i]["id"] {
			t.Fatalf("offset row %d = %v, cursor walk row = %v", i, r["id"], walk[10+i]["id"])
		}
	}
	for _, bad := range []string{"&offset=1001", "&cursor=garbage!",
		"&sort=last_seen&cursor=" + url.QueryEscape(page.NextCursor)} {
		if _, code := getList(t, h, "/api/v1/findings?database=testdb"+bad); code != 400 {
			t.Errorf("%s: code %d, want 400", bad, code)
		}
	}
}

// Two databases (here: two instances on one database, so every row exists
// twice with the same id): the merged walk returns each (database, id)
// exactly once, in order.
func TestFindingsKeyset_FleetWalkAcrossDatabases(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedFindings(t, pool, "fleet", "open", "warning", 20, 0)
	mgr := fleet.NewManager(&config.Config{Mode: "fleet"})
	for _, name := range []string{"db_b", "db_a"} {
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
			Config: config.DatabaseConfig{Name: name},
			Status: &fleet.InstanceStatus{Connected: true}})
	}
	h := findingsListHandler(mgr)
	rows, _ := walkPages(t, h, "/api/v1/findings?database=all&limit=7", findingRows)
	if len(rows) != 40 {
		t.Fatalf("fleet walk = %d rows, want 40", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		key := fmt.Sprint(r["database_name"], "/", r["id"])
		if seen[key] {
			t.Fatalf("%s returned twice", key)
		}
		seen[key] = true
	}
	first, _ := getList(t, h, "/api/v1/findings?database=all&limit=7")
	if first.Total != 40 {
		t.Fatalf("fleet total = %d, want 40", first.Total)
	}
}

// explainPlan returns the JSON plan of sql with sequential scans and sorts
// disabled: a plan that still sorts or scans the table has no index for
// the list's filter and order.
func explainPlan(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off; SET enable_sort = off"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "RESET enable_seqscan; RESET enable_sort") }()
	var plan []byte
	if err := conn.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&plan); err != nil {
		t.Fatalf("explain: %v\n%s", err, sql)
	}
	return string(plan)
}

func TestFindingsPageSQL_UsesListIndexes(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cases := map[string]struct {
		sort, index string
		keys        []string
	}{
		"severity": {"severity", "idx_findings_list_rank", []string{"2"}},
	}
	for name, tc := range cases {
		for _, withCursor := range []bool{false, true} {
			f := fleet.FindingFilters{Status: "resolved", Sort: tc.sort, Order: "desc", Limit: 50}
			var cur *listCursor
			if withCursor {
				cur = &listCursor{Sort: tc.sort, Order: "desc", Keys: tc.keys,
					Source: "testdb", ID: 99}
			}
			sql, args := buildFindingsPageSQL(f, cur, "testdb", 51)
			plan := explainPlan(t, pool, sql, args...)
			if !strings.Contains(plan, tc.index) {
				t.Errorf("%s cursor=%v: plan does not use %s:\n%s", name, withCursor,
					tc.index, plan)
			}
			if strings.Contains(plan, `"Node Type": "Sort"`) ||
				strings.Contains(plan, `"Node Type": "Seq Scan"`) {
				t.Errorf("%s cursor=%v: plan sorts or scans findings:\n%s", name,
					withCursor, plan)
			}
		}
	}
}
