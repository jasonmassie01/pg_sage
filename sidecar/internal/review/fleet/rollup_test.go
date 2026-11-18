package fleet

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Tier 1: the deterministic rollup of every member.

func TestBuildRollups_EightWorkersMatchOne(t *testing.T) {
	fleet := synth(1000, 7, 500, 999)
	one := &mapSource{rollups: fleet}
	eight := &mapSource{rollups: fleet}
	a := BuildRollups(context.Background(), names(fleet), one, 1)
	b := BuildRollups(context.Background(), names(fleet), eight, 8)
	if len(a) != 1000 || !reflect.DeepEqual(a, b) {
		t.Fatalf("8 workers gave %d rollups unlike 1 worker's %d", len(b), len(a))
	}
	if one.reads.Load() != 1000 || eight.reads.Load() != 1000 {
		t.Fatalf("reads %d / %d, want each member once", one.reads.Load(), eight.reads.Load())
	}
	if a[0].ID != "R1" || a[0].Database != "db0001" || a[999].ID != "R1000" ||
		a[999].Database != "db1000" || a[6].GradeDrop() != 30 {
		t.Fatalf("ids/order: %s %s %s %s drop %v", a[0].ID, a[0].Database, a[999].ID,
			a[999].Database, a[6].GradeDrop())
	}
}

func TestBuildRollups_EmptyDuplicatesAndZeroWorkers(t *testing.T) {
	src := &mapSource{rollups: synth(2)}
	if got := BuildRollups(context.Background(), nil, src, 4); len(got) != 0 {
		t.Fatalf("no members gave %d rollups", len(got))
	}
	got := BuildRollups(context.Background(), []string{"db0002", "db0001", "db0002", ""},
		src, 0)
	if len(got) != 2 || got[0].Database != "db0001" || got[1].Database != "db0002" ||
		src.reads.Load() != 2 {
		t.Fatalf("rollups %+v after %d reads, want db0001 and db0002 once", got,
			src.reads.Load())
	}
}

func TestBuildRollups_ReadErrorIsKeptWithItsText(t *testing.T) {
	src := &mapSource{rollups: synth(2, 1),
		errs: map[string]error{"db0001": errors.New("dial tcp: connection refused")}}
	got := BuildRollups(context.Background(), []string{"db0001", "db0002"}, src, 2)
	if len(got) != 2 || !strings.Contains(got[0].Error, "connection refused") ||
		got[0].Graded || got[0].ID != "R1" || got[1].Error != "" {
		t.Fatalf("rollups %+v, want db0001 with its read error and db0002 whole", got)
	}
	if out := DeterministicTriage(got); len(out) != 0 {
		t.Fatalf("an unreadable member became an outlier: %+v", out)
	}
}

func TestBuildRollups_CanceledContextMarksTheRest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := BuildRollups(ctx, names(synth(5)), &mapSource{rollups: synth(5)}, 2)
	if len(got) != 5 {
		t.Fatalf("%d rollups, want 5", len(got))
	}
	for _, r := range got {
		if !strings.Contains(r.Error, "canceled") {
			t.Fatalf("rollup %+v lacks the cancel", r)
		}
	}
}

func TestRollup_GradeDrop(t *testing.T) {
	prev := func(v float64) *float64 { return &v }
	for _, c := range []struct {
		r    Rollup
		want float64
	}{
		{Rollup{Graded: true, Score: 70}, 0},
		{Rollup{Graded: true, Score: 80, PrevScore: prev(70)}, 0},
		{Rollup{Graded: true, Score: 60, PrevScore: prev(70)}, 10},
		{Rollup{Graded: false, Score: 0, PrevScore: prev(70)}, 0},
	} {
		if got := c.r.GradeDrop(); got != c.want {
			t.Errorf("GradeDrop(%+v) = %v, want %v", c.r, got, c.want)
		}
	}
}
