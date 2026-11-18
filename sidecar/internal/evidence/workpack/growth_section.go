package workpack

import (
	"time"
)

// The growth section: weekly calls and exec time per listed family over
// the window's whole weeks (a trailing partial week is left out), and the
// daily database and largest-table sizes, each with its fit (stats.go). A
// week with no rollup coverage and no rows is missing, not zero.

const week = 7 * 24 * time.Hour

// weekWindows are the window's whole weeks from its start.
func weekWindows(w Window) []Window {
	n := int(w.To.Sub(w.From) / week)
	out := make([]Window, n)
	for k := range n {
		out[k] = Window{w.From.Add(time.Duration(k) * week),
			w.From.Add(time.Duration(k+1) * week)}
	}
	return out
}

// weekIndex is t's whole week, or -1 outside them.
func weekIndex(w Window, n int, t time.Time) int {
	if t.Before(w.From) {
		return -1
	}
	k := int(t.Sub(w.From) / week)
	if k >= n {
		return -1
	}
	return k
}

// familyWeeks sums each family's calls and exec time per week, and marks
// the weeks seen (any coverage hour or row).
type familyWeeks struct {
	calls, exec map[string][]float64
	seen        []bool
}

func sumWeeks(plan dayPlan, w Window, h history, famOf map[int64]string) familyWeeks {
	n := len(weekWindows(w))
	fw := familyWeeks{calls: map[string][]float64{}, exec: map[string][]float64{},
		seen: make([]bool, n)}
	add := func(qid int64, t time.Time, calls int64, exec float64) {
		k := weekIndex(w, n, t)
		if k < 0 {
			return
		}
		fw.seen[k] = true
		key := famOf[qid]
		if fw.calls[key] == nil {
			fw.calls[key], fw.exec[key] = make([]float64, n), make([]float64, n)
		}
		fw.calls[key][k] += float64(calls)
		fw.exec[key][k] += exec
	}
	daily := map[string]bool{}
	for _, d := range plan.dailyDays {
		daily[d] = true
	}
	for _, r := range h.hourly {
		if inHourly(plan, w, r.hour) {
			add(r.qid, r.hour, r.calls, r.execMs)
		}
	}
	for _, r := range h.daily {
		if daily[r.day.UTC().Format(time.DateOnly)] {
			add(r.qid, r.day.UTC(), r.calls, r.execMs)
		}
	}
	for _, t := range h.covered {
		if k := weekIndex(w, n, t); k >= 0 {
			fw.seen[k] = true
		}
	}
	return fw
}

// weekly is a family's series: nil for an unseen week.
func weekly(sums []float64, seen []bool) []*float64 {
	out := make([]*float64, len(seen))
	for k := range seen {
		if seen[k] {
			v := 0.0
			if sums != nil {
				v = round12(sums[k])
			}
			out[k] = &v
		}
	}
	return out
}

// buildGrowth is the growth section for the listed families.
func buildGrowth(req Request, plan dayPlan, h history, fams []*famAcc) Growth {
	w := req.Window.UTC()
	totals := queryTotals(plan, w, h)
	_, famOf := groupFamilies(totals, h)
	fw := sumWeeks(plan, w, h, famOf)
	g := Growth{Weeks: weekWindows(w), Workload: []FamilyGrowth{}, Tables: []Series{}}
	for _, f := range fams {
		calls, exec := weekly(fw.calls[f.key], fw.seen), weekly(fw.exec[f.key], fw.seen)
		g.Workload = append(g.Workload, FamilyGrowth{ID: "growth/workload/" + familyHex(f.key),
			Family: familyID(f.key), Calls: Trend{Values: calls, Fit: fit(calls)},
			ExecMs: Trend{Values: exec, Fit: fit(exec)}})
	}
	g.DBSize = dailySeries("growth/db/size", "", plan, h.dbSize)
	for _, table := range h.largest {
		g.Tables = append(g.Tables, dailySeries("growth/table/"+table, table, plan,
			h.tables[table]))
	}
	return g
}

// dailySeries is one value per header day from byDay (day to bytes).
func dailySeries(id, table string, plan dayPlan, byDay map[string]int64) Series {
	s := Series{ID: id, Table: table, Unit: "bytes", Values: make([]*float64, len(plan.days))}
	for i, d := range plan.days {
		v, ok := byDay[d.Day]
		if !ok {
			s.MissingDays++
			continue
		}
		f := float64(v)
		s.Values[i] = &f
	}
	s.Fit = fit(s.Values)
	return s
}
