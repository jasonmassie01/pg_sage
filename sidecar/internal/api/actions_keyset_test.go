package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedActionLog inserts n executed actions running sqlText, spaced by step
// from start (0 ties them).
func seedActionLog(t *testing.T, pool *pgxpool.Pool, sqlText string, n int,
	start time.Time, step time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, executed_at)
		SELECT 'create_index', $1, 'success',
		       $2::timestamptz + make_interval(secs => g * $3::float8)
		  FROM generate_series(1, $4) g`, sqlText, start, step.Seconds(), n)
	if err != nil {
		t.Fatalf("seed action_log: %v", err)
	}
}

func seedActionQueue(t *testing.T, pool *pgxpool.Pool, n int, start time.Time,
	step time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO sage.action_queue
		(finding_id, proposed_sql, action_risk, status, proposed_at)
		SELECT g, 'CREATE INDEX CONCURRENTLY q' || g || ' ON t (c)', 'safe', 'pending',
		       $1::timestamptz + make_interval(secs => g * $2::float8)
		  FROM generate_series(1, $3) g`, start, step.Seconds(), n)
	if err != nil {
		t.Fatalf("seed action_queue: %v", err)
	}
}

func actionRows(r listResponse) []map[string]any { return r.Actions }

// The attempts column used to be a window over all of action_log
// (COUNT(*) OVER (PARTITION BY sql_executed)) evaluated before LIMIT; it is
// now counted for the page's rows only, with the same value.
func TestActionsKeyset_AttemptsCountedPerPageRow(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedActionLog(t, pool, "CREATE INDEX a ON t (a)", 3, keysetBase, time.Hour)
	seedActionLog(t, pool, "CREATE INDEX b ON t (b)", 1, keysetBase, time.Minute)
	h := actionsListHandler(phase2MgrWithPool(pool))
	resp, code := getList(t, h, "/api/v1/actions?database=testdb")
	if code != 200 || len(resp.Actions) != 4 || resp.Total != 4 {
		t.Fatalf("code=%d rows=%d total=%d", code, len(resp.Actions), resp.Total)
	}
	for _, a := range resp.Actions {
		want := 1.0
		if a["sql_executed"] == "CREATE INDEX a ON t (a)" {
			want = 3
		}
		if a["attempts"] != want {
			t.Errorf("%v attempts = %v, want %v", a["sql_executed"], a["attempts"], want)
		}
	}
	// Inside a time window, attempts count only the window's executions.
	from := url.QueryEscape(keysetBase.Add(90 * time.Minute).Format(time.RFC3339))
	resp, _ = getList(t, h, "/api/v1/actions?database=testdb&from="+from)
	for _, a := range resp.Actions {
		if a["sql_executed"] == "CREATE INDEX a ON t (a)" && a["attempts"] != 2.0 {
			t.Errorf("windowed attempts = %v, want 2", a["attempts"])
		}
	}
}

// Executed actions and queued proposals interleave by time; the cursor
// walk returns every ledger row once, newest first, including ties.
func TestActionsKeyset_WalkMergesExecutedAndQueued(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedActionLog(t, pool, "CREATE INDEX w ON t (w)", 25, keysetBase, 30*time.Second)
	seedActionQueue(t, pool, 15, keysetBase, time.Minute) // shares timestamps
	h := actionsListHandler(phase2MgrWithPool(pool))
	rows, pages := walkPages(t, h, "/api/v1/actions?database=testdb&limit=6", actionRows)
	if len(rows) != 40 || pages != 7 {
		t.Fatalf("walk = %d rows in %d pages, want 40 in 7", len(rows), pages)
	}
	seen := map[any]bool{}
	var prev time.Time
	for i, r := range rows {
		if seen[r["ledger_key"]] {
			t.Fatalf("%v returned twice", r["ledger_key"])
		}
		seen[r["ledger_key"]] = true
		at, err := time.Parse(time.RFC3339Nano, fmt.Sprint(r["event_at"]))
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && at.After(prev) {
			t.Fatalf("row %d (%v) newer than the row before it", i, r["ledger_key"])
		}
		prev = at
	}
}

// Offset paging used to ignore the offset for queued proposals, repeating
// them on every page.
func TestActionsKeyset_OffsetDoesNotRepeatQueuedRows(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedActionQueue(t, pool, 12, keysetBase, time.Minute)
	h := actionsListHandler(phase2MgrWithPool(pool))
	p1, _ := getList(t, h, "/api/v1/actions?database=testdb&limit=5")
	p2, _ := getList(t, h, "/api/v1/actions?database=testdb&limit=5&offset=5")
	if len(p1.Actions) != 5 || len(p2.Actions) != 5 {
		t.Fatalf("pages = %d, %d rows", len(p1.Actions), len(p2.Actions))
	}
	for _, a := range p1.Actions {
		for _, b := range p2.Actions {
			if a["ledger_key"] == b["ledger_key"] {
				t.Fatalf("%v on both pages", a["ledger_key"])
			}
		}
	}
	if _, code := getList(t, h, "/api/v1/actions?database=testdb&offset=1001"); code != 400 {
		t.Fatalf("deep offset code = %d, want 400", code)
	}
	if _, code := getList(t, h, "/api/v1/actions?database=testdb&cursor=bogus!"); code != 400 {
		t.Fatalf("bad cursor code = %d, want 400", code)
	}
}

