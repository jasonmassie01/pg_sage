package planstability

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

var env = candidate.Env{Database: "db1", WindowDays: 30}

// fakeSource is the catalog as a test states it; it records each call.
type fakeSource struct {
	mu    sync.Mutex
	facts Facts
	err   error
	calls [][]string
}

func (f *fakeSource) Read(_ context.Context, tables []string) (Facts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), tables...))
	return f.facts, f.err
}

func f64(v float64) *float64 { return &v }
func i16(v int16) *int16     { return &v }

// col is an analyzed column with no cliff.
func col(nullFrac, nDistinct float64) ColumnFacts {
	return ColumnFacts{NullFrac: nullFrac, NDistinct: nDistinct, Correlation: f64(0.1),
		TopFreq: f64(0.01), HasStats: true}
}

// clerkUsers is the Clerk shape: 1,000,000 rows, deleted_at 99.9996% NULL,
// a 50%-NULL control column, owned by pg_sage's role.
func clerkUsers() TableFacts {
	return TableFacts{Status: StatusOK, RelTuples: 1_000_000, ModSinceAnalyze: 10, Owned: true,
		Columns: map[string]ColumnFacts{
			"id":         col(0, -1),
			"deleted_at": col(0.999996, 4),
			"status":     col(0.5, 3),
			"region":     col(0, 12),
		}}
}

func source(tables map[string]TableFacts) *fakeSource {
	return &fakeSource{facts: Facts{DefaultTarget: 100, Role: "sage_agent", Tables: tables}}
}

// query is one top query of a family on rels.
type query struct {
	qid    string
	text   string
	rels   []string
	execMs float64
	calls  int64
	rows   int64
}

func queryID(qid string) string { return "workload/query/" + qid }

// packOf seals the queries' workload and texts, the stats section of the
// columns named, and plans (nil: no plans section).
func packOf(t *testing.T, queries []query, stats map[string][]string,
	plans []workpack.PlanFact) *workpack.Pack {
	t.Helper()
	spec := reviewtest.Spec{Database: "db1", Window: reviewtest.Window(30),
		Platform: "postgres", PGVersion: "17.2", Texts: &workpack.Texts{}}
	for i, q := range queries {
		fam := workpack.Family{ID: "workload/family/f" + strconv.Itoa(i), Key: q.qid,
			Command: "SELECT", Relations: q.rels, Calls: q.calls, ExecMs: q.execMs,
			Rows: q.rows, QueryIDs: []workpack.Query{{ID: queryID(q.qid), QueryID: q.qid,
				Family: "f" + strconv.Itoa(i), Calls: q.calls, ExecMs: q.execMs, Rows: q.rows,
				TextKnown: q.text != ""}}}
		spec.Workload.Families = append(spec.Workload.Families, fam)
		if q.text != "" {
			spec.Texts.Texts = append(spec.Texts.Texts, workpack.QueryText{QueryID: q.qid,
				Family: fam.ID, Text: q.text})
		}
	}
	if stats != nil {
		spec.Stats = statsSection(stats)
	}
	if plans != nil {
		spec.Plans = &workpack.Plans{Status: "ok", Plans: plans}
	}
	p, err := reviewtest.Build(spec)
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return p
}

func statsSection(stats map[string][]string) *workpack.Stats {
	out := &workpack.Stats{Tables: []workpack.StatsTable{}, Columns: []workpack.ColumnStats{}}
	tables := make([]string, 0, len(stats))
	for table := range stats {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		out.Tables = append(out.Tables, workpack.StatsTable{ID: "stats/table/" + table,
			Table: table, Status: "ok"})
		for _, c := range stats[table] {
			out.Columns = append(out.Columns, workpack.ColumnStats{
				ID: "stats/column/" + table + "." + c, Table: table, Column: c})
		}
	}
	out.ColumnsListed = len(out.Columns)
	return out
}

// clerkQuery is the top query filtering on the cliff column.
func clerkQuery() query {
	return query{qid: "101", rels: []string{"public.users"}, execMs: 90_000, calls: 1000,
		rows: 4, text: "SELECT id FROM public.users WHERE deleted_at IS NOT NULL"}
}

func clerkStats() map[string][]string {
	return map[string][]string{"public.users": {"deleted_at", "status", "id"}}
}

func generate(t *testing.T, src Source, p *workpack.Pack) ([]candidate.Candidate,
	candidate.Coverage) {
	t.Helper()
	cands, cov, err := New(src).Generate(context.Background(), p, env)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return cands, cov
}

func byAction(cands []candidate.Candidate, action string) []candidate.Candidate {
	var out []candidate.Candidate
	for _, c := range cands {
		if c.ActionType == action {
			out = append(out, c)
		}
	}
	return out
}

var errCatalog = errors.New("permission denied for table pg_statistic")
