package autovacuum

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/pg-sage/sidecar/internal/evidence/workpack"
	"github.com/pg-sage/sidecar/internal/pgconf"
	"github.com/pg-sage/sidecar/internal/review/candidate"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

// plan is the arithmetic's result for one table.
type plan struct {
	peak, target, growth, lag, budget float64
	scale, thresh, analyze            float64
	keys                              []string // the keys the step sets, in order
}

// judge is one table's candidate, if any; a table it cannot judge is
// noted in the coverage with why.
func judge(f workpack.AutovacuumFact, g globals, cov *candidate.Coverage,
) (candidate.Candidate, bool) {
	t, why := readTable(f, g)
	if why != "ok" {
		if why != "" {
			cov.Note("%s left alone: %s", f.Table, why)
		}
		return candidate.Candidate{}, false
	}
	p := plan{peak: maxOf(t.dead), target: math.Floor(TargetDeadShare * t.live),
		growth: medianRise(t.dead) / 24, lag: g.naptimeHours + MarginHours}
	if p.peak <= p.target {
		return candidate.Candidate{}, false
	}
	p.budget = p.target - p.growth*p.lag
	c := base(t, g, p)
	if p.budget < 1 || !p.size(t) {
		return fallingBehind(c, t, p), true
	}
	if why := p.render(&c, t); why != "" {
		cov.Note("%s left alone: %s", f.Table, why)
		return candidate.Candidate{}, false
	}
	return c, true
}

// size computes the new trigger. The threshold drops to at most a tenth of
// the budget, only together with keys of the same prior state (one
// statement must roll back exactly); the scale factor takes the rest of
// the budget, floored to 0.001. false when it would not tighten the
// trigger, or the tightest trigger it may set still predicts a peak over
// the target (a threshold it cannot lower, or the 0.001 floor): no step
// may claim a peak it cannot hold.
func (p *plan) size(t table) bool {
	set := func(k string) bool { _, ok := t.fact.Reloptions[k]; return ok }
	p.keys, p.thresh = []string{keyScale}, t.thresh
	if want := math.Min(t.thresh, math.Floor(p.budget*thresholdShare)); want != t.thresh &&
		set(keyThresh) == set(keyScale) {
		p.keys, p.thresh = append(p.keys, keyThresh), want
	}
	p.scale = math.Max(0.001, math.Floor((p.budget-p.thresh)/t.live*1000)/1000)
	if p.scale >= t.scale || p.predicted(t) > p.target {
		return false
	}
	p.analyze = math.Max(0.001, math.Floor(p.scale/2*1000)/1000)
	if p.analyze < t.anal && set(keyAnalyze) == set(keyScale) {
		p.keys = append(p.keys, keyAnalyze)
	}
	return true
}

// render writes the step's SQL, rollback and prediction; why is set when
// a value falls outside pgconf's range (never emitted raw).
func (p plan) render(c *candidate.Candidate, t table) string {
	values := map[string]string{keyScale: num(p.scale), keyThresh: num(p.thresh),
		keyAnalyze: num(p.analyze)}
	var set, prior, reset []string
	for _, k := range p.keys {
		if ok, why := pgconf.ValidateReloption(k, values[k]); !ok {
			return why
		}
		set = append(set, k+" = "+values[k])
		if v, ok := t.fact.Reloptions[k]; ok {
			prior = append(prior, k+" = "+literal(v))
		} else {
			reset = append(reset, k)
		}
	}
	rel := sanitize.QuoteQualifiedName(t.schema, t.name)
	c.ActionType, c.Reversible = "set_table_autovacuum", true
	c.Approval = candidate.ApprovalAutomatic
	c.ForwardSQL = "ALTER TABLE " + rel + " SET (" + strings.Join(set, ", ") + ")"
	c.RollbackSQL = "ALTER TABLE " + rel + " RESET (" + strings.Join(reset, ", ") + ")"
	if len(prior) > 0 {
		c.RollbackSQL = "ALTER TABLE " + rel + " SET (" + strings.Join(prior, ", ") + ")"
	}
	predicted := p.predicted(t)
	pct := (p.peak - predicted) / p.peak * 100
	c.Prediction.SizePct = &pct
	c.Prediction.Quantities["predicted_peak"] = predicted
	c.Reason = fmt.Sprintf("Dead tuples on %s peaked at %.0f, %.1f%% of its %.0f live rows; "+
		"at %.0f new dead tuples an hour, %s keeps the predicted peak at %.0f, under the "+
		"%.0f%% target.", t.fact.Table, p.peak, p.peak/t.live*100, t.live, p.growth,
		strings.Join(set, ", "), predicted, TargetDeadShare*100)
	c.ID = candidate.NewID(c.Generator, c.Kind, c.Target, c.IDBody())
	return ""
}