func TestActionsKeyset_TotalIsCapped(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedActionLog(t, pool, "CREATE INDEX c ON t (c)", maxListTotal+1, keysetBase, time.Second)
	h := actionsListHandler(phase2MgrWithPool(pool))
	resp, code := getList(t, h, "/api/v1/actions?database=testdb")
	if code != 200 || resp.Total != maxListTotal || !resp.TotalCapped || len(resp.Actions) != 50 {
		t.Fatalf("code=%d total=%d capped=%v rows=%d", code, resp.Total, resp.TotalCapped,
			len(resp.Actions))
	}
	// 1001 executions of one statement: attempts are bounded too (the
	// performance gate's fixture runs one statement 20,000 times).
	if a := resp.Actions[0]; a["attempts"] != float64(maxAttempts) ||
		a["attempts_capped"] != true {
		t.Fatalf("attempts = %v (capped %v), want %d capped", a["attempts"],
			a["attempts_capped"], maxAttempts)
	}
}

func TestActionsPageSQL_UsesIndexes(t *testing.T) {
	pool, _ := phase2RequireDB(t)
	cur := &listCursor{Sort: actionsSort, Order: "desc",
		Keys: []string{keysetBase.Format(time.RFC3339Nano)}, Source: "testdb/log", ID: 7}
	for _, c := range []*listCursor{nil, cur} {
		sql, args := buildActionLogPageSQL(time.Time{}, time.Time{}, c, "testdb/log", 51)
		plan := explainPlan(t, pool, sql, args...)
		// Newest first through the existing time index; ties on executed_at
		// are ordered by id with an incremental sort, never a full one.
		for _, want := range []string{`"Index Name": "idx_action_log_time"`,
			"idx_action_log_sql_md5"} {
			if !strings.Contains(plan, want) {
				t.Errorf("cursor=%v: plan lacks %s:\n%s", c != nil, want, plan)
			}
		}
		for _, banned := range []string{`"Node Type": "WindowAgg"`, `"Node Type": "Sort"`,
			`"Node Type": "Seq Scan"`} {
			if strings.Contains(plan, banned) {
				t.Errorf("cursor=%v: plan has %s:\n%s", c != nil, banned, plan)
			}
		}
		qsql, qargs := buildQueuedLedgerPageSQL(time.Time{}, time.Time{}, c, "testdb/queue", 51)
		qplan := explainPlan(t, pool, qsql, qargs...)
		if !strings.Contains(qplan, "idx_action_queue_ledger") ||
			strings.Contains(qplan, `"Node Type": "Sort"`) {
			t.Errorf("cursor=%v: queued ledger plan:\n%s", c != nil, qplan)
		}
	}
}

// analyzedNode is the part of an EXPLAIN (ANALYZE, FORMAT JSON) node read
// here.
type analyzedNode struct {
	NodeType    string         `json:"Node Type"`
	Alias       string         `json:"Alias"`
	ActualRows  float64        `json:"Actual Rows"`
	ActualLoops float64        `json:"Actual Loops"`
	Plans       []analyzedNode `json:"Plans"`
}

func nodesWithAlias(n analyzedNode, alias string, out []analyzedNode) []analyzedNode {
	if n.Alias == alias {
		out = append(out, n)
	}
	for _, c := range n.Plans {
		out = nodesWithAlias(c, alias, out)
	}
	return out
}

// A page of 51 executions of one statement repeated 3,000 times counts its
// attempts once, reading at most maxAttempts+1 rows (counting per row read
// all 3,000 repeats 51 times: 1 s in the performance gate).
func TestActionsPageSQL_CountsAttemptsOncePerStatement(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	seedActionLog(t, pool, "SELECT 1", 3000, keysetBase, time.Second)
	// Planned with current statistics, as autovacuum keeps them.
	if _, err := pool.Exec(ctx, "ANALYZE sage.action_log"); err != nil {
		t.Fatal(err)
	}
	sql, args := buildActionLogPageSQL(time.Time{}, time.Time{}, nil, "testdb/log", 51)
	var raw []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...).
		Scan(&raw); err != nil {
		t.Fatalf("explain analyze: %v", err)
	}
	var plans []struct {
		Plan analyzedNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode plan: %v", err)
	}
	scans := nodesWithAlias(plans[0].Plan, "a2", nil)
	if len(scans) == 0 {
		t.Fatalf("no attempts scan in the plan:\n%s", raw)
	}
	for _, n := range scans {
		if n.ActualLoops != 1 || n.ActualRows > maxAttempts+1 {
			t.Fatalf("attempts %s read %v rows in %v loops, want one bounded read:\n%s",
				n.NodeType, n.ActualRows, n.ActualLoops, raw)
		}
	}
}
