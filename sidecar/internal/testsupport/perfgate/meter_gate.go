package perfgate

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// Gate H tolerances. The product meter and the gate read the same
// pg_stat_statements, so they differ only by sampling windows and by
// statements the meter cannot see (untagged ones): 10% of the gate's DB
// time per cycle, never less than 5 ms; the meter's cycle within 10% of
// the collector period observed.
const (
	meterTolerancePct      = 10.0
	meterFloorMs           = 5.0
	meterCycleTolerancePct = 10.0
)

// pgSageTag starts the comment that marks pg_sage's statements: the
// product meter (selfcost) finds its own entries by it.
const pgSageTag = "/* pg_sage"

// meterOffenders charges a steady phase's product meter (gate H): the mean
// DB time per cycle of its known readings must match the DB time per cycle
// the gate measured, and each known reading's cycle length must match the
// collector period observed. A meter with no known reading, an unknown
// period or an invalid reading is an offender, never a skip: a meter that
// does not measure leaves sage_self_cost and the self-budget blind.
func meterOffenders(p Phase) []Offender {
	if p.Unmetered != "" {
		return nil
	}
	known, invalid := knownMeter(p.Meter)
	var out []Offender
	if invalid > 0 {
		out = append(out, Offender{Gate: GateMeterParity, Phase: p.Name,
			Subject: "invalid meter reading", Measured: float64(invalid), Budget: 0,
			Unit: "readings", Detail: fmt.Sprintf("%d of %d readings have a NaN, infinite "+
				"or negative DB time, or a cycle that is not positive", invalid, len(p.Meter))})
	}
	out = append(out, meterParity(p, known)...)
	return append(out, meterCycle(p, known)...)
}

// knownMeter is the readings with a known DB time and valid numbers, and
// how many known readings were invalid.
func knownMeter(costs []selfcost.Cost) ([]selfcost.Cost, int) {
	var known []selfcost.Cost
	invalid := 0
	for _, c := range costs {
		if !c.Known || !c.DBTimeKnown {
			continue
		}
		if !finiteNonNegative(c.DBTimeMsPerCycle) || !finiteNonNegative(c.CycleSeconds) ||
			c.CycleSeconds == 0 {
			invalid++
			continue
		}
		known = append(known, c)
	}
	return known, invalid
}

func finiteNonNegative(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}

func meterParity(p Phase, known []selfcost.Cost) []Offender {
	gate := GateMsPerCycle(p)
	o := Offender{Gate: GateMeterParity, Phase: p.Name, Subject: "meter unknown",
		Budget: gate, Unit: "ms per cycle"}
	switch {
	case len(known) == 0:
		o.Detail = fmt.Sprintf("no known DB-time reading among %d readings: the product "+
			"meter did not measure the phase (sage_self_cost and the self-budget stay "+
			"unknown)", len(p.Meter))
		return []Offender{o}
	case p.Cycles <= 0:
		o.Subject, o.Measured = "phase has no cycles", meterMean(known)
		o.Detail = "the gate's DB time per cycle cannot be computed"
		return []Offender{o}
	}
	mean := meterMean(known)
	tolerance := math.Max(gate*meterTolerancePct/100, meterFloorMs)
	if math.Abs(mean-gate) <= tolerance {
		return nil
	}
	o.Subject, o.Measured = "selfcost DB time per cycle", mean
	o.Detail = fmt.Sprintf("meter mean %.1f ms of %d known readings vs %.1f ms per cycle "+
		"measured; tolerance %.1f ms (%.0f%%, %.0f ms floor); untagged statements %.1f ms "+
		"per cycle", mean, len(known), gate, tolerance, meterTolerancePct, meterFloorMs,
		untaggedMsPerCycle(p))
	return []Offender{o}
}

func meterCycle(p Phase, known []selfcost.Cost) []Offender {
	o := Offender{Gate: GateMeterCycle, Phase: p.Name, Unit: "s per cycle"}
	if p.CollectorPeriod <= 0 {
		o.Subject = "collector period unknown"
		o.Detail = "fewer than two collector snapshots in the phase: the meter's cycle " +
			"length cannot be checked"
		return []Offender{o}
	}
	period := p.CollectorPeriod.Seconds()
	limit := period * meterCycleTolerancePct / 100
	off, worst, worstDev := 0, 0.0, 0.0
	for _, c := range known {
		dev := math.Abs(c.CycleSeconds - period)
		if dev <= limit {
			continue
		}
		off++
		if dev > worstDev {
			worst, worstDev = c.CycleSeconds, dev
		}
	}
	if off == 0 {
		return nil
	}
	o.Subject, o.Measured, o.Budget = "selfcost cycle length", worst, period
	o.Detail = fmt.Sprintf("%d of %d known readings normalize to a cycle more than %.0f%% "+
		"off the observed collector period %.1f s (worst %.1f s): the per-cycle self-cost "+
		"and the self-budget are scaled wrong", off, len(known), meterCycleTolerancePct,
		period, worst)
	return []Offender{o}
}

// GateMsPerCycle is the DB time per cycle the gate measured in a phase:
// every statement but the harness's and the workload's (0 without cycles).
func GateMsPerCycle(p Phase) float64 {
	if p.Cycles <= 0 {
		return 0
	}
	var total float64
	for _, s := range p.Statements {
		total += s.TotalMs
	}
	return total / float64(p.Cycles)
}

func meterMean(known []selfcost.Cost) float64 {
	var sum float64
	for _, c := range known {
		sum += c.DBTimeMsPerCycle
	}
	return sum / float64(len(known))
}

func untaggedMsPerCycle(p Phase) float64 {
	if p.Cycles <= 0 {
		return 0
	}
	var total float64
	for _, s := range UntaggedStatements(p.Statements, len(p.Statements)) {
		total += s.TotalMs
	}
	return total / float64(p.Cycles)
}

// UntaggedStatements is up to n of the statements without the pg_sage
// tag, costliest first: the gate charges them, the product meter cannot
// see them.
func UntaggedStatements(stmts []Statement, n int) []Statement {
	var out []Statement
	for _, s := range stmts {
		if !strings.Contains(s.Query, pgSageTag) {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	if len(out) > n {
		out = out[:max(n, 0)]
	}
	return out
}

// HoldsStatement reports whether a statement with sql's text is among
// stmts, white space and pg_sage tag comments aside: the sidecar's pool
// moves the tag after the first keyword on the wire, and PostgreSQL 18's
// pg_stat_statements drops a leading comment.
func HoldsStatement(stmts []Statement, sql string) bool {
	want := oneLine(sql)
	if want == "" {
		return false
	}
	for _, s := range stmts {
		if strings.Contains(oneLine(s.Query), want) {
			return true
		}
	}
	return false
}

// tagComments matches pg_sage's tag comments (/* pg_sage */ or one naming
// a component).
var tagComments = regexp.MustCompile(`/\*\s*pg_sage\b[^*]*\*/`)

func oneLine(q string) string {
	q = tagComments.ReplaceAllString(q, " ")
	return strings.TrimSpace(spaces.ReplaceAllString(q, " "))
}
