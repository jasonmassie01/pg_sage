package testdb

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Explain and XactScansOf run against PostgreSQL in every performance
// test that uses them; these tests pin the helpers' own logic (plan
// walking, decoding, recording) with a scripted row.

type scriptedRow struct {
	scan func(dest ...any) error
}

func (r scriptedRow) Scan(dest ...any) error { return r.scan(dest...) }

type scriptedQuerier struct {
	sql  string
	args []any
	scan func(dest ...any) error
}

func (q *scriptedQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	q.sql, q.args = sql, args
	return scriptedRow{scan: q.scan}
}

func rawPlan(doc string) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*[]byte) = []byte(doc)
		return nil
	}
}

const samplePlan = `[{"Plan": {"Node Type": "Nested Loop", "Actual Rows": 2,
  "Actual Loops": 1, "Plans": [
  {"Node Type": "Seq Scan", "Relation Name": "a", "Actual Rows": 10, "Actual Loops": 1},
  {"Node Type": "Index Scan", "Relation Name": "b", "Index Name": "b_pkey",
   "Index Cond": "(id = a.id)", "Actual Rows": 1, "Actual Loops": 10,
   "Output": ["b.id"], "Plans": [
     {"Node Type": "Seq Scan", "Relation Name": "a"}]}]}}]`

func TestExplain_DecodesThePlanAndAddsJSONFormat(t *testing.T) {
	q := &scriptedQuerier{scan: rawPlan(samplePlan)}
	plan, err := Explain(context.Background(), q, "ANALYZE", "SELECT $1", 7)
	if err != nil {
		t.Fatal(err)
	}
	if q.sql != "EXPLAIN (ANALYZE, FORMAT JSON) SELECT $1" || len(q.args) != 1 ||
		q.args[0] != 7 {
		t.Fatalf("sent %q %v", q.sql, q.args)
	}
	if plan.NodeType != "Nested Loop" || len(plan.Plans) != 2 ||
		plan.Plans[1].Index != "b_pkey" || plan.Plans[1].IndexCond != "(id = a.id)" ||
		plan.Plans[1].ActualLoops != 10 || plan.Plans[1].Output[0] != "b.id" {
		t.Fatalf("decoded %+v", plan)
	}
	if _, err := Explain(context.Background(), q, "  ", "SELECT 1"); err != nil ||
		q.sql != "EXPLAIN (FORMAT JSON) SELECT 1" {
		t.Fatalf("blank options sent %q (%v)", q.sql, err)
	}
}

func TestExplain_Errors(t *testing.T) {
	boom := errors.New("connection refused")
	for name, scan := range map[string]func(dest ...any) error{
		"query":     func(...any) error { return boom },
		"malformed": rawPlan(`{"Plan": `),
		"two plans": rawPlan(`[{"Plan": {}}, {"Plan": {}}]`),
		"no plan":   rawPlan(`[]`),
	} {
		_, err := Explain(context.Background(), &scriptedQuerier{scan: scan}, "", "SELECT 1")
		if err == nil {
			t.Errorf("%s: no error", name)
		}
		if name == "query" && (!errors.Is(err, boom) || !strings.Contains(err.Error(),
			"explain")) {
			t.Errorf("query error = %v, want it wrapped", err)
		}
	}
}

