package testdb

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

// Plan-shape helpers for performance tests: pg_sage's own statements must
// find their rows through an index (reviews/2026-10-03-perf-gate-report.md),
// so a test explains the statement as it is executed and asserts on the
// plan instead of on timings.

// PlanNode is one node of an EXPLAIN (FORMAT JSON) plan.
type PlanNode struct {
	NodeType    string     `json:"Node Type"`
	Relation    string     `json:"Relation Name"`
	Index       string     `json:"Index Name"`
	IndexCond   string     `json:"Index Cond"`
	ActualRows  float64    `json:"Actual Rows"`
	ActualLoops float64    `json:"Actual Loops"`
	Output      []string   `json:"Output"`
	Plans       []PlanNode `json:"Plans"`
}

// Querier is what Explain needs (a pool, a connection or a transaction).
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Explain returns the root plan node of sql explained with options (for
// example "ANALYZE, VERBOSE"; FORMAT JSON is added).
func Explain(ctx context.Context, q Querier, options, sql string,
	args ...any) (PlanNode, error) {
	opts := "FORMAT JSON"
	if strings.TrimSpace(options) != "" {
		opts = options + ", FORMAT JSON"
	}
	var raw []byte
	if err := q.QueryRow(ctx, "EXPLAIN ("+opts+") "+sql, args...).Scan(&raw); err != nil {
		return PlanNode{}, fmt.Errorf("explain: %w", err)
	}
	var out []struct {
		Plan PlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
		return PlanNode{}, fmt.Errorf("decode plan (%d plans): %v", len(out), err)
	}
	return out[0].Plan, nil
}

// Walk visits n and every node below it.
func (n PlanNode) Walk(fn func(PlanNode)) {
	fn(n)
	for _, child := range n.Plans {
		child.Walk(fn)
	}
}

// SeqScans counts the sequential scans of relation in the plan.
func (n PlanNode) SeqScans(relation string) int {
	count := 0
	n.Walk(func(p PlanNode) {
		if p.NodeType == "Seq Scan" && p.Relation == relation {
			count++
		}
	})
	return count
}

// Has reports whether some node of the plan satisfies match.
func (n PlanNode) Has(match func(PlanNode) bool) bool {
	found := false
	n.Walk(func(p PlanNode) { found = found || match(p) })
	return found
}

// String renders the plan as an indented node list for failure messages.
func (n PlanNode) String() string {
	var b strings.Builder
	var walk func(PlanNode, int)
	walk = func(p PlanNode, depth int) {
		fmt.Fprintf(&b, "%s%s", strings.Repeat("  ", depth), p.NodeType)
		if p.Relation != "" {
			fmt.Fprintf(&b, " on %s", p.Relation)
		}
		if p.Index != "" {
			fmt.Fprintf(&b, " using %s", p.Index)
		}
		if p.ActualLoops > 0 {
			fmt.Fprintf(&b, " (rows=%g loops=%g)", p.ActualRows, p.ActualLoops)
		}
		b.WriteString("\n")
		for _, c := range p.Plans {
			walk(c, depth+1)
		}
	}
	walk(n, 0)
	return b.String()
}

// RecordedQuery is one statement a pool sent, with its arguments.
type RecordedQuery struct {
	SQL  string
	Args []any
}

// QueryRecorder is a pgx.QueryTracer that records every statement, so a
// test can explain exactly what the code under test executed.
type QueryRecorder struct {
	mu      sync.Mutex
	queries []RecordedQuery
}

// TraceQueryStart records the statement.
func (r *QueryRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn,
	data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, RecordedQuery{SQL: data.SQL,
		Args: append([]any(nil), data.Args...)})
	return ctx
}

// TraceQueryEnd does nothing.
func (r *QueryRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Matching returns the recorded statements containing every fragment.
func (r *QueryRecorder) Matching(fragments ...string) []RecordedQuery {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []RecordedQuery
	for _, q := range r.queries {
		all := true
		for _, f := range fragments {
			all = all && strings.Contains(q.SQL, f)
		}
		if all {
			out = append(out, q)
		}
	}
	return out
}

// Reset forgets the recorded statements.
func (r *QueryRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = nil
}

// XactScans are a table's scan counters of the current transaction
// (pg_stat_xact_user_tables): visible at once, without waiting for the
// statistics to be flushed, so run the code under test and read them in
// one transaction.
type XactScans struct {
	Seq        int64 // sequential scans
	SeqRead    int64 // rows read by them
	Index      int64 // index scans
	IndexFetch int64 // rows fetched by them
}

// XactScansOf reads table's (schema-qualified) scan counters of the
// current transaction.
func XactScansOf(ctx context.Context, q Querier, table string) (XactScans, error) {
	var s XactScans
	err := q.QueryRow(ctx, `SELECT COALESCE(sum(seq_scan), 0)::int8,
		COALESCE(sum(seq_tup_read), 0)::int8, COALESCE(sum(idx_scan), 0)::int8,
		COALESCE(sum(idx_tup_fetch), 0)::int8
		FROM pg_catalog.pg_stat_xact_user_tables WHERE relid = $1::regclass`,
		table).Scan(&s.Seq, &s.SeqRead, &s.Index, &s.IndexFetch)
	if err != nil {
		return XactScans{}, fmt.Errorf("read scan counters of %s: %w", table, err)
	}
	return s, nil
}

// Minus is the change from before to s.
func (s XactScans) Minus(before XactScans) XactScans {
	return XactScans{Seq: s.Seq - before.Seq, SeqRead: s.SeqRead - before.SeqRead,
		Index: s.Index - before.Index, IndexFetch: s.IndexFetch - before.IndexFetch}
}
