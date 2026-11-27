package upgradecheck

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/extschema"
)

// drill-02c: the brief records each table's query-time share, so the
// post-upgrade ANALYZE can be ordered after pg_upgrade empties
// pg_stat_statements. No concurrent-access test: tableShares and the
// section are pure functions.

func shareTables() []knownTable {
	return []knownTable{{Schema: "public", Name: "orders", Qualified: "public.orders"},
		{Schema: "public", Name: "notes", Qualified: "public.notes"},
		{Schema: "Sales", Name: "Big", Qualified: `"Sales"."Big"`},
		{Schema: "public", Name: "idle", Qualified: "public.idle"}}
}

func TestTableShares_AttributesEachStatementToTheTablesItNames(t *testing.T) {
	stmts := []statementTime{
		{QueryID: 1, Text: "SELECT * FROM orders o JOIN notes n ON n.id = o.id", TotalMs: 60},
		{QueryID: 2, Text: `SELECT count(*) FROM "Sales"."Big"`, TotalMs: 30},
		{QueryID: 3, Text: "UPDATE notes SET a = $1", TotalMs: 5},
		{QueryID: 4, Text: "SELECT 1", TotalMs: 5},
	}
	got, total := tableShares(stmts, shareTables())
	if total != 3 || len(got) != 3 {
		t.Fatalf("shares = %+v (total %d), want notes, orders, Sales.Big", got, total)
	}
	want := []TableShare{
		{Table: "public.notes", Share: 0.65, QueryIDs: []int64{1, 3}},
		{Table: "public.orders", Share: 0.6, QueryIDs: []int64{1}},
		{Table: `"Sales"."Big"`, Share: 0.3, QueryIDs: []int64{2}},
	}
	for i, w := range want {
		g := got[i]
		if g.Table != w.Table || math.Abs(g.Share-w.Share) > 1e-9 ||
			!slices.Equal(g.QueryIDs, w.QueryIDs) {
			t.Fatalf("share %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestTableShares_EmptyAndZeroInputs(t *testing.T) {
	if got, total := tableShares(nil, shareTables()); len(got) != 0 || total != 0 {
		t.Fatalf("no statements = %+v, %d", got, total)
	}
	if got, _ := tableShares([]statementTime{{QueryID: 1, Text: "SELECT * FROM orders",
		TotalMs: 3}}, nil); len(got) != 0 {
		t.Fatalf("no known tables = %+v, want none", got)
	}
	zero := []statementTime{{QueryID: 1, Text: "SELECT * FROM orders", TotalMs: 0}}
	if got, _ := tableShares(zero, shareTables()); len(got) != 0 {
		t.Fatalf("zero total time = %+v, want none (no share to divide)", got)
	}
}

func TestTableShares_BoundsTablesAndQueryIDs(t *testing.T) {
	var tables []knownTable
	var stmts []statementTime
	for i := range MaxRows + 10 {
		name := "t" + strings.Repeat("x", i)
		tables = append(tables, knownTable{Schema: "public", Name: name,
			Qualified: "public." + name})
		stmts = append(stmts, statementTime{QueryID: int64(i + 1),
			Text: "SELECT * FROM " + name, TotalMs: float64(i + 1)})
	}
	for i := range 9 {
		stmts = append(stmts, statementTime{QueryID: int64(1000 + i),
			Text: "SELECT 1 FROM t", TotalMs: 1})
	}
	got, total := tableShares(stmts, tables)
	if total != MaxRows+10 || len(got) != MaxRows {
		t.Fatalf("total %d len %d, want %d and %d", total, len(got), MaxRows+10, MaxRows)
	}
	if got[0].Table != "public.t"+strings.Repeat("x", MaxRows+9) {
		t.Fatalf("first = %s, want the busiest table", got[0].Table)
	}
	for _, s := range got {
		if len(s.QueryIDs) > maxShareQueryIDs {
			t.Fatalf("%s carries %d query ids, want at most %d", s.Table, len(s.QueryIDs),
				maxShareQueryIDs)
		}
	}
}

func TestQueryTimeSection_StatesAndHash(t *testing.T) {
	known := queryTimeSection(briefFacts{shares: []TableShare{{Table: "public.a",
		Share: 1}}, sharesTotal: 1})
	if known.State != SectionKnown || known.Total != 1 || len(known.Items) != 1 {
		t.Fatalf("known = %+v", known)
	}
	absent := queryTimeSection(briefFacts{sharesErr: extschema.ErrExtensionAbsent})
	if absent.State != SectionUnknown || !strings.Contains(absent.Reason, "size") ||
		absent.Items == nil {
		t.Fatalf("absent = %+v, want unknown naming the size fallback", absent)
	}
	failed := queryTimeSection(briefFacts{sharesErr: errors.New("boom")})
	if failed.State != SectionUnknown || !strings.Contains(failed.Reason, "boom") {
		t.Fatalf("failed = %+v", failed)
	}
	a := newBrief(Report{Database: "db", Target: Target{Major: 18}}, briefFacts{}, BriefOptions{})
	b := newBrief(Report{Database: "db", Target: Target{Major: 18}}, briefFacts{
		shares: []TableShare{{Table: "public.a", Share: 1}}, sharesTotal: 1}, BriefOptions{})
	// The share moves with every statement, so it is kept out of the hash
	// (else the delta-only brief history would grow a row per build).
	if a.Hash != b.Hash || b.QueryTime.Total != 1 {
		t.Fatalf("the query-time share changed the brief's hash: %s %s", a.Hash, b.Hash)
	}
}