func TestPlanNode_WalkSeqScansHasString(t *testing.T) {
	plan, err := Explain(context.Background(), &scriptedQuerier{scan: rawPlan(samplePlan)},
		"", "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	plan.Walk(func(n PlanNode) { kinds = append(kinds, n.NodeType) })
	if strings.Join(kinds, ",") != "Nested Loop,Seq Scan,Index Scan,Seq Scan" {
		t.Fatalf("walk order %v", kinds)
	}
	if plan.SeqScans("a") != 2 || plan.SeqScans("b") != 0 || plan.SeqScans("") != 0 {
		t.Fatalf("seq scans a=%d b=%d", plan.SeqScans("a"), plan.SeqScans("b"))
	}
	if !plan.Has(func(n PlanNode) bool { return n.Index == "b_pkey" }) ||
		plan.Has(func(n PlanNode) bool { return n.NodeType == "Sort" }) {
		t.Fatal("Has does not match exactly the nodes present")
	}
	want := "Nested Loop (rows=2 loops=1)\n" +
		"  Seq Scan on a (rows=10 loops=1)\n" +
		"  Index Scan on b using b_pkey (rows=1 loops=10)\n" +
		"    Seq Scan on a\n"
	if got := plan.String(); got != want {
		t.Fatalf("String() =\n%s\nwant\n%s", got, want)
	}
	if (PlanNode{}).String() != "\n" || (PlanNode{}).SeqScans("a") != 0 {
		t.Fatal("empty plan")
	}
}

func TestQueryRecorder_MatchingAndReset(t *testing.T) {
	rec := &QueryRecorder{}
	ctx := context.Background()
	args := []any{1, "x"}
	if got := rec.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{
		SQL: "SELECT a FROM sage.t WHERE id = $1", Args: args}); got != ctx {
		t.Fatal("TraceQueryStart replaced the context")
	}
	args[0] = 99 // the caller reusing its slice must not change the record
	rec.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT b FROM sage.u"})
	rec.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	got := rec.Matching("sage.t", "id = $1")
	if len(got) != 1 || got[0].Args[0] != 1 || got[0].Args[1] != "x" {
		t.Fatalf("matching = %+v", got)
	}
	if n := len(rec.Matching("FROM sage.")); n != 2 {
		t.Fatalf("both statements match a shared fragment, got %d", n)
	}
	if n := len(rec.Matching()); n != 2 {
		t.Fatalf("no fragment matches everything, got %d", n)
	}
	if n := len(rec.Matching("sage.t", "sage.u")); n != 0 {
		t.Fatalf("every fragment must match, got %d", n)
	}
	rec.Reset()
	if n := len(rec.Matching()); n != 0 {
		t.Fatalf("after Reset %d statements", n)
	}
}

// No other concurrent access test: XactScans and PlanNode are values.
func TestQueryRecorder_ConcurrentStatements(t *testing.T) {
	rec := &QueryRecorder{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.TraceQueryStart(context.Background(), nil,
				pgx.TraceQueryStartData{SQL: "SELECT 1"})
			_ = rec.Matching("SELECT")
		}()
	}
	wg.Wait()
	if n := len(rec.Matching("SELECT 1")); n != 50 {
		t.Fatalf("recorded %d of 50 statements", n)
	}
}

func TestXactScansOf_ReadsAndSubtracts(t *testing.T) {
	q := &scriptedQuerier{scan: func(dest ...any) error {
		for i, v := range []int64{3, 300, 5, 7} {
			*dest[i].(*int64) = v
		}
		return nil
	}}
	got, err := XactScansOf(context.Background(), q, "sage.decision")
	if err != nil {
		t.Fatal(err)
	}
	if got != (XactScans{Seq: 3, SeqRead: 300, Index: 5, IndexFetch: 7}) ||
		len(q.args) != 1 || q.args[0] != "sage.decision" ||
		!strings.Contains(q.sql, "pg_stat_xact_user_tables") {
		t.Fatalf("read %+v with %q %v", got, q.sql, q.args)
	}
	delta := got.Minus(XactScans{Seq: 1, SeqRead: 100, Index: 5, IndexFetch: 2})
	if delta != (XactScans{Seq: 2, SeqRead: 200, Index: 0, IndexFetch: 5}) {
		t.Fatalf("delta %+v", delta)
	}
	if (XactScans{}).Minus(XactScans{}) != (XactScans{}) {
		t.Fatal("zero minus zero")
	}
	boom := errors.New("relation \"sage.nope\" does not exist")
	_, err = XactScansOf(context.Background(),
		&scriptedQuerier{scan: func(...any) error { return boom }}, "sage.nope")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "sage.nope") {
		t.Fatalf("error = %v, want it wrapped with the table", err)
	}
}
