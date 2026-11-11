package evidence

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// audit-07: a file over the row bound took LIMIT 10001 rows with no ORDER
// BY and sorted them by their JSON text, so a cut file was an arbitrary
// subset in lexical order ("id": 10 before "id": 9), and only the single
// manifest truncated flag said that something, somewhere, was cut.

// seedActions inserts n chained actions in the window and returns the
// lowest id.
func seedActions(t *testing.T, pool *pgxpool.Pool, n int) int64 {
	t.Helper()
	mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed)
		SELECT 'vacuum', 'VACUUM t' || g FROM generate_series(1, $1) g`, n)
	var lo int64
	if err := pool.QueryRow(context.Background(),
		`SELECT min(id) FROM sage.action_log`).Scan(&lo); err != nil {
		t.Fatalf("min id: %v", err)
	}
	return lo
}

func actionIDs(t *testing.T, raw []byte) []int64 {
	t.Helper()
	var rows []struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("actions.json: %v", err)
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

// assertConsecutive checks ids run from first, one apart, n of them.
func assertConsecutive(t *testing.T, ids []int64, first int64, n int) {
	t.Helper()
	if len(ids) != n {
		t.Fatalf("got %d rows, want %d", len(ids), n)
	}
	for i, id := range ids {
		if id != first+int64(i) {
			t.Fatalf("row %d has id %d, want %d (rows not in id order)", i, id,
				first+int64(i))
		}
	}
}

// 10,001 rows: the pack holds the 10,000 lowest ids in id order, the file
// says it was cut and where, and its chain proof covers exactly those rows.
// The continuation after that id holds the one row left.
func TestWindowPackTruncatesInIDOrder(t *testing.T) {
	pool := freshDB(t, "evidence_truncate")
	lo := seedActions(t, pool, maxWindowRows+1)
	m, files := buildFrom(t, Sources{DB: pool}, lastHour())
	assertConsecutive(t, actionIDs(t, files["actions.json"]), lo, maxWindowRows)
	e := entry(t, m, "actions.json")
	if !e.Truncated || e.Rows == nil || *e.Rows != maxWindowRows || e.LastID == nil ||
		*e.LastID != lo+maxWindowRows-1 {
		t.Fatalf("actions.json entry = %+v, want truncated at id %d", e,
			lo+maxWindowRows-1)
	}
	if lo == 1 && *e.LastID != 10000 {
		t.Fatalf("last_id = %d on a fresh database, want 10000", *e.LastID)
	}
	if !m.Truncated || entry(t, m, "auth_audit.json").Truncated {
		t.Fatalf("manifest truncated %v; only actions.json is cut", m.Truncated)
	}
	if p := proofOf(t, files, "action_log"); p.Verification == nil ||
		p.Verification.Links != maxWindowRows {
		t.Fatalf("action_log proof = %+v, want the %d links taken", p, maxWindowRows)
	}

	page := lastHour()
	page.File, page.AfterID = "actions.json", *e.LastID
	m, files = buildFrom(t, Sources{DB: pool}, page)
	assertConsecutive(t, actionIDs(t, files["actions.json"]), lo+maxWindowRows, 1)
	if len(m.Files) != 2 || m.Files[0].Name != "actions.json" ||
		m.Files[1].Name != "chains.json" {
		t.Fatalf("continuation files = %+v, want actions.json and chains.json", m.Files)
	}
	e = entry(t, m, "actions.json")
	if e.Truncated || m.Truncated || *e.LastID != lo+maxWindowRows ||
		m.Scope.File != "actions.json" || m.Scope.AfterID != page.AfterID {
		t.Fatalf("continuation manifest = %+v", m)
	}
	if p := proofOf(t, files, "action_log"); p.Verification == nil ||
		p.Verification.Links != 1 {
		t.Fatalf("continuation proof = %+v, want the one link taken", p)
	}
}

// Boundary: exactly the bound is not truncated; the continuation past the
// last row is empty, with nothing to prove.
func TestWindowPackTruncationBoundary(t *testing.T) {
	pool := freshDB(t, "evidence_truncate_edge")
	lo := seedActions(t, pool, maxWindowRows+1)
	page := lastHour()
	page.File, page.AfterID = "actions.json", lo
	m, files := buildFrom(t, Sources{DB: pool}, page)
	assertConsecutive(t, actionIDs(t, files["actions.json"]), lo+1, maxWindowRows)
	if e := entry(t, m, "actions.json"); e.Truncated || m.Truncated {
		t.Fatalf("exactly %d rows reported truncated: %+v", maxWindowRows, e)
	}
	page.AfterID = lo + maxWindowRows
	m, files = buildFrom(t, Sources{DB: pool}, page)
	e := entry(t, m, "actions.json")
	if strings.TrimSpace(string(files["actions.json"])) != "[]" || e.Rows == nil ||
		*e.Rows != 0 || e.LastID != nil || e.Truncated {
		t.Fatalf("past the end: %s entry %+v", files["actions.json"], e)
	}
	if p := proofOf(t, files, "action_log"); p.Verification != nil || p.Note == "" {
		t.Fatalf("empty continuation proof = %+v, want a note and no verification", p)
	}
}

// A continuation of decisions.json holds the decisions' chain proof
// (audit-11: decisions were an unproven dump, and chains.json was empty).
func TestWindowPackContinuationProvesDecisions(t *testing.T) {
	pool := freshDB(t, "evidence_truncate_decisions")
	seedAction(t, pool)
	page := lastHour()
	page.File = "decisions.json"
	m, files := buildFrom(t, Sources{DB: pool}, page)
	p := proofOf(t, files, "decision")
	if len(m.Files) != 2 || p.Verification == nil || p.Verification.Links != 1 ||
		len(p.Verification.Problems) != 0 {
		t.Fatalf("decisions continuation = %+v chains %s", m.Files, files["chains.json"])
	}
	if e := entry(t, m, "decisions.json"); e.Rows == nil || *e.Rows != 1 {
		t.Fatalf("decisions.json entry = %+v, want 1 row", e)
	}
}

// Review of audit-07: the bound is applied in id order, not heap order.
// Rows stored highest id first (and more than the bound plus one of them)
// would give a LIMIT without ORDER BY the highest ids; the pack must still
// hold the lowest.
func TestWindowPackTruncatesByIDNotHeapOrder(t *testing.T) {
	pool := freshDB(t, "evidence_truncate_heap")
	n := maxWindowRows + 2
	mustExec(t, pool, `INSERT INTO sage.action_log (id, action_type, sql_executed)
		SELECT g, 'vacuum', 'VACUUM t' || g FROM generate_series($1::bigint, 1, -1) g`, n)
	m, files := buildFrom(t, Sources{DB: pool}, lastHour())
	assertConsecutive(t, actionIDs(t, files["actions.json"]), 1, maxWindowRows)
	if e := entry(t, m, "actions.json"); !e.Truncated || e.LastID == nil ||
		*e.LastID != maxWindowRows {
		t.Fatalf("actions.json entry = %+v, want truncated at id %d", e, maxWindowRows)
	}
}
