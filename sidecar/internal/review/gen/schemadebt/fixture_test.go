package schemadebt

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/migration/plan"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

// sampleEnd is the newest runway sample of the fixtures: a day before the
// pack window (reviewtest.Window) ends.
var sampleEnd = time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

const (
	int4Max   = 2147483647
	ordersSeq = "public.orders_id_seq"
)

var ordersID = ColumnRef{Schema: "public", Table: "orders", Column: "id"}

// ordersTable is public.orders: an int4 serial key, a timestamp without
// time zone and a status column, of size bytes.
func ordersTable(size int64) workpack.TableFact {
	return workpack.TableFact{ID: "schema/table/public.orders", Table: "public.orders",
		Kind: "table", SizeBytes: size, Columns: []workpack.ColumnFact{
			{Name: "id", Type: "integer", NotNull: true},
			{Name: "placed_at", Type: "timestamp without time zone"},
			{Name: "note", Type: "text"}},
		Constraints: []string{"orders_pkey PRIMARY KEY (id)"},
		Indexes: []string{
			"CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)"}}
}

// schemaPack seals tables (and the families reading them) as a 90-day pack.
func schemaPack(t *testing.T, fams []workpack.Family, idx *workpack.Indexes,
	tables ...workpack.TableFact) *workpack.Pack {
	t.Helper()
	if tables == nil {
		tables = []workpack.TableFact{}
	}
	p, err := reviewtest.Build(reviewtest.Spec{Database: "db1", Window: reviewtest.Window(90),
		Workload: workpack.Workload{Families: fams}, Indexes: idx,
		Schema: &workpack.Schema{Tables: tables, TablesTotal: len(tables)}})
	if err != nil {
		t.Fatalf("build the pack: %v", err)
	}
	return p
}

// linear is n daily samples of a series ending at sampleEnd with last,
// rising by perDay, as the runway store records them.
func linear(subject string, last, perDay float64, n int, limit float64) Series {
	s := Series{Subject: subject, Epoch: "e1", Limit: limit}
	for i := n - 1; i >= 0; i-- {
		s.Points = append(s.Points, Point{At: sampleEnd.AddDate(0, 0, -i),
			Value: last - float64(i)*perDay})
	}
	return s
}

// serialFacts are the catalog facts of a plannable serial integer key
// schema.table.column owning schema.table_column_seq.
func serialFacts(schema, table, column string) plan.SwapFacts {
	seq := table + "_" + column + "_seq"
	return plan.SwapFacts{Schema: schema, Table: table, RelKind: "r",
		EstimatedRows: 1000, ServerVersion: 170000,
		Column: plan.SwapColumn{Name: column, Type: "integer", NotNull: true,
			Default:      "nextval('" + schema + "." + seq + "'::regclass)",
			DefaultIsSeq: true, SequenceSchema: schema, SequenceName: seq,
			SequenceType: "integer"},
		PrimaryKey: &plan.SwapKey{Constraint: table + "_pkey", Columns: []string{column},
			ColumnType: "integer"}}
}

// keyInputs are the facts of public.orders.id and its sequence's series.
func keyInputs(s Series) Inputs {
	return Inputs{Columns: map[ColumnRef]Column{ordersID: {
		Facts: serialFacts("public", "orders", "id"), Sequence: ordersSeq, Increment: 1}},
		Series: map[string]Series{s.Subject: s}}
}

func env() candidate.Env { return candidate.Env{Database: "db1", WindowDays: 90} }

func build(t *testing.T, pack *workpack.Pack, in Inputs, opts Options) []candidate.Candidate {
	t.Helper()
	cands, _, err := Build(context.Background(), pack, in, env(), opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return cands
}

// ofKind is the candidates of kind.
func ofKind(cands []candidate.Candidate, kind string) []candidate.Candidate {
	var out []candidate.Candidate
	for _, c := range cands {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

// only is the one candidate of kind, failing otherwise.
func only(t *testing.T, cands []candidate.Candidate, kind string) candidate.Candidate {
	t.Helper()
	got := ofKind(cands, kind)
	if len(got) != 1 {
		t.Fatalf("%d candidates of kind %s, want 1: %+v", len(got), kind, cands)
	}
	return got[0]
}

func mustContain(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s %q lacks %q", what, text, w)
		}
	}
}
