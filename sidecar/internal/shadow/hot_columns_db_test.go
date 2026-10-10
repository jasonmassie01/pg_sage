package shadow

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sage.shadow_decision is updated by four statements. Three recur for a
// pending decision: the seen bump, the insert's ON CONFLICT bump (both
// last_seen_at, seen_count) and markApplied (applied_after,
// applied_detected_at). They must stay heap-only (HOT): none of their
// columns may be in an index key, INCLUDE list, expression or predicate.
// Only the one-time score write moves a row between the partial indexes
// (status, counted, scored_at); it carries the shadow:score tag the
// performance gate's HOT exemption is narrowed to.

var recurringColumns = []string{"applied_after", "applied_detected_at", "last_seen_at",
	"seen_count"}

var (
	setClause  = regexp.MustCompile(`(?is)\bSET\s+(.*?)\s+(?:WHERE|RETURNING)\b`)
	assignment = regexp.MustCompile(`(?:^|,)\s*([a-z_]+)\s*=`)
)

// setColumns lists the columns an UPDATE (or ON CONFLICT DO UPDATE) sets.
func setColumns(t *testing.T, sql string) []string {
	t.Helper()
	m := setClause.FindStringSubmatch(sql)
	if m == nil {
		t.Fatalf("no SET clause in:\n%s", sql)
	}
	var out []string
	for _, a := range assignment.FindAllStringSubmatch(m[1], -1) {
		out = append(out, a[1])
	}
	return out
}

func TestScoreWriteCarriesTheGateTag(t *testing.T) {
	if !strings.HasPrefix(writeScoreSQL, "/* pg_sage shadow:score v1 */") {
		t.Fatalf("score write is not tagged for the performance gate:\n%s", writeScoreSQL)
	}
	for _, sql := range []string{seenSQL, recordSQL, markAppliedSQL} {
		if strings.Contains(sql, "shadow:score") {
			t.Fatalf("a recurring update path carries the score tag:\n%s", sql)
		}
	}
}

func TestRecurringUpdatePathsSetOnlyTheRecurringColumns(t *testing.T) {
	seen := map[string]bool{}
	for _, sql := range []string{seenSQL, recordSQL, markAppliedSQL} {
		for _, c := range setColumns(t, sql) {
			seen[c] = true
		}
	}
	var got []string
	for c := range seen {
		got = append(got, c)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(recurringColumns, ",") {
		t.Fatalf("recurring paths set %v, want %v", got, recurringColumns)
	}
	score := strings.Join(setColumns(t, writeScoreSQL), ",")
	for _, c := range []string{"status", "score", "counted", "scored_at"} {
		if !strings.Contains(score, c) {
			t.Fatalf("score write sets %s, want %s among them", score, c)
		}
	}
}

func TestRecurringColumnsAreInNoIndex(t *testing.T) {
	pool, ctx := testPool(t)
	rows, err := pool.Query(ctx, `SELECT pg_get_indexdef(i.indexrelid) FROM pg_index i
		WHERE i.indrelid = 'sage.shadow_decision'::regclass`)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	var defs []string
	for rows.Next() {
		var def string
		if err := rows.Scan(&def); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		defs = append(defs, def)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("indexes: %v", err)
	}
	if len(defs) < 6 {
		t.Fatalf("%d indexes on sage.shadow_decision, want the primary key and the "+
			"shadow mode indexes: %v", len(defs), defs)
	}
	mentions := func(col string) []string {
		word := regexp.MustCompile(`\b` + col + `\b`)
		var hits []string
		for _, d := range defs {
			if word.MatchString(d) {
				hits = append(hits, d)
			}
		}
		return hits
	}
	for _, col := range recurringColumns {
		if hits := mentions(col); len(hits) > 0 {
			t.Errorf("%s is indexed, so its updates cannot be HOT: %v", col, hits)
		}
	}
	// The check can see a column: the score write's columns are indexed
	// (key, INCLUDE list and predicate).
	for _, col := range []string{"status", "scored_at", "counted"} {
		if len(mentions(col)) == 0 {
			t.Fatalf("%s is in no index definition: %v", col, defs)
		}
	}
}

// Integration: on a page with room, the recurring paths write heap-only
// tuples and the score write does not (the transaction's own counters).
func TestRecurringUpdatesAreHeapOnly(t *testing.T) {
	pool, ctx := testPool(t)
	d := record(t, NewStore(pool), sample("vacuum", `VACUUM public.o`))
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	counters := func() (upd, hot int64) {
		t.Helper()
		if err := tx.QueryRow(ctx, `SELECT
			pg_stat_get_xact_tuples_updated('sage.shadow_decision'::regclass),
			pg_stat_get_xact_tuples_hot_updated('sage.shadow_decision'::regclass)`).
			Scan(&upd, &hot); err != nil {
			t.Fatalf("read transaction counters: %v", err)
		}
		return upd, hot
	}
	for i := 0; i < 3; i++ {
		var seen bool
		if err := tx.QueryRow(ctx, seenSQL, d.DatabaseID, d.Fingerprint,
			3600.0).Scan(&seen); err != nil || !seen {
			t.Fatalf("seen bump %d: %t %v", i, seen, err)
		}
	}
	if _, err := tx.Exec(ctx, markAppliedSQL, d.ID); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	if upd, hot := counters(); upd != 4 || hot != 4 {
		t.Fatalf("recurring paths: %d updates, %d HOT; want 4 and 4", upd, hot)
	}
	if _, err := tx.Exec(ctx, writeScoreSQL, d.ID, "correct", "external", true, "r",
		map[string]any{}, int64(0), int64(0)); err != nil {
		t.Fatalf("score write: %v", err)
	}
	if upd, hot := counters(); upd != 5 || hot != 4 {
		t.Fatalf("after the score write: %d updates, %d HOT; want 5 and 4", upd, hot)
	}
}
