package backtest

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/pg-sage/sidecar/internal/verify"
)

// Placebo is a step that changed nothing: a time, and the targets and
// controls a real step on Table would have had. A global placebo (Kind
// verify.AttributionGlobal) stands for a setting: no table, no controls.
type Placebo struct {
	At       time.Time
	Kind     string
	Table    string
	Targets  []int64
	Controls []int64
}

// drawAttempts bounds the rejection sampling per requested placebo.
const drawAttempts = 50

// Draw draws n placebos in [from, to) with rng: a table placebo targets
// the statements of one random table (the busiest maxTargets) with the
// busiest statements that do not name it as controls; one in GlobalShare
// is global and targets the busiest statements. A time within ActionGap
// of a real action on the placebo's tables is redrawn. Fewer than n come
// back when the window has no room for them.
func Draw(rng *rand.Rand, stmts []Statement, actions []Action, from, to time.Time,
	n int) []Placebo {
	if n <= 0 || !to.After(from) || len(stmts) == 0 {
		return nil
	}
	busiest := slices.Clone(stmts)
	slices.SortStableFunc(busiest, func(a, b Statement) int {
		return cmp.Or(cmp.Compare(b.Calls, a.Calls), cmp.Compare(a.QueryID, b.QueryID))
	})
	tables := tablesOf(busiest)
	out := make([]Placebo, 0, n)
	for attempt := 0; len(out) < n && attempt < n*drawAttempts; attempt++ {
		p := Placebo{Kind: verify.AttributionGlobal}
		if len(tables) > 0 && rng.IntN(GlobalShare) != 0 {
			p = tablePlacebo(busiest, tables[rng.IntN(len(tables))])
		} else {
			p.Targets = topIDs(busiest, maxTargets, func(Statement) bool { return true })
		}
		p.At = from.Add(time.Duration(rng.Int64N(int64(to.Sub(from))))).
			Truncate(time.Minute).UTC()
		if len(p.Targets) == 0 || nearAction(p, actions) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func tablePlacebo(stmts []Statement, table string) Placebo {
	names := func(s Statement) bool { return slices.Contains(s.Tables, table) }
	return Placebo{Kind: verify.AttributionIndex, Table: table,
		Targets:  topIDs(stmts, maxTargets, names),
		Controls: topIDs(stmts, maxControls, func(s Statement) bool { return !names(s) })}
}

// topIDs are the first limit queryids of stmts (busiest first) that keep.
func topIDs(stmts []Statement, limit int, keep func(Statement) bool) []int64 {
	var out []int64
	for _, s := range stmts {
		if len(out) == limit {
			break
		}
		if keep(s) {
			out = append(out, s.QueryID)
		}
	}
	return out
}

func tablesOf(stmts []Statement) []string {
	var out []string
	for _, s := range stmts {
		for _, t := range s.Tables {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	slices.Sort(out)
	return out
}

// nearAction reports a real action within ActionGap of p on p's tables: a
// global placebo, or an action naming no table, is near every action.
func nearAction(p Placebo, actions []Action) bool {
	for _, a := range actions {
		gap := p.At.Sub(a.At)
		if gap < 0 {
			gap = -gap
		}
		if gap >= ActionGap {
			continue
		}
		if p.Table == "" || len(a.Tables) == 0 || slices.Contains(a.Tables, p.Table) {
			return true
		}
	}
	return false
}
