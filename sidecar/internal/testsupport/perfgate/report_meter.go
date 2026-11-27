package perfgate

import (
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/selfcost"
)

// meterUntaggedShown is how many untagged statements the meter section
// lists per phase.
const meterUntaggedShown = 5

// writeMeterBudgets is gate H's budget rows.
func writeMeterBudgets(sb *strings.Builder) {
	fmt.Fprintf(sb, "| %s | within %.0f%% of the gate's DB time per cycle (%.0f ms floor; "+
		"steady phase) |\n", GateMeterParity, meterTolerancePct, meterFloorMs)
	fmt.Fprintf(sb, "| %s | within %.0f%% of the observed collector period "+
		"(steady phase) |\n", GateMeterCycle, meterCycleTolerancePct)
}

// writeMeter is gate H's measurements: per steady phase the product
// meter against the gate, and the untagged statements the meter cannot
// see, costliest first.
func writeMeter(sb *strings.Builder, phases []Phase) {
	sb.WriteString("\n## Product meter (gate H)\n\n" +
		"| phase | meter ms per cycle | known readings | gate ms per cycle | " +
		"meter cycle s | observed period s |\n|---|---|---|---|---|---|\n")
	var notes []string
	for _, p := range phases {
		if !p.Steady {
			continue
		}
		if p.Unmetered != "" {
			notes = append(notes, fmt.Sprintf("%s: not charged (%s)", p.Name, p.Unmetered))
			continue
		}
		known, _ := knownMeter(p.Meter)
		meter, cycle := "unknown", "-"
		if len(known) > 0 {
			meter = fmt.Sprintf("%.1f", meterMean(known))
			cycle = fmt.Sprintf("%.1f", meanCycleSeconds(known))
		}
		period := "unknown"
		if p.CollectorPeriod > 0 {
			period = fmt.Sprintf("%.1f", p.CollectorPeriod.Seconds())
		}
		fmt.Fprintf(sb, "| %s | %s | %d of %d | %.1f | %s | %s |\n", p.Name, meter,
			len(known), len(p.Meter), GateMsPerCycle(p), cycle, period)
	}
	for _, n := range notes {
		sb.WriteString("\n" + n + "\n")
	}
	for _, p := range phases {
		if p.Steady && p.Unmetered == "" {
			writeUntagged(sb, p)
		}
	}
}

func writeUntagged(sb *strings.Builder, p Phase) {
	stmts := UntaggedStatements(p.Statements, meterUntaggedShown)
	fmt.Fprintf(sb, "\nTop %d untagged statements, %s (charged by the gate, not on the "+
		"meter): %d\n\n", meterUntaggedShown, p.Name, len(stmts))
	if len(stmts) == 0 {
		return
	}
	sb.WriteString("| queryid | calls | total ms | statement |\n|---|---|---|---|\n")
	for _, s := range stmts {
		fmt.Fprintf(sb, "| %d | %d | %.1f | %s |\n", s.QueryID, s.Calls, s.TotalMs,
			cell(shortQuery(s.Query)))
	}
}

func meanCycleSeconds(known []selfcost.Cost) float64 {
	var sum float64
	for _, c := range known {
		sum += c.CycleSeconds
	}
	return sum / float64(len(known))
}
