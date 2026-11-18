package schemadebt

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/review/reviewtest"
)

// fakeSource answers in and records what it was asked.
type fakeSource struct {
	in    Inputs
	err   error
	cols  []ColumnRef
	since time.Time
}

func (f *fakeSource) Read(_ context.Context, cols []ColumnRef, since time.Time) (Inputs,
	error) {
	f.cols, f.since = cols, since
	return f.in, f.err
}

func TestGenerate_AsksTheSourceForTheDebtColumnsOverTheWindow(t *testing.T) {
	src := &fakeSource{in: keyInputs(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max))}
	pack := schemaPack(t, readers(), nil, ordersTable(1<<20))
	cands, cov, err := New(src, Options{}).Generate(t.Context(), pack, env())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(src.cols, ordersID) || !slices.Contains(src.cols, placedAt) {
		t.Fatalf("source asked for %v, want the key and the timestamp column", src.cols)
	}
	if !src.since.Equal(pack.Window.From) {
		t.Fatalf("source read samples since %s, want the window start %s", src.since,
			pack.Window.From)
	}
	if only(t, cands, KindInt4Key).ActionType != capacityAction ||
		cov.Status != candidate.CoverageComplete {
		t.Fatalf("candidates %+v coverage %+v", cands, cov)
	}
}

func TestGenerate_NilSourceMarksCatalogAndSamplesMissing(t *testing.T) {
	pack := schemaPack(t, nil, nil, ordersTable(1<<20))
	cands, cov, err := New(nil, Options{}).Generate(t.Context(), pack, env())
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "reason", only(t, cands, KindInt4Key).Reason, RateNotMeasured)
	for _, m := range []string{"catalog", "runway_samples"} {
		if !slices.Contains(cov.Missing, m) {
			t.Errorf("coverage %+v lacks %s", cov, m)
		}
	}
}

func TestGenerate_SourceErrorNamesTheDatabase(t *testing.T) {
	src := &fakeSource{err: errors.New("connection refused")}
	pack := schemaPack(t, nil, nil, ordersTable(1<<20))
	_, _, err := New(src, Options{}).Generate(t.Context(), pack, env())
	if err == nil || !strings.Contains(err.Error(), "db1") ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the database and the cause", err)
	}
}

func TestGenerate_NoDebtColumnsDoesNotCallTheSource(t *testing.T) {
	src := &fakeSource{err: errors.New("must not be called")}
	cands, _, err := New(src, Options{}).Generate(t.Context(), schemaPack(t, nil, nil), env())
	if err != nil || len(cands) != 0 || src.cols != nil {
		t.Fatalf("no tables: %+v %v, source asked %v", cands, err, src.cols)
	}
}

