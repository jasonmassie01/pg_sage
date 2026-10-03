package perfgate

import (
	"strings"
	"testing"
)

const nestedPlan = `[{"Plan": {"Node Type": "Hash Join", "Plans": [
 {"Node Type": "Seq Scan", "Schema": "sage", "Relation Name": "decision", "Alias": "d"},
 {"Node Type": "Hash", "Plans": [
   {"Node Type": "Index Scan", "Schema": "sage", "Relation Name": "action_log"},
   {"Node Type": "Seq Scan", "Schema": "pg_catalog", "Relation Name": "pg_class"}
 ]},
 {"Node Type": "Subquery Scan", "Plans": [
   {"Node Type": "Seq Scan", "Schema": "sage", "Relation Name": "findings"},
   {"Node Type": "Seq Scan", "Schema": "sage", "Relation Name": "decision"}
 ]}
]}}]`

func TestSeqScansWalksNestedPlans(t *testing.T) {
	got, err := SeqScans([]byte(nestedPlan))
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// Every sage relation once, sorted; catalog scans are not sage scans.
	want := []PlanScan{{"sage", "decision"}, {"sage", "findings"}}
	if len(got) != len(want) {
		t.Fatalf("scans = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scans = %+v, want %+v", got, want)
		}
	}
}

func TestSeqScansCountsParallelAndBitmapCorrectly(t *testing.T) {
	plan := `[{"Plan": {"Node Type": "Gather", "Plans": [
	 {"Node Type": "Seq Scan", "Parallel Aware": true, "Schema": "sage",
	  "Relation Name": "query_store"},
	 {"Node Type": "Bitmap Heap Scan", "Schema": "sage", "Relation Name": "snapshots"}]}}]`
	got, err := SeqScans([]byte(plan))
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(got) != 1 || got[0].Relation != "query_store" {
		t.Fatalf("scans = %+v, want only the parallel seq scan of query_store", got)
	}
}

func TestSeqScansNoScans(t *testing.T) {
	got, err := SeqScans([]byte(`[{"Plan": {"Node Type": "Result"}}]`))
	if err != nil || len(got) != 0 {
		t.Fatalf("scans = %+v, err = %v; want none", got, err)
	}
}

func TestSeqScansRejectsMalformedPlans(t *testing.T) {
	for _, raw := range []string{"", "{", `{"Plan": {}}`, `[]`, `[{"NoPlan": 1}]`} {
		if _, err := SeqScans([]byte(raw)); err == nil {
			t.Errorf("plan %q accepted", raw)
		}
	}
}

func TestExplainable(t *testing.T) {
	yes := []string{
		"SELECT 1",
		"/* pg_sage */ SELECT * FROM sage.findings WHERE id = $1",
		"-- leading comment\nWITH x AS (SELECT 1) SELECT * FROM x",
		"  insert into sage.snapshots (data) values ($1)",
		"UPDATE sage.findings SET status = $1",
		"DELETE FROM sage.snapshots WHERE id = ANY($1)",
		"/* a */ /* b */ MERGE INTO sage.x USING y ON true WHEN MATCHED THEN DELETE",
	}
	no := []string{
		"", "BEGIN", "COMMIT", "SET statement_timeout = $1", "SET LOCAL lock_timeout = 1",
		"/* pg_sage */ VACUUM sage.snapshots", "CREATE INDEX CONCURRENTLY x ON y (z)",
		"EXPLAIN SELECT 1", "SELECT 1; SELECT 2", "/* unterminated SELECT 1",
		"DO $$ BEGIN END $$", "LOCK TABLE sage.findings", "COPY sage.x FROM STDIN",
	}
	for _, q := range yes {
		if !Explainable(q) {
			t.Errorf("%q not explainable", q)
		}
	}
	for _, q := range no {
		if Explainable(q) {
			t.Errorf("%q explainable", q)
		}
	}
}

func TestTouchesSage(t *testing.T) {
	if !TouchesSage("SELECT * FROM sage.findings") || !TouchesSage(`select 1 from "sage".x`) {
		t.Fatal("sage reference missed")
	}
	if TouchesSage("SELECT * FROM pg_class") || TouchesSage("SELECT usage.x FROM t usage") {
		t.Fatal("non-sage statement flagged")
	}
}

func TestShortQueryIsBoundedAndSingleLine(t *testing.T) {
	q := "SELECT  a,\n\tb\nFROM sage.findings " + strings.Repeat("x", 500)
	got := shortQuery(q)
	if strings.ContainsAny(got, "\n\t") || len(got) > 163 || !strings.HasPrefix(got, "SELECT a, b") {
		t.Fatalf("shortQuery = %q", got)
	}
	if shortQuery("SELECT 1") != "SELECT 1" {
		t.Fatal("short query altered")
	}
}
