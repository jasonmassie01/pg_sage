package backtest

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Draw is pure over its inputs and a seeded RNG: no concurrent-access
// test, it shares nothing.

func drawStmts() []Statement {
	return []Statement{
		{QueryID: 1, Tables: []string{"public.a"}, Calls: 100},
		{QueryID: 2, Tables: []string{"public.a", "public.b"}, Calls: 90},
		{QueryID: 3, Tables: []string{"public.b"}, Calls: 80},
		{QueryID: 4, Tables: []string{"public.c"}, Calls: 70},
		{QueryID: 5, Tables: []string{"public.c"}, Calls: 60},
		{QueryID: 6, Calls: 50},
	}
}

var (
	drawFrom = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	drawTo   = drawFrom.Add(20 * day)
)

func TestDraw_TargetsAndControlsFollowTheTable(t *testing.T) {
	ps := Draw(rand.New(rand.NewPCG(1, 2)), drawStmts(), nil, drawFrom, drawTo, 300)
	if len(ps) != 300 {
		t.Fatalf("drew %d, want 300", len(ps))
	}
	stmts := drawStmts()
	names := func(id int64, table string) bool {
		i := slices.IndexFunc(stmts, func(s Statement) bool { return s.QueryID == id })
		return slices.Contains(stmts[i].Tables, table)
	}
	global := 0
	for _, p := range ps {
		if p.At.Before(drawFrom) || !p.At.Before(drawTo) || p.At.Location() != time.UTC {
			t.Fatalf("placebo at %s outside [%s, %s)", p.At, drawFrom, drawTo)
		}
		if p.Kind == verify.AttributionGlobal {
			global++
			if p.Table != "" || len(p.Controls) != 0 || len(p.Targets) != 6 ||
				p.Targets[0] != 1 {
				t.Fatalf("global placebo %+v, want the busiest statements, no controls", p)
			}
			continue
		}
		if p.Kind != verify.AttributionIndex || p.Table == "" || len(p.Targets) == 0 {
			t.Fatalf("table placebo %+v", p)
		}
		for _, id := range p.Targets {
			if !names(id, p.Table) {
				t.Fatalf("target %d does not name %s", id, p.Table)
			}
		}
		for _, id := range p.Controls {
			if names(id, p.Table) || slices.Contains(p.Targets, id) {
				t.Fatalf("control %d names %s or is a target", id, p.Table)
			}
		}
	}
	if global < 45 || global > 105 {
		t.Fatalf("%d global placebos of 300, want about one in %d", global, GlobalShare)
	}
}

func TestDraw_KeepsTheGapFromRealActions(t *testing.T) {
	onA := drawFrom.Add(5 * day)
	anywhere := drawFrom.Add(15 * day)
	actions := []Action{{At: onA, Tables: []string{"public.a"}}, {At: anywhere}}
	ps := Draw(rand.New(rand.NewPCG(3, 4)), drawStmts(), actions, drawFrom, drawTo, 300)
	if len(ps) != 300 {
		t.Fatalf("drew %d, want 300", len(ps))
	}
	near := func(at, action time.Time) bool {
		d := at.Sub(action)
		return d > -ActionGap && d < ActionGap
	}
	sawOtherTableNearA := false
	for _, p := range ps {
		if near(p.At, anywhere) {
			t.Fatalf("placebo %+v within 72h of an action naming no table", p)
		}
		if near(p.At, onA) {
			if p.Table == "public.a" || p.Kind == verify.AttributionGlobal {
				t.Fatalf("placebo %+v within 72h of an action on its table", p)
			}
			sawOtherTableNearA = true
		}
	}
	if !sawOtherTableNearA {
		t.Fatal("no placebo on another table near the action on public.a: the gap " +
			"should be per table")
	}
}

func TestDraw_EmptyAndInvalid(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	if ps := Draw(rng, drawStmts(), nil, drawFrom, drawTo, 0); ps != nil {
		t.Fatalf("n=0 drew %d", len(ps))
	}
	if ps := Draw(rng, nil, nil, drawFrom, drawTo, 10); ps != nil {
		t.Fatalf("no statements drew %d", len(ps))
	}
	if ps := Draw(rng, drawStmts(), nil, drawTo, drawFrom, 10); ps != nil {
		t.Fatalf("an inverted window drew %d", len(ps))
	}
	untabled := []Statement{{QueryID: 9, Calls: 1}}
	ps := Draw(rng, untabled, nil, drawFrom, drawTo, 10)
	if len(ps) != 10 || ps[0].Kind != verify.AttributionGlobal {
		t.Fatalf("statements naming no table drew %+v, want global placebos", ps)
	}
	blocked := []Action{{At: drawFrom.Add(10 * day)}}
	short := Draw(rng, drawStmts(), blocked, drawFrom.Add(9*day), drawFrom.Add(11*day), 5)
	if len(short) != 0 {
		t.Fatalf("a window inside an action's gap drew %d placebos", len(short))
	}
}

func TestDraw_SameSeedSamePlacebos(t *testing.T) {
	a := Draw(rand.New(rand.NewPCG(5, rngStream)), drawStmts(), nil, drawFrom, drawTo, 50)
	b := Draw(rand.New(rand.NewPCG(5, rngStream)), drawStmts(), nil, drawFrom, drawTo, 50)
	c := Draw(rand.New(rand.NewPCG(6, rngStream)), drawStmts(), nil, drawFrom, drawTo, 50)
	if !slices.EqualFunc(a, b, samePlacebo) {
		t.Fatal("the same seed drew different placebos")
	}
	if slices.EqualFunc(a, c, samePlacebo) {
		t.Fatal("another seed drew the same placebos")
	}
}

func samePlacebo(x, y Placebo) bool {
	return x.At.Equal(y.At) && x.Kind == y.Kind && x.Table == y.Table &&
		slices.Equal(x.Targets, y.Targets) && slices.Equal(x.Controls, y.Controls)
}