// predicted is the dead-tuple peak under the planned trigger: the trigger
// plus the growth while autovacuum gets to the table.
func (p plan) predicted(t table) float64 {
	return p.thresh + p.scale*t.live + p.growth*p.lag
}

// base is the candidate's common part: what it cites and the numbers.
func base(t table, g globals, p plan) candidate.Candidate {
	return candidate.Candidate{Generator: Name, Dimension: Dimension, Kind: KindTable,
		Target: t.fact.Table, EvidenceIDs: append([]string{t.fact.ID}, g.ids...),
		Prediction: candidate.Prediction{Metric: MetricDeadPeak, Direction: "down",
			Tier: candidate.TierNone, Source: "arithmetic: dead-tuple growth fitted over " +
				"the window's daily snapshots",
			Quantities: map[string]float64{"live_tuples": t.live, "dead_tuple_peak": p.peak,
				"target_dead": p.target, "growth_per_hour": p.growth, "lag_hours": p.lag}}}
}

// fallingBehind is advice: no trigger holds the target, because the dead
// tuples grow faster than autovacuum clears them or the trigger is already
// tighter than the arithmetic asks.
func fallingBehind(c candidate.Candidate, t table, p plan) candidate.Candidate {
	c.Kind, c.ActionType = KindFallingBehind, candidate.ActionAdvice
	c.Reason = fmt.Sprintf("Autovacuum is falling behind on %s: dead tuples peaked at %.0f "+
		"(target %.0f) and grow %.0f an hour, and a tighter trigger (scale factor now %s, "+
		"threshold now %s) cannot hold the target. Look at the table's own threshold, "+
		"autovacuum_vacuum_cost_limit, the number of workers and long-running "+
		"transactions that hold back cleanup.", t.fact.Table, p.peak, p.target, p.growth,
		num(t.scale), num(t.thresh))
	c.ID = candidate.NewID(c.Generator, c.Kind, c.Target, c.IDBody())
	return c
}

// medianRise is the median of the day-over-day rises (a drop is a vacuum).
func medianRise(dead []float64) float64 {
	var rises []float64
	for i := 1; i < len(dead); i++ {
		if d := dead[i] - dead[i-1]; d > 0 {
			rises = append(rises, d)
		}
	}
	if len(rises) == 0 {
		return 0
	}
	sort.Float64s(rises)
	m := len(rises) / 2
	if len(rises)%2 == 0 {
		return (rises[m-1] + rises[m]) / 2
	}
	return rises[m]
}

func maxOf(xs []float64) float64 {
	m := xs[0]
	for _, x := range xs[1:] {
		m = math.Max(m, x)
	}
	return m
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// literal renders a stored reloption value: bare when it is a plain
// number, quoted otherwise (PostgreSQL then stores it as written).
func literal(v string) string {
	if _, err := strconv.ParseFloat(v, 64); err == nil && !strings.ContainsAny(v, " +-eE") {
		return v
	}
	return sanitize.QuoteLiteral(v)
}
