package analyzer

import (
	"context"
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Roadmap 2.3: the analyzer hands its snapshots and findings to the fact
// filter (confirmed facts), which removes test-fixture schemas before the
// rules run and redirects or adds findings afterwards.

type recordingFactFilter struct {
	excluded  []string
	snapshots int
	gotIn     []Finding
	gotExcl   []string
	out       []Finding
	evaluated []string
}

func (f *recordingFactFilter) ExcludeSnapshot(_ context.Context,
	snaps ...*collector.Snapshot) []string {
	for _, s := range snaps {
		if s != nil {
			f.snapshots++
			s.Tables = nil
		}
	}
	return f.excluded
}

func (f *recordingFactFilter) ApplyFindings(_ context.Context, in []Finding,
	excluded []string) ([]Finding, []string) {
	f.gotIn, f.gotExcl = in, excluded
	return f.out, f.evaluated
}

func TestAnalyzerAppliesTheFactFilter(t *testing.T) {
	filter := &recordingFactFilter{excluded: []string{"test_a"},
		out:       []Finding{{Category: "test_fixture_cleanup", ObjectIdentifier: "test_*"}},
		evaluated: []string{"test_fixture_cleanup"}}
	a := &Analyzer{logFn: func(string, string, ...any) {}}
	a.WithFactFilter(filter)
	a.eval = newCycleEval()
	current := &collector.Snapshot{Tables: []collector.TableStats{{SchemaName: "test_a"}}}
	excluded := a.excludeFactSchemas(context.Background(), current, nil)
	if !reflect.DeepEqual(excluded, []string{"test_a"}) || filter.snapshots != 1 ||
		current.Tables != nil {
		t.Fatalf("excluded %v, snapshots %d, tables %v", excluded, filter.snapshots,
			current.Tables)
	}
	in := []Finding{{Category: "duplicate_index", ObjectIdentifier: "test_a.i"}}
	out := a.applyFactFilter(context.Background(), in, excluded)
	if !reflect.DeepEqual(out, filter.out) || !reflect.DeepEqual(filter.gotIn, in) ||
		!reflect.DeepEqual(filter.gotExcl, excluded) {
		t.Fatalf("out %+v, filter saw %+v / %v", out, filter.gotIn, filter.gotExcl)
	}
	if !a.eval.resolvable(nil)["test_fixture_cleanup"] {
		t.Fatal("the filter's evaluated categories must be resolvable")
	}
}

func TestAnalyzerWithoutAFactFilterPassesThrough(t *testing.T) {
	a := &Analyzer{logFn: func(string, string, ...any) {}}
	a.eval = newCycleEval()
	snap := &collector.Snapshot{Tables: []collector.TableStats{{SchemaName: "test_a"}}}
	if got := a.excludeFactSchemas(context.Background(), snap, snap); got != nil ||
		len(snap.Tables) != 1 {
		t.Fatalf("excluded %v tables %d", got, len(snap.Tables))
	}
	in := []Finding{{Category: "x"}}
	if got := a.applyFactFilter(context.Background(), in, nil); !reflect.DeepEqual(got, in) {
		t.Fatalf("out %+v", got)
	}
}
