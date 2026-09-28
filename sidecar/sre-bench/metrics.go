package srebench

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Score aggregates scored results (AI-SRE-SPEC §12 metrics): precision
// and recall of the supported mechanisms (root plus contributing)
// against the gold ones, top-1 where the cause is known, abstention
// where the right answer is "inconclusive".
type Score struct {
	N, TP, FP, FN                int
	Top1Correct, Top1Total       int
	AbstainCorrect, AbstainTotal int
}

func ratio(a, b int) float64 {
	if b == 0 {
		return math.NaN()
	}
	return float64(a) / float64(b)
}

// Precision is TP/(TP+FP); NaN when nothing was predicted.
func (s Score) Precision() float64 { return ratio(s.TP, s.TP+s.FP) }

// Recall is TP/(TP+FN); NaN when nothing was expected.
func (s Score) Recall() float64 { return ratio(s.TP, s.TP+s.FN) }

// Top1 is the share of known-cause scenarios whose root matched.
func (s Score) Top1() float64 { return ratio(s.Top1Correct, s.Top1Total) }

// Abstention is the share of benign scenarios left inconclusive.
func (s Score) Abstention() float64 { return ratio(s.AbstainCorrect, s.AbstainTotal) }

func (s *Score) add(r Result) {
	s.N++
	predicted := map[string]bool{}
	if r.Outcome.Root != "" {
		predicted[r.Outcome.Root] = true
		for _, c := range r.Outcome.Contributing {
			predicted[c] = true
		}
	}
	gold := map[string]bool{}
	if r.Scenario.Gold.Root != "" {
		gold[r.Scenario.Gold.Root] = true
		for _, c := range r.Scenario.Gold.Contributing {
			gold[c] = true
		}
	}
	for m := range predicted {
		if gold[m] {
			s.TP++
		} else {
			s.FP++
		}
	}
	for m := range gold {
		if !predicted[m] {
			s.FN++
		}
	}
	if r.Scenario.Gold.Root == "" {
		s.AbstainTotal++
		if r.Outcome.Root == "" {
			s.AbstainCorrect++
		}
		return
	}
	s.Top1Total++
	if r.Outcome.Root == r.Scenario.Gold.Root {
		s.Top1Correct++
	}
}

func scored(r Result) bool { return r.Skipped == "" && r.Err == nil }

// ScoreResults scores per family and pooled.
func ScoreResults(rs []Result) (map[string]Score, Score) {
	fams := map[string]Score{}
	var pooled Score
	for _, r := range rs {
		if !scored(r) {
			continue
		}
		f := fams[string(r.Scenario.Family)]
		f.add(r)
		fams[string(r.Scenario.Family)] = f
		pooled.add(r)
	}
	return fams, pooled
}

// ScoreClass scores one scenario class.
func ScoreClass(rs []Result, class string) Score {
	var s Score
	for _, r := range rs {
		if scored(r) && r.Scenario.Class == class {
			s.add(r)
		}
	}
	return s
}

func pct(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

func (s Score) row(name string) string {
	return fmt.Sprintf("| %s | %d | %s | %s | %s (%d/%d) | %s (%d/%d) |\n", name, s.N,
		pct(s.Precision()), pct(s.Recall()), pct(s.Top1()), s.Top1Correct, s.Top1Total,
		pct(s.Abstention()), s.AbstainCorrect, s.AbstainTotal)
}

// Report renders the per-scenario outcomes and the scores as Markdown.
func Report(rs []Result) string {
	var b strings.Builder
	b.WriteString("| scenario | family | class | gold | diagnosis | verdict |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", r.Scenario.ID,
			r.Scenario.Family, r.Scenario.Class, goldText(r.Scenario.Gold),
			outcomeText(r.Outcome), verdict(r))
	}
	fams, pooled := ScoreResults(rs)
	b.WriteString("\n| family | n | precision | recall | top-1 | abstention |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	names := make([]string, 0, len(fams))
	for name := range fams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(fams[name].row(name))
	}
	b.WriteString(pooled.row("all"))
	return b.String()
}

func goldText(g Gold) string {
	if g.Root == "" {
		return "inconclusive"
	}
	return strings.Join(append([]string{g.Root}, g.Contributing...), " + ")
}

func outcomeText(o Outcome) string {
	if o.Root == "" {
		return string(o.State)
	}
	return strings.Join(append([]string{o.Root}, o.Contributing...), " + ")
}

func verdict(r Result) string {
	switch {
	case r.Skipped != "":
		return "skipped: " + r.Skipped
	case r.Err != nil:
		return "error: " + r.Err.Error()
	case r.Outcome.Root == r.Scenario.Gold.Root:
		return "correct"
	}
	return "wrong"
}
