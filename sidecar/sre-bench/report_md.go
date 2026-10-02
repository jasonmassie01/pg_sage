package srebench

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// pct renders a rate as a whole percentage; n/a for NaN.
func pct(v float64) string {
	if math.IsNaN(v) {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

// propText renders "rate (k/n) [lo-hi]" with the Wilson interval, or
// "n/a" without a denominator.
func propText(p Prop) string {
	if p.N == 0 {
		return "n/a"
	}
	lo, hi := p.Interval()
	return fmt.Sprintf("%s (%d/%d) [%.0f-%.0f]", pct(p.Rate()), p.K, p.N, lo*100, hi*100)
}

func metricText(m Metric) string { return propText(Prop{K: m.K, N: m.N}) }

func msText(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f ms", *v)
}

func floatText(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f", *v)
}

// cell escapes text for a Markdown table cell.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.Join(strings.Fields(s), " ")
}

// separator is the header separator row of an n-column table.
func separator(n int) string { return "|" + strings.Repeat(" --- |", n) + "\n" }

func row(cells ...string) string {
	for i, c := range cells {
		cells[i] = cell(c)
	}
	return "| " + strings.Join(cells, " | ") + " |\n"
}

var cellColumns = []string{"family", "arm", "runs", "Safe Pass", "top-1", "top-3",
	"clean top-1", "noise top-1", "decoy false dx", "abstention", "abstain (insufficient)",
	"selective acc.", "consistency", "forbidden", "probes/run", "TTFE p50", "packet p95",
	"errored/skipped"}

// Markdown renders the report: run context, the per family x arm table,
// the gates and every run.
func (r Report) Markdown() string {
	var b strings.Builder
	b.WriteString("# PGIncidentBench v1\n\n")
	fmt.Fprintf(&b, "- Server: %s\n- Repeats: %d\n- Generated: %s\n- LLM-on arm model: %s\n",
		r.ServerVersion, r.Repeats, r.GeneratedAt.Format("2006-01-02 15:04:05 MST"), r.LLM)
	fmt.Fprintf(&b, "- Gated arms: %s\n", strings.Join(r.Gated, ", "))
	pending := make([]string, 0, len(r.Pending))
	for arm := range r.Pending {
		pending = append(pending, arm)
	}
	sort.Strings(pending)
	for _, arm := range pending {
		fmt.Fprintf(&b, "- %s: not evaluated: %s\n", arm, r.Pending[arm])
	}
	b.WriteString("\nRates show (hits/denominator) [95% Wilson interval]; n/a has no " +
		"denominator.\n\n## Per family and arm\n\n")
	r.writeCells(&b)
	r.writeModel(&b)
	writeUsage(&b, r.usage(), r.LLM)
	b.WriteString("\n## Gates\n\n")
	b.WriteString(row("gate", "family", "arm", "status", "observed", "threshold", "note"))
	b.WriteString(separator(7))
	for _, g := range r.Gates {
		b.WriteString(row(g.ID, g.Family, g.Arm, string(g.Status), g.Observed, g.Threshold,
			g.Reason))
	}
	b.WriteString("\n## Runs\n\n")
	r.writeRuns(&b)
	if r.Replay != nil {
		b.WriteString("\n" + r.Replay.Markdown())
	}
	return b.String()
}

// usage is each arm's pooled model traffic.
func (r Report) usage() []ArmUsage {
	var out []ArmUsage
	for _, c := range r.Cells {
		if c.Family == PooledFamily && c.Model != nil {
			out = append(out, ArmUsage{Arm: c.Arm, Runs: c.Model.Runs, Usage: c.Model.Usage})
		}
	}
	return out
}

func (r Report) writeCells(b *strings.Builder) {
	b.WriteString(row(append([]string(nil), cellColumns...)...))
	b.WriteString(separator(len(cellColumns)))
	for _, c := range r.Cells {
		if c.Pending != "" {
			if c.Family == PooledFamily {
				cells := make([]string, len(cellColumns))
				cells[0], cells[1], cells[2] = c.Family, c.Arm, "not evaluated: "+c.Pending
				b.WriteString(row(cells...))
			}
			continue
		}
		b.WriteString(row(c.Family, c.Arm, fmt.Sprint(c.Runs), metricText(c.SafePass),
			metricText(c.Top1), metricText(c.Top3), metricText(c.CleanTop1),
			metricText(c.NoiseTop1), metricText(c.DecoyFalse), metricText(c.Abstention),
			metricText(c.InsufficientAbstention), metricText(c.Selective),
			metricText(c.Consistency), fmt.Sprint(c.Forbidden), floatText(c.ProbesPerRun),
			msText(c.FirstEvidenceP50MS), msText(c.PacketP95MS),
			fmt.Sprintf("%d/%d", c.Errored, c.Skipped)))
	}
}

// writeRuns renders one row per scenario and repeat, one column per arm.
func (r Report) writeRuns(b *strings.Builder) {
	head := append([]string{"scenario", "family", "class", "repeat", "gold"}, r.Arms...)
	b.WriteString(row(head...))
	b.WriteString(separator(len(head)))
	type key struct {
		scenario string
		repeat   int
	}
	var order []key
	byKey := map[key]map[string]RunRecord{}
	firsts := map[key]RunRecord{}
	for _, run := range r.Runs {
		k := key{run.Scenario, run.Repeat}
		if byKey[k] == nil {
			byKey[k], firsts[k] = map[string]RunRecord{}, run
			order = append(order, k)
		}
		byKey[k][run.Arm] = run
	}
	for _, k := range order {
		first := firsts[k]
		cells := []string{k.scenario, first.Family, first.Class, fmt.Sprint(k.repeat),
			goldText(first)}
		for _, arm := range r.Arms {
			run, ok := byKey[k][arm]
			cells = append(cells, runText(run, ok, r.Pending[arm] != ""))
		}
		b.WriteString(row(cells...))
	}
}

func goldText(run RunRecord) string {
	if run.GoldRoot == "" {
		if run.Lookalike != "" {
			return "inconclusive (lookalike " + run.Lookalike + ")"
		}
		return "inconclusive"
	}
	return strings.Join(append([]string{run.GoldRoot}, run.GoldContributing...), " + ")
}

func runText(run RunRecord, ok, pending bool) string {
	switch {
	case pending:
		return "not evaluated"
	case !ok:
		return "-"
	case run.Skipped != "":
		return "skipped: " + run.Skipped
	case run.Error != "":
		return "error: " + run.Error
	}
	text := "inconclusive"
	if run.Root != "" {
		text = strings.Join(append([]string{run.Root}, run.Contributing...), " + ")
	}
	if g := run.Grade; g != nil {
		text += " (" + gradeText(*g, run.Forbidden) + ")"
	}
	return text
}

func gradeText(g Grade, forbidden []string) string {
	switch {
	case g.Unsafe:
		return "UNSAFE: " + strings.Join(forbidden, "; ")
	case g.Top1, !g.Sufficient && g.Abstained:
		return "correct"
	case g.FalseRoot:
		return "false root"
	case g.Abstained:
		return "abstained"
	}
	return "wrong"
}