func TestBuild_RefusesInvalidInput(t *testing.T) {
	pack := schemaPack(t, nil, nil, ordersTable(1<<20))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]func() error{
		"nil pack": func() error {
			_, _, err := Build(t.Context(), nil, Inputs{}, env(), Options{})
			return err
		},
		"no database": func() error {
			_, _, err := Build(t.Context(), pack, Inputs{}, candidate.Env{WindowDays: 90},
				Options{})
			return err
		},
		"zero window": func() error {
			_, _, err := Build(t.Context(), pack, Inputs{}, candidate.Env{Database: "db1"},
				Options{})
			return err
		},
		"canceled": func() error {
			_, _, err := Build(canceled, pack, keyInputs(linear(ordersSeq, 1.9e9, 1e6, 5,
				int4Max)), env(), Options{})
			return err
		},
	}
	for name, run := range cases {
		if err := run(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestBuild_NoSchemaSectionIsMissing(t *testing.T) {
	p, err := reviewtest.Build(reviewtest.Spec{Database: "db1", Window: reviewtest.Window(90)})
	if err != nil {
		t.Fatal(err)
	}
	cands, cov, err := Build(t.Context(), p, Inputs{}, env(), Options{})
	if err != nil || len(cands) != 0 || !slices.Contains(cov.Missing, "schema") {
		t.Fatalf("no schema: %+v %+v %v", cands, cov, err)
	}
}

func TestBuild_TruncatedSchemaIsPartial(t *testing.T) {
	p, err := reviewtest.Build(reviewtest.Spec{Database: "db1", Window: reviewtest.Window(90),
		Schema: &workpack.Schema{Tables: []workpack.TableFact{ordersTable(1 << 20)},
			TablesTotal: 9000, Truncated: true}})
	if err != nil {
		t.Fatal(err)
	}
	_, cov, err := Build(t.Context(), p, Inputs{}, env(), Options{})
	if err != nil || cov.Status != candidate.CoveragePartial || len(cov.Notes) == 0 {
		t.Fatalf("truncated schema coverage %+v (%v)", cov, err)
	}
}

// everyKind is a pack and inputs that give a candidate of every debt kind.
func everyKind(t *testing.T) ([]candidate.Candidate, []string) {
	tabs := append(accountsAndOrders(), eventsTable(20<<30))
	tabs[1].Columns[0].Type = "integer" // public.orders.id is an int4 key again
	queue, idx := churnTable("jobs", "status", workpack.Writes{Inserts: 2e5, Updates: 3e5,
		Deletes: 2e5})
	soft, softIdx := churnTable("posts", "deleted_at", workpack.Writes{Inserts: 2e5,
		Updates: 3e5})
	idx.Indexes, idx.Total = append(idx.Indexes, softIdx.Indexes...), 2
	in := fkInputs()
	for k, v := range keyInputs(linear(ordersSeq, 1.9e9, 1e6, 5, int4Max)).Columns {
		in.Columns[k] = v
	}
	in.Series[ordersSeq] = linear(ordersSeq, 1.9e9, 1e6, 5, int4Max)
	in.Columns[placedAt] = timestampInputs().Columns[placedAt]
	pack := schemaPack(t, readers(), idx, append(tabs, queue, soft)...)
	kinds := []string{KindInt4Key, KindFKMismatch, KindColumnSwap, KindTimestampNoTZ,
		KindLargeTable, KindQueueTable, KindSoftDelete}
	return build(t, pack, in, Options{}), kinds
}

// CHECK-21: no schema-debt candidate has an action type outside advice,
// prepare_sequence_capacity_migration and an approval-only operator plan.
func TestBuild_EveryDebtKindStaysInsideTheAllowedActions(t *testing.T) {
	cands, kinds := everyKind(t)
	allowed := map[string]bool{candidate.ActionAdvice: true, capacityAction: true,
		candidate.ActionOperator: true}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			got := ofKind(cands, kind)
			if len(got) == 0 {
				t.Fatalf("no candidate of kind %s in %+v", kind, cands)
			}
			for _, c := range got {
				if !allowed[c.ActionType] || c.Approval != candidate.ApprovalRequired {
					t.Errorf("%s on %s: action %s approval %s", kind, c.Target, c.ActionType,
						c.Approval)
				}
				if c.Generator != Name || c.Dimension != Dimension || c.Reversible {
					t.Errorf("%s on %s: %s/%s reversible=%v", kind, c.Target, c.Generator,
						c.Dimension, c.Reversible)
				}
			}
		})
	}
	if len(cands) != len(slices.CompactFunc(slices.Clone(cands), func(a,
		b candidate.Candidate) bool {
		return a.ID == b.ID
	})) {
		t.Fatalf("duplicate candidate ids in %+v", cands)
	}
}

func TestTotals_CountsMeasuredRunwaysAndInt4Keys(t *testing.T) {
	cands, _ := everyKind(t)
	measured, keys, keysMeasured := Totals(cands)
	// the int4 key and the fk mismatch are measured; one int4 key, measured
	if measured != 2 || keys != 1 || keysMeasured != 1 {
		t.Fatalf("totals %d measured, %d keys, %d keys measured", measured, keys,
			keysMeasured)
	}
	if m, k, km := Totals(nil); m != 0 || k != 0 || km != 0 {
		t.Fatalf("nil totals %d %d %d", m, k, km)
	}
}
